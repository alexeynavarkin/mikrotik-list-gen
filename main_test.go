package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseRIPE(t *testing.T) {
	input := strings.Join([]string{
		"2|ripencc|20260530|1234|19830705|20260530|+0200",               // version line
		"ripencc|*|ipv4|*|1000|summary",                                 // summary, too few fields anyway
		"ripencc|RU|ipv4|2.56.88.0|1024|20180101|allocated|abc|e-stats", // power-of-two -> /22
		"ripencc|RU|ipv4|5.8.8.0|2048|20180101|assigned",                // /21
		"ripencc|RU|ipv4|10.0.0.0|1536|20180101|allocated",              // 1024+512 -> two CIDRs
		"ripencc|DE|ipv4|9.9.9.0|256|20180101|allocated",                // wrong country
		"ripencc|RU|ipv4|6.6.6.0|256|20180101|reserved",                 // wrong status
		"ripencc|RU|ipv6|2a00:1118::|29|20180101|allocated",             // ipv6 prefix len
		"ripencc|RU|ipv6|2a01:230::|32|20180101|assigned",               // ipv6
		"ripencc|RU|ipv6|bogus|32|20180101|allocated",                   // invalid ip
		"garbage line without pipes",
	}, "\n")

	v4, v6, err := parseRIPE(strings.NewReader(input), "ru")
	if err != nil {
		t.Fatalf("parseRIPE: %v", err)
	}

	wantV4 := []string{
		"2.56.88.0/22",
		"5.8.8.0/21",
		"10.0.0.0/22", // 1024
		"10.0.4.0/23", // 512
	}
	if !equalSlice(v4, wantV4) {
		t.Errorf("v4 = %v, want %v", v4, wantV4)
	}

	wantV6 := []string{
		"2a00:1118::/29",
		"2a01:230::/32",
	}
	if !equalSlice(v6, wantV6) {
		t.Errorf("v6 = %v, want %v", v6, wantV6)
	}
}

func TestIPv4RangeToCIDRs(t *testing.T) {
	cases := []struct {
		start string
		count uint64
		want  []string
	}{
		{"2.56.88.0", 1024, []string{"2.56.88.0/22"}},
		{"0.0.0.0", 256, []string{"0.0.0.0/24"}},
		{"192.168.1.0", 1, []string{"192.168.1.0/32"}},
		// Unaligned start forces a split.
		{"192.168.0.128", 256, []string{"192.168.0.128/25", "192.168.1.0/25"}},
	}
	for _, c := range cases {
		ip := mustIPv4(t, c.start)
		got := ipv4RangeToCIDRs(ip, c.count)
		if !equalSlice(got, c.want) {
			t.Errorf("ipv4RangeToCIDRs(%s, %d) = %v, want %v", c.start, c.count, got, c.want)
		}
	}
}

func TestParseZone(t *testing.T) {
	input := strings.Join([]string{
		"# comment",
		"2.56.88.0/22",
		"  5.8.8.0/21  ",
		"not-a-cidr",
		"2.56.88.5/22", // non-canonical -> normalized
		"",
	}, "\n")
	got, err := parseZone(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseZone: %v", err)
	}
	want := []string{"2.56.88.0/22", "5.8.8.0/21", "2.56.88.0/22"}
	if !equalSlice(got, want) {
		t.Errorf("parseZone = %v, want %v", got, want)
	}
}

func TestRenderRSC(t *testing.T) {
	ts := time.Date(2026, 5, 30, 14, 30, 0, 0, time.UTC)
	out := string(renderRSC("by", ts,
		section{v4Prefix, []string{"5.8.8.0/21"}},
		section{v6Prefix, []string{"2a00:1118::/29"}},
	))

	want := strings.Join([]string{
		"# auto-generated 2026-05-30T14:30:00Z, country=BY, entries=2",
		`/ip firewall address-list remove [find list=BY comment="by-auto"]`,
		`/ip firewall address-list add list=BY address=5.8.8.0/21 comment="by-auto"`,
		`/ipv6 firewall address-list remove [find list=BY comment="by-auto"]`,
		`/ipv6 firewall address-list add list=BY address=2a00:1118::/29 comment="by-auto"`,
		"",
	}, "\n")

	if out != want {
		t.Errorf("renderRSC mismatch:\n got:\n%s\nwant:\n%s", out, want)
	}
}

func TestRenderRSCSkipsEmptySection(t *testing.T) {
	ts := time.Date(2026, 5, 30, 14, 30, 0, 0, time.UTC)
	out := string(renderRSC("ru", ts,
		section{v4Prefix, []string{"2.56.88.0/22"}},
		section{v6Prefix, nil},
	))
	if strings.Contains(out, "/ipv6") {
		t.Errorf("expected no IPv6 section, got:\n%s", out)
	}
	if !strings.Contains(out, "entries=1") {
		t.Errorf("expected entries=1, got:\n%s", out)
	}
}

func TestParseCountries(t *testing.T) {
	got, err := parseCountries(" RU, by ,kz, ru,")
	if err != nil {
		t.Fatalf("parseCountries: %v", err)
	}
	want := []string{"ru", "by", "kz"} // lowercased, de-duped, order preserved
	if !equalSlice(got, want) {
		t.Errorf("parseCountries = %v, want %v", got, want)
	}

	for _, bad := range []string{"", ",", "rus", "r", "r1", "12"} {
		if _, err := parseCountries(bad); err == nil {
			t.Errorf("parseCountries(%q) expected error", bad)
		}
	}
}

func TestValidCC(t *testing.T) {
	for _, ok := range []string{"ru", "by", "us", "cn"} {
		if !validCC(ok) {
			t.Errorf("validCC(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "r", "rus", "RU", "r1", "..", "/r"} {
		if validCC(bad) {
			t.Errorf("validCC(%q) = true, want false", bad)
		}
	}
}

// --- helpers ---

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustIPv4(t *testing.T, s string) uint32 {
	t.Helper()
	ip := net4(s)
	if ip == nil {
		t.Fatalf("bad test ip %q", s)
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func net4(s string) []byte {
	ip := parseIP4(s)
	return ip
}

func parseIP4(s string) []byte {
	var b [4]byte
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return nil
	}
	for i, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return nil
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return nil
		}
		b[i] = byte(n)
	}
	return b[:]
}
