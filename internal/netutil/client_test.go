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

// Security review A4, option (a): with REDROP_TRUSTED_HOPS=2 the client is
// the X-Forwarded-For entry exactly two from the right, for the production
// chain client -> Cloudflare (appends the connecting address) -> Traefik
// (trusts Cloudflare, keeps the header, appends the edge address) -> redrop.
func TestClientIPFromTrustedHops(t *testing.T) {
	const (
		traefik = "10.0.0.2:4000"
		edge    = "172.70.1.1"          // Cloudflare edge, appended by Traefik
		worker  = "2a06:98c0:3600::103" // Cloudflare's Worker egress address
	)
	cases := []struct {
		name, peer string
		xff        []string // header lines, in order
		want       string
	}{
		{"visitor through Cloudflare", traefik, []string{"198.51.100.7, " + edge}, "198.51.100.7"},
		{"IPv6 visitor through Cloudflare", traefik, []string{"2001:db8::5, " + edge}, "2001:db8::5"},
		{"visitor who sent their own header", traefik, []string{"6.6.6.6, 198.51.100.7, " + edge}, "198.51.100.7"},
		{"Worker with a forged value", traefik, []string{"203.0.113.99, " + worker + ", " + edge}, worker},
		{"Worker with another forged value", traefik, []string{"192.0.2.1, " + worker + ", " + edge}, worker},
		{"Worker forging several entries", traefik, []string{"198.51.100.7, 203.0.113.5, " + worker + ", " + edge}, worker},
		{"Worker forging a trusted-looking entry", traefik, []string{"10.0.0.2, " + worker + ", " + edge}, worker},
		// Traefik does not trust a direct client: it deletes the header and
		// sets it to the peer address, so there is only one entry.
		{"direct hit on the proxy", traefik, []string{"192.0.2.50"}, "192.0.2.50"},
		{"no header at all", traefik, nil, "10.0.0.2"},
		{"empty header", traefik, []string{""}, "10.0.0.2"},
		{"garbage at the client position", traefik, []string{"not-an-ip, " + edge}, "10.0.0.2"},
		{"garbage as the only entry", traefik, []string{"unknown"}, "10.0.0.2"},
		{"address with a port", traefik, []string{"198.51.100.7:443, " + edge}, "10.0.0.2"},
		{"untrusted peer", "203.0.113.9:5000", []string{"198.51.100.7, " + edge}, "203.0.113.9"},
		{"header split over two lines", traefik, []string{"6.6.6.6, 198.51.100.7", edge}, "198.51.100.7"},
		{"spaces around entries", traefik, []string{" 198.51.100.7 ,  " + edge + " "}, "198.51.100.7"},
	}
	c := resolver()
	c.TrustedHops = 2
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.peer
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := c.IP(r); got != tc.want {
			t.Errorf("%s: IP = %s, want %s", tc.name, got, tc.want)
		}
	}

	// One hop (a reverse proxy alone): the rightmost entry.
	c.TrustedHops = 1
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = traefik
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.8")
	if got := c.IP(r); got != "198.51.100.8" {
		t.Errorf("one hop: IP = %s", got)
	}
}

// Without a hop count, the walk skips every trusted entry: trusting a whole
// CDN range lets a Worker choose the address. Pinned here as the reason
// REDROP_TRUSTED_HOPS exists.
func TestTrustedWalkIsSpoofableWithCDNRanges(t *testing.T) {
	var nets []*net.IPNet
	for _, cidr := range []string{"10.0.0.2/32", "172.64.0.0/13", "2a06:98c0::/29"} {
		_, n, _ := net.ParseCIDR(cidr)
		nets = append(nets, n)
	}
	c := &ClientResolver{TrustedProxies: nets, IPHeader: "X-Forwarded-For"}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.2:4000"
	r.Header.Set("X-Forwarded-For", "203.0.113.99, 2a06:98c0:3600::103, 172.70.1.1")
	if got := c.IP(r); got != "203.0.113.99" {
		t.Fatalf("walk = %s", got)
	}
	c.TrustedHops = 2
	if got := c.IP(r); got != "2a06:98c0:3600::103" {
		t.Fatalf("two hops = %s", got)
	}
}
