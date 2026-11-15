package netutil

import (
	"net"
	"net/http/httptest"
	"testing"
)

func resolver() *ClientResolver {
	_, traefik, _ := net.ParseCIDR("10.0.0.2/32")
	return &ClientResolver{TrustedProxies: []*net.IPNet{traefik}, IPHeader: "X-Forwarded-For", CountryHeader: "CF-IPCountry"}
}

func TestClientIPIgnoresHeadersFromUntrustedPeers(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:4000"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("CF-IPCountry", "UA")
	c := resolver()
	if got := c.IP(r); got != "203.0.113.9" {
		t.Errorf("spoofed IP accepted: %s", got)
	}
	if got := c.Country(r); got != "" {
		t.Errorf("spoofed country accepted: %s", got)
	}
}

func TestClientIPFromTrustedProxy(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.2:4000"
	// A client-supplied value precedes the address the proxy appended.
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	r.Header.Set("CF-IPCountry", "pl")
	c := resolver()
	if got := c.IP(r); got != "198.51.100.7" {
		t.Errorf("IP = %s, want 198.51.100.7", got)
	}
	if got := c.Country(r); got != "PL" {
		t.Errorf("country = %s", got)
	}
}

// Every per-client limiter keys on Network, so a host cannot dodge them by
// rotating through the addresses of its IPv6 /64.
func TestNetworkGroupsIPv6By64(t *testing.T) {
	cases := map[string]string{
		"198.51.100.7":                "198.51.100.7",
		"::ffff:198.51.100.7":         "198.51.100.7",
		"2001:db8:77::1":              "2001:db8:77::/64",
		"2001:db8:77::ffff:1":         "2001:db8:77::/64",
		"2001:db8:77:0:1234:5678::1":  "2001:db8:77::/64",
		"2001:db8:77:1::1":            "2001:db8:77:1::/64",
		"2001:0DB8:0077:0000:0000::9": "2001:db8:77::/64",
		"not-an-ip":                   "not-an-ip",
	}
	for in, want := range cases {
		if got := Network(in); got != want {
			t.Errorf("Network(%q) = %q, want %q", in, got, want)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.2:4000"
	r.Header.Set("X-Forwarded-For", "2001:db8:77::abcd")
	if got := resolver().Network(r); got != "2001:db8:77::/64" {
		t.Errorf("resolver Network = %q", got)
	}
}
