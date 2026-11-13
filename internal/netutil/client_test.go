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
