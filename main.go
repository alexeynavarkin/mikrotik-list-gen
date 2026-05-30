// Command geoip-list-server fetches per-country IP subnet lists and serves them
// as RouterOS .rsc scripts for import into MikroTik address-lists.
//
// Which countries are served is controlled by the -countries flag. See
// README.md for the rationale and deployment notes.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/bits"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Data sources. %s is the lowercase ISO 3166-1 alpha-2 country code.
const (
	ipdenyV4Tmpl = "https://www.ipdeny.com/ipblocks/data/countries/%s.zone"
	ipdenyV6Tmpl = "https://www.ipdeny.com/ipv6/ipaddresses/blocks/%s.zone"
	ripeURL      = "https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest"

	userAgent   = "geoip-list-server/2.0 (+https://github.com/alexeynavarkin/mikrotik-list-gen)"
	httpTimeout = 60 * time.Second
)

// RouterOS address-list command prefixes.
const (
	v4Prefix = "/ip firewall address-list"
	v6Prefix = "/ipv6 firewall address-list"
)

// listName / autoMarker derive the RouterOS list name and comment marker from a
// country code, so importing several countries never clobbers each other.
func listName(cc string) string   { return strings.ToUpper(cc) }
func autoMarker(cc string) string { return cc + "-auto" }

// listState is an immutable snapshot of one country's successfully built list.
// It is swapped atomically under the server mutex; readers never mutate it.
type listState struct {
	cc      string
	rscAll  []byte // combined IPv4 + IPv6
	rscV4   []byte // IPv4 only
	rscV6   []byte // IPv6 only
	countV4 int
	countV6 int
	source  string // "ipdeny", "ripe" or a mix like "ipdeny+ripe(v6)"
	updated time.Time
}

type server struct {
	client    *http.Client
	countries []string        // configured, in order
	served    map[string]bool // configured set, for O(1) lookup

	mu     sync.RWMutex
	states map[string]*listState // cc -> latest successful snapshot
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	refresh := flag.Duration("refresh", 24*time.Hour, "background refresh interval")
	ccList := flag.String("countries", "ru", "comma-separated ISO 3166-1 alpha-2 country codes to serve")
	healthcheck := flag.Bool("healthcheck", false, "probe local /healthz and exit (for container HEALTHCHECK); uses -addr")
	flag.Parse()

	// Self-probe mode for distroless containers (no shell/curl available).
	if *healthcheck {
		os.Exit(runHealthcheck(*addr))
	}

	countries, err := parseCountries(*ccList)
	if err != nil {
		log.Fatalf("invalid -countries: %v", err)
	}

	srv := &server{
		client:    &http.Client{Timeout: httpTimeout},
		countries: countries,
		served:    make(map[string]bool, len(countries)),
		states:    make(map[string]*listState),
	}
	for _, cc := range countries {
		srv.served[cc] = true
	}

	// Synchronous initial load. A service with no data at all is useless, so
	// fail hard only if *nothing* loaded; a single flaky country just warns.
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	initial := srv.buildAll(ctx)
	cancel()
	if len(initial) == 0 {
		log.Fatalf("no country lists could be loaded for %v", countries)
	}
	srv.merge(initial)
	for _, cc := range countries {
		if st := initial[cc]; st != nil {
			log.Printf("[%s] loaded %d IPv4 + %d IPv6 entries from %s", cc, st.countV4, st.countV6, st.source)
		}
	}

	go srv.refreshLoop(*refresh)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", srv.handleHealth)
	mux.HandleFunc("GET /{name}", srv.handleList)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("listening on %s, serving: %s", *addr, strings.Join(countries, ", "))
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("http server: %v", err)
	}
}

func (s *server) refreshLoop(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
		built := s.buildAll(ctx)
		cancel()
		if len(built) == 0 {
			log.Printf("refresh produced nothing, keeping previous lists")
			continue
		}
		s.merge(built)
		for cc, st := range built {
			log.Printf("[%s] refreshed: %d IPv4 + %d IPv6 entries from %s", cc, st.countV4, st.countV6, st.source)
		}
	}
}

// buildAll builds every configured country concurrently and returns only the
// ones that succeeded.
func (s *server) buildAll(ctx context.Context) map[string]*listState {
	type result struct {
		cc string
		st *listState
	}
	ch := make(chan result, len(s.countries))
	var wg sync.WaitGroup
	for _, cc := range s.countries {
		wg.Add(1)
		go func(cc string) {
			defer wg.Done()
			st, err := s.build(ctx, cc)
			if err != nil {
				log.Printf("[%s] build failed: %v", cc, err)
				ch <- result{cc, nil}
				return
			}
			ch <- result{cc, st}
		}(cc)
	}
	wg.Wait()
	close(ch)

	out := make(map[string]*listState)
	for r := range ch {
		if r.st != nil {
			out[r.cc] = r.st
		}
	}
	return out
}

// merge overwrites snapshots for the countries that rebuilt successfully,
// leaving the previous snapshot in place for any that failed.
func (s *server) merge(built map[string]*listState) {
	s.mu.Lock()
	for cc, st := range built {
		s.states[cc] = st
	}
	s.mu.Unlock()
}

func (s *server) getState(cc string) *listState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.states[cc]
}

// build fetches and renders a fresh list for one country. IPv4 is mandatory
// (primary source with a RIPE fallback); IPv6 is best-effort.
func (s *server) build(ctx context.Context, cc string) (*listState, error) {
	var (
		v4, v6     []string
		source     string
		ripeV4     []string
		ripeV6     []string
		ripeLoaded bool
	)

	loadRIPE := func() error {
		if ripeLoaded {
			return nil
		}
		var err error
		ripeV4, ripeV6, err = s.fetchRIPE(ctx, cc)
		if err != nil {
			return err
		}
		ripeLoaded = true
		return nil
	}

	// --- IPv4 (mandatory) ---
	if list, err := s.fetchZone(ctx, fmt.Sprintf(ipdenyV4Tmpl, cc)); err == nil && len(list) > 0 {
		v4 = list
		source = "ipdeny"
	} else {
		log.Printf("[%s] ipdeny IPv4 failed (%v), falling back to RIPE", cc, err)
		if rerr := loadRIPE(); rerr != nil {
			return nil, fmt.Errorf("no IPv4 source available: ipdeny: %v; ripe: %w", err, rerr)
		}
		v4 = ripeV4
		source = "ripe"
	}
	if len(v4) == 0 {
		return nil, fmt.Errorf("IPv4 list is empty (unknown country code %q?)", cc)
	}

	// --- IPv6 (best effort) ---
	if list, err := s.fetchZone(ctx, fmt.Sprintf(ipdenyV6Tmpl, cc)); err == nil && len(list) > 0 {
		v6 = list
	} else {
		log.Printf("[%s] ipdeny IPv6 failed (%v), trying RIPE for IPv6", cc, err)
		if rerr := loadRIPE(); rerr != nil {
			log.Printf("[%s] RIPE IPv6 also unavailable (%v), continuing IPv4-only", cc, rerr)
		} else {
			v6 = ripeV6
			if source == "ipdeny" && len(v6) > 0 {
				source = "ipdeny+ripe(v6)"
			}
		}
	}

	now := time.Now().UTC()
	st := &listState{
		cc:      cc,
		rscAll:  renderRSC(cc, now, section{v4Prefix, v4}, section{v6Prefix, v6}),
		rscV4:   renderRSC(cc, now, section{v4Prefix, v4}),
		rscV6:   renderRSC(cc, now, section{v6Prefix, v6}),
		countV4: len(v4),
		countV6: len(v6),
		source:  source,
		updated: now,
	}
	return st, nil
}

// --- HTTP fetching ---

func (s *server) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: status %s", url, resp.Status)
	}
	return resp.Body, nil
}

// fetchZone reads a plain-text zone file (one CIDR per line) and returns the
// validated, canonicalized CIDRs. Invalid lines are dropped.
func (s *server) fetchZone(ctx context.Context, url string) ([]string, error) {
	body, err := s.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return parseZone(body)
}

func parseZone(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if c, ok := canonCIDR(line); ok {
			out = append(out, c)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// fetchRIPE downloads and parses the RIPE NCC delegated statistics file for one
// country. Note: RIPE NCC only covers Europe, the Middle East and Central Asia;
// for other regions this fallback returns nothing and only ipdeny is used.
func (s *server) fetchRIPE(ctx context.Context, cc string) (v4, v6 []string, err error) {
	body, err := s.get(ctx, ripeURL)
	if err != nil {
		return nil, nil, err
	}
	defer body.Close()
	return parseRIPE(body, cc)
}

// parseRIPE parses the delegated-ripencc file format:
//
//	registry|cc|type|start|value|date|status[|...]
//
// For ipv4 records, value is the address count (converted to a CIDR mask);
// for ipv6 records, value is the prefix length. Only records for the requested
// country with status allocated/assigned are kept.
func parseRIPE(r io.Reader, cc string) (v4, v6 []string, err error) {
	want := strings.ToUpper(cc)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "|")
		if len(f) < 7 {
			continue
		}
		// Skip version and summary header lines (status is "summary" or empty).
		if f[1] != want {
			continue
		}
		status := f[6]
		if status != "allocated" && status != "assigned" {
			continue
		}
		start, value := f[3], f[4]
		switch f[2] {
		case "ipv4":
			n, perr := strconv.ParseUint(value, 10, 64)
			if perr != nil || n == 0 {
				continue
			}
			ip := net.ParseIP(start).To4()
			if ip == nil {
				continue
			}
			for _, cidr := range ipv4RangeToCIDRs(binary.BigEndian.Uint32(ip), n) {
				if c, ok := canonCIDR(cidr); ok {
					v4 = append(v4, c)
				}
			}
		case "ipv6":
			prefix, perr := strconv.Atoi(value)
			if perr != nil || prefix < 0 || prefix > 128 {
				continue
			}
			if c, ok := canonCIDR(start + "/" + value); ok {
				v6 = append(v6, c)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	return v4, v6, nil
}

// ipv4RangeToCIDRs decomposes the range [start, start+count) into the minimal
// set of aligned CIDR blocks. RIPE address counts are usually, but not always,
// powers of two, so a single "32 - log2(n)" mask is not sufficient.
func ipv4RangeToCIDRs(start uint32, count uint64) []string {
	var out []string
	cur := uint64(start)
	end := cur + count // exclusive
	for cur < end {
		// Largest block the current address is aligned to.
		maxByAlign := uint32(32)
		if cur != 0 {
			maxByAlign = uint32(bits.TrailingZeros32(uint32(cur)))
		}
		// Largest block that fits in the remaining range.
		remaining := end - cur
		var sizeLog uint32
		for sizeLog+1 <= maxByAlign && (uint64(1)<<(sizeLog+1)) <= remaining {
			sizeLog++
		}
		prefix := 32 - sizeLog
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, uint32(cur))
		out = append(out, fmt.Sprintf("%s/%d", ip.String(), prefix))
		cur += uint64(1) << sizeLog
	}
	return out
}

// canonCIDR validates a CIDR and returns its canonical network form
// (e.g. "2.56.88.5/22" -> "2.56.88.0/22").
func canonCIDR(s string) (string, bool) {
	_, ipnet, err := net.ParseCIDR(strings.TrimSpace(s))
	if err != nil {
		return "", false
	}
	return ipnet.String(), true
}

// --- RouterOS rendering ---

type section struct {
	prefix  string
	entries []string
}

// renderRSC builds a RouterOS script that first removes the previously
// auto-generated entries for this country (matched by comment) and then re-adds
// the current set. The list name is the uppercase country code; the comment
// marker is "<cc>-auto". Empty sections are skipped entirely.
func renderRSC(cc string, updated time.Time, sections ...section) []byte {
	list := listName(cc)
	marker := autoMarker(cc)

	total := 0
	for _, s := range sections {
		total += len(s.entries)
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "# auto-generated %s, country=%s, entries=%d\n",
		updated.UTC().Format(time.RFC3339), strings.ToUpper(cc), total)

	for _, s := range sections {
		if len(s.entries) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s remove [find list=%s comment=%q]\n", s.prefix, list, marker)
		for _, cidr := range s.entries {
			fmt.Fprintf(&b, "%s add list=%s address=%s comment=%q\n",
				s.prefix, list, cidr, marker)
		}
	}
	return b.Bytes()
}

// --- HTTP handlers ---

// handleList serves "/{cc}.rsc", "/{cc}-v4.rsc" and "/{cc}-v6.rsc".
func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(r.PathValue("name"))
	if !strings.HasSuffix(name, ".rsc") {
		http.NotFound(w, r)
		return
	}
	base := strings.TrimSuffix(name, ".rsc")

	family := "all"
	switch {
	case strings.HasSuffix(base, "-v4"):
		family, base = "v4", strings.TrimSuffix(base, "-v4")
	case strings.HasSuffix(base, "-v6"):
		family, base = "v6", strings.TrimSuffix(base, "-v6")
	}

	cc := base
	if !validCC(cc) {
		http.NotFound(w, r)
		return
	}
	if !s.served[cc] {
		http.Error(w, fmt.Sprintf("country %q is not served by this instance", cc), http.StatusNotFound)
		return
	}
	st := s.getState(cc)
	if st == nil {
		http.Error(w, "list not ready", http.StatusServiceUnavailable)
		return
	}

	var body []byte
	switch family {
	case "v4":
		body = st.rscV4
	case "v6":
		body = st.rscV6
	default:
		body = st.rscAll
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Last-Modified", st.updated.Format(http.TimeFormat))
	w.Write(body)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	type ccHealth struct {
		Source    string `json:"source"`
		EntriesV4 int    `json:"entries_v4"`
		EntriesV6 int    `json:"entries_v6"`
		UpdatedAt string `json:"updated_at"`
		AgeSecond int    `json:"age_seconds"`
	}

	countries := make(map[string]any, len(s.countries))
	ready := 0
	for _, cc := range s.countries {
		st := s.getState(cc)
		if st == nil {
			countries[cc] = map[string]string{"status": "pending"}
			continue
		}
		ready++
		countries[cc] = ccHealth{
			Source:    st.source,
			EntriesV4: st.countV4,
			EntriesV6: st.countV6,
			UpdatedAt: st.updated.Format(time.RFC3339),
			AgeSecond: int(time.Since(st.updated).Seconds()),
		}
	}

	status := "ok"
	code := http.StatusOK
	if ready == 0 {
		status, code = "starting", http.StatusServiceUnavailable
	} else if ready < len(s.countries) {
		status = "degraded"
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"status":    status,
		"countries": countries,
	})
}

// --- helpers ---

// parseCountries splits, normalizes, validates and de-duplicates a
// comma-separated list of country codes, preserving order.
func parseCountries(s string) ([]string, error) {
	seen := make(map[string]bool)
	var out []string
	for _, raw := range strings.Split(s, ",") {
		cc := strings.ToLower(strings.TrimSpace(raw))
		if cc == "" {
			continue
		}
		if !validCC(cc) {
			return nil, fmt.Errorf("%q is not a 2-letter country code", raw)
		}
		if !seen[cc] {
			seen[cc] = true
			out = append(out, cc)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no country codes given")
	}
	return out, nil
}

// validCC reports whether s is a two-letter lowercase country code. It also
// guards the source URLs against path-injection from request paths.
func validCC(s string) bool {
	if len(s) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		if c := s[i]; c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// runHealthcheck probes the local /healthz endpoint and returns a process exit
// code (0 = healthy). It derives the target from the listen address, mapping a
// wildcard/empty host to 127.0.0.1. Used as the container HEALTHCHECK command.
func runHealthcheck(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = "", addr // tolerate a bare ":8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.Status)
		return 1
	}
	return 0
}
