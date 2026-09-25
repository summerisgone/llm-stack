package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Addresses fetch_url must never connect to (ADR 0017 section 2). The pod's
// egress NetworkPolicy denies the same ranges as a second layer.
var defaultDeny = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "fc00::/7", "fe80::/10", "ff00::/8",
}

var errBlocked = errors.New("destination not allowed")

type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type guard struct {
	deny     []netip.Prefix
	resolver resolver
	// anyPort is for tests only: httptest listens on random ports.
	anyPort bool
}

func newGuard(extra string) (*guard, error) {
	g := &guard{resolver: net.DefaultResolver}
	for _, s := range append(defaultDeny, strings.FieldsFunc(extra, func(r rune) bool { return r == ',' || r == ' ' })...) {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("deny CIDR %q: %w", s, err)
		}
		g.deny = append(g.deny, p)
	}
	return g, nil
}

func (g *guard) allowedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range g.deny {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func (g *guard) checkURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q", errBlocked, u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%w: no host", errBlocked)
	}
	if u.User != nil {
		return fmt.Errorf("%w: credentials in URL", errBlocked)
	}
	if p := u.Port(); p != "" && !g.anyPort && p != "80" && p != "443" {
		return fmt.Errorf("%w: port %s", errBlocked, p)
	}
	return nil
}

// dialContext resolves the host once, refuses if any answer is a denied
// address, and connects to the checked address literal, so a second
// resolution (DNS rebinding) cannot change the destination.
func (g *guard) dialContext(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if !g.anyPort && port != "80" && port != "443" {
			return nil, fmt.Errorf("%w: port %s", errBlocked, port)
		}
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else if ips, err = g.resolver.LookupNetIP(ctx, "ip", host); err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no addresses for %s", host)
		}
		for _, ip := range ips {
			if !g.allowedIP(ip) {
				return nil, fmt.Errorf("%w: %s resolves to %s", errBlocked, host, ip)
			}
		}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
	}
}

func (g *guard) client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           g.dialContext(&net.Dialer{Timeout: 10 * time.Second}),
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return g.checkURL(req.URL)
		},
	}
}
