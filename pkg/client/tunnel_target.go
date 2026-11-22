package client

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// AddrClass is the kind of network an address belongs to. A tunnel's target
// must be in one class (DESIGN.md "Dial safety").
type AddrClass string

const (
	ClassLoopback AddrClass = "loopback"
	ClassPrivate  AddrClass = "private network"
	ClassPublic   AddrClass = "public internet"
)

// TunnelTarget is where a tunnel's requests go: resolved once, then pinned.
// Requests are dialed to Addrs in order, never to a name from a request.
type TunnelTarget struct {
	// Host is the Host header the app receives: the name and port the owner
	// typed, such as localhost:5173.
	Host  string
	Addrs []netip.AddrPort
	Class AddrClass
}

func (t *TunnelTarget) String() string { return "http://" + t.Host }

// Resolver looks names up (net.DefaultResolver; tests replace it).
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// ResolveTunnelTarget parses a target (3000, host:3000, [::1]:3000,
// http://host:3000) and resolves it once. A bare port, localhost and
// *.localhost mean 127.0.0.1 then ::1, without DNS. Every address must be in
// one class; anything but loopback needs allowRemote. Errors are
// *UsageError.
func ResolveTunnelTarget(ctx context.Context, raw string, allowRemote bool, r Resolver) (*TunnelTarget, error) {
	if r == nil {
		r = net.DefaultResolver
	}
	host, port, err := splitTarget(strings.TrimSpace(raw))
	if err != nil {
		return nil, &UsageError{Message: err.Error(), Cause: err}
	}
	// The Host header has no IPv6 zone; the dialed addresses keep it.
	hostOnly, _, _ := strings.Cut(host, "%")
	t := &TunnelTarget{Host: net.JoinHostPort(hostOnly, strconv.Itoa(int(port)))}
	var addrs []netip.Addr
	switch lower := strings.ToLower(strings.TrimSuffix(host, ".")); {
	case lower == "localhost" || strings.HasSuffix(lower, ".localhost"):
		addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()}
	default:
		if a, perr := netip.ParseAddr(host); perr == nil {
			addrs = []netip.Addr{a}
		} else {
			if addrs, err = r.LookupNetIP(ctx, "ip", host); err != nil {
				return nil, usageErrorf("cannot resolve the tunnel target %s: %v", host, err)
			}
			if len(addrs) == 0 {
				return nil, usageErrorf("the tunnel target %s has no addresses", host)
			}
		}
	}
	for i, a := range addrs {
		a = a.Unmap() // a link-local address keeps its zone, or it can't be dialed
		c, err := classOf(a)
		if err != nil {
			return nil, usageErrorf("the tunnel target %s: %v", t.Host, err)
		}
		if i > 0 && c != t.Class {
			return nil, usageErrorf("the tunnel target %s resolves to more than one kind of network (%s and %s); name an address", host, t.Class, c)
		}
		t.Class = c
		t.Addrs = append(t.Addrs, netip.AddrPortFrom(a, port))
	}
	if t.Class != ClassLoopback && !allowRemote {
		return nil, usageErrorf("the tunnel target %s is on the %s, not this computer; pass --allow-remote-target to tunnel to it", t.Host, t.Class)
	}
	return t, nil
}

// splitTarget returns the host and port of a target ("" host means
// localhost).
func splitTarget(raw string) (string, uint16, error) {
	if raw == "" {
		return "", 0, fmt.Errorf("missing tunnel target: give a port (3000) or host:port")
	}
	if p, err := parsePort(raw); err == nil {
		return "localhost", p, nil
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", 0, fmt.Errorf("invalid tunnel target %q", raw)
		}
		switch {
		case u.Scheme == "https":
			return "", 0, fmt.Errorf("HTTPS targets are not supported; tunnel to the app's plain HTTP port")
		case u.Scheme != "http":
			return "", 0, fmt.Errorf("tunnel targets are http://host:port (got %q)", raw)
		case u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
			return "", 0, fmt.Errorf("a tunnel target is a host and port, without a path or query (got %q)", raw)
		}
		host, port := u.Hostname(), u.Port()
		if port == "" {
			port = "80"
		}
		p, err := parsePort(port)
		if host == "" || err != nil {
			return "", 0, fmt.Errorf("invalid tunnel target %q", raw)
		}
		return host, p, nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return "", 0, fmt.Errorf("invalid tunnel target %q: give a port (3000) or host:port", raw)
	}
	p, err := parsePort(port)
	if host == "" || err != nil {
		return "", 0, fmt.Errorf("invalid tunnel target %q: give a port (3000) or host:port", raw)
	}
	return host, p, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != s {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return uint16(n), nil
}

var (
	cgnat     = netip.MustParsePrefix("100.64.0.0/10")
	broadcast = netip.MustParseAddr("255.255.255.255")
)

// classOf returns an address's class, refusing addresses a tunnel never
// dials: unspecified, multicast and broadcast.
func classOf(a netip.Addr) (AddrClass, error) {
	switch {
	case !a.IsValid() || a.IsUnspecified():
		return "", fmt.Errorf("%s is not a single host; use 127.0.0.1 for this computer", a)
	case a.IsMulticast() || a == broadcast:
		return "", fmt.Errorf("%s is a multicast or broadcast address", a)
	case a.IsLoopback():
		return ClassLoopback, nil
	case a.IsPrivate() || a.IsLinkLocalUnicast() || cgnat.Contains(a):
		return ClassPrivate, nil
	}
	return ClassPublic, nil
}
