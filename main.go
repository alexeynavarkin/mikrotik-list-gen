// Command ru-list-server fetches the list of Russian IP subnets and serves it
// as a RouterOS .rsc script for import into a MikroTik address-list.
//
// See README.md for the rationale and deployment notes.
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

// Data sources.
const (
	ipdenyV4URL = "https://www.ipdeny.com/ipblocks/data/countries/ru.zone"
	ipdenyV6URL = "https://www.ipdeny.com/ipv6/ipaddresses/blocks/ru.zone"
	ripeURL     = "https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest"

	userAgent   = "ru-list-server/1.0 (+https://github.com/)"
	httpTimeout = 60 * time.Second
)

// RouterOS rendering constants.
const (
	listName   = "RU"
	autoMarker = "ru-auto"
	v4Prefix   = "/ip firewall address-list"
	v6Prefix   = "/ipv6 firewall address-list"
)

// listState is an immutable snapshot of a successfully built list. It is
// swapped atomically under the server mutex; readers never mutate it.
type listState struct {
	rscAll  []byte // combined IPv4 + IPv6
	rscV4   []byte // IPv4 only
	rscV6   []byte // IPv6 only
	countV4 int
	countV6 int
	source  string // "ipdeny", "ripe" or a mix like "ipdeny+ripe(v6)"
	updated time.Time
}

type server struct {
	client *http.Client

	mu    sync.RWMutex
	state *listState // nil until the first successful load
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	refresh := flag.Duration("refresh", 24*time.Hour, "background refresh interval")
	healthcheck := flag.Bool("healthcheck", false, "probe local /healthz and exit (for container HEALTHCHECK); uses -addr")
	flag.Parse()

	// Self-probe mode for distroless containers (no shell/curl available).
	if *healthcheck {
		os.Exit(runHealthcheck(*addr))
	}

	srv := &server{
		client: &http.Client{Timeout: httpTimeout},
	}

	// Synchronous initial load — a service with no data is useless, so fail hard.
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	st, err := srv.build(ctx)
	cancel()
	if err != nil {
		log.Fatalf("initial list load failed: %v", err)
	}
	srv.swap(st)
	log.Printf("loaded %d IPv4 + %d IPv6 entries from %s", st.countV4, st.countV6, st.source)

	go srv.refreshLoop(*refresh)

	mux := http.NewServeMux()
	mux.HandleFunc("/ru-list.rsc", srv.handleList(func(s *listState) []byte { return s.rscAll }))
	mux.HandleFunc("/ru-list-v4.rsc", srv.handleList(func(s *listState) []byte { return s.rscV4 }))
	mux.HandleFunc("/ru-list-v6.rsc", srv.handleList(func(s *listState) []byte { return s.rscV6 }))
	mux.HandleFunc("/healthz", srv.handleHealth)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("listening on %s", *addr)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("http server: %v", err)
	}
}

func (s *server) refreshLoop(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
		st, err := s.build(ctx)
		cancel()
		if err != nil {
			// Keep the previous successful list rather than serving nothing.
			log.Printf("refresh failed, keeping previous list: %v", err)
			continue
		}
		s.swap(st)
		log.Printf("refreshed: %d IPv4 + %d IPv6 entries from %s", st.countV4, st.countV6, st.source)
	}
}

func (s *server) swap(st *listState) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

func (s *server) snapshot() *listState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// build fetches and renders a fresh list. IPv4 is mandatory (primary source
// with a RIPE fallback); IPv6 is best-effort.
func (s *server) build(ctx context.Context) (*listState, error) {
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
		ripeV4, ripeV6, err = s.fetchRIPE(ctx)
		if err != nil {
			return err
		}
		ripeLoaded = true
		return nil
	}

	// --- IPv4 (mandatory) ---
	if list, err := s.fetchZone(ctx, ipdenyV4URL); err == nil && len(list) > 0 {
		v4 = list
		source = "ipdeny"
	} else {
		log.Printf("ipdeny IPv4 failed (%v), falling back to RIPE", err)
		if rerr := loadRIPE(); rerr != nil {
			return nil, fmt.Errorf("no IPv4 source available: ipdeny: %v; ripe: %w", err, rerr)
		}
		v4 = ripeV4
		source = "ripe"
	}
	if len(v4) == 0 {
		return nil, fmt.Errorf("IPv4 list is empty")
	}

	// --- IPv6 (best effort) ---
	if list, err := s.fetchZone(ctx, ipdenyV6URL); err == nil && len(list) > 0 {
		v6 = list
	} else {
		log.Printf("ipdeny IPv6 failed (%v), trying RIPE for IPv6", err)
		if rerr := loadRIPE(); rerr != nil {
			log.Printf("RIPE IPv6 also unavailable (%v), continuing IPv4-only", rerr)
		} else {
			v6 = ripeV6
			if source == "ipdeny" && len(v6) > 0 {
				source = "ipdeny+ripe(v6)"
			}
		}
	}

	now := time.Now().UTC()
	st := &listState{
		rscAll:  renderRSC(now, section{v4Prefix, v4}, section{v6Prefix, v6}),
		rscV4:   renderRSC(now, section{v4Prefix, v4}),
		rscV6:   renderRSC(now, section{v6Prefix, v6}),
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

// fetchRIPE downloads and parses the RIPE NCC delegated statistics file.
func (s *server) fetchRIPE(ctx context.Context) (v4, v6 []string, err error) {
	body, err := s.get(ctx, ripeURL)
	if err != nil {
		return nil, nil, err
	}
	defer body.Close()
	return parseRIPE(body)
}

// parseRIPE parses the delegated-ripencc file format:
//
//	registry|cc|type|start|value|date|status[|...]
//
// For ipv4 records, value is the address count (converted to a CIDR mask);
// for ipv6 records, value is the prefix length. Only cc=RU records with
// status allocated/assigned are kept.
func parseRIPE(r io.Reader) (v4, v6 []string, err error) {
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
		if f[1] != "RU" {
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
// auto-generated entries (matched by comment) and then re-adds the current set.
// Empty sections are skipped entirely.
func renderRSC(updated time.Time, sections ...section) []byte {
	total := 0
	for _, s := range sections {
		total += len(s.entries)
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "# auto-generated %s, entries=%d\n",
		updated.UTC().Format(time.RFC3339), total)

	for _, s := range sections {
		if len(s.entries) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s remove [find list=%s comment=%q]\n", s.prefix, listName, autoMarker)
		for _, cidr := range s.entries {
			fmt.Fprintf(&b, "%s add list=%s address=%s comment=%q\n",
				s.prefix, listName, cidr, autoMarker)
		}
	}
	return b.Bytes()
}

// --- HTTP handlers ---

func (s *server) handleList(pick func(*listState) []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := s.snapshot()
		if st == nil {
			http.Error(w, "list not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Last-Modified", st.updated.Format(http.TimeFormat))
		w.Write(pick(st))
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := s.snapshot()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if st == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"status": "starting"})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"source":      st.source,
		"entries_v4":  st.countV4,
		"entries_v6":  st.countV6,
		"updated_at":  st.updated.Format(time.RFC3339),
		"age_seconds": int(time.Since(st.updated).Seconds()),
	})
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
