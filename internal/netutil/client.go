// Package netutil resolves client details behind trusted reverse proxies.
package netutil

import (
	"net"
	"net/http"
	"strings"
)

// ClientResolver derives the client IP and CDN country from a request,
// trusting forwarding headers only from configured proxies.
type ClientResolver struct {
	TrustedProxies []*net.IPNet
	IPHeader       string // e.g. X-Forwarded-For or CF-Connecting-IP
	CountryHeader  string // e.g. CF-IPCountry
}

func (c *ClientResolver) peer(r *http.Request) (string, net.IP) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host, net.ParseIP(host)
}

func (c *ClientResolver) trusted(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range c.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// IP returns the client address. Forwarded values are walked right to left,
// skipping trusted proxies, so clients cannot spoof their address.
func (c *ClientResolver) IP(r *http.Request) string {
	host, ip := c.peer(r)
	if !c.trusted(ip) || c.IPHeader == "" {
		return host
	}
	hdr := r.Header.Get(c.IPHeader)
	if hdr == "" {
		return host
	}
	parts := strings.Split(hdr, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		cip := net.ParseIP(strings.TrimSpace(parts[i]))
		if cip == nil {
			break
		}
		if !c.trusted(cip) || i == 0 {
			return cip.String()
		}
	}
	return host
}

// Country returns an ISO country code supplied by a trusted CDN, or "".
func (c *ClientResolver) Country(r *http.Request) string {
	if c.CountryHeader == "" {
		return ""
	}
	if _, ip := c.peer(r); !c.trusted(ip) {
		return ""
	}
	v := strings.ToUpper(strings.TrimSpace(r.Header.Get(c.CountryHeader)))
	if len(v) != 2 || v == "XX" || v == "T1" {
		return ""
	}
	return v
}
