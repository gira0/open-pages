package server

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// peerAddr is the address of the TCP peer, or false when RemoteAddr isn't an IP address.
func peerAddr(r *http.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

// trusted reports whether addr belongs to one of the configured proxy networks.
func (s *Server) trusted(addr netip.Addr) bool {
	return slices.ContainsFunc(s.cfg.TrustedProxies, func(p netip.Prefix) bool { return p.Contains(addr) })
}

// fromTrustedProxy reports whether the direct peer of r is a trusted proxy. Only then may
// X-Forwarded-* headers be believed; from anyone else they are client input.
func (s *Server) fromTrustedProxy(r *http.Request) bool {
	if len(s.cfg.TrustedProxies) == 0 {
		return false
	}
	addr, ok := peerAddr(r)
	return ok && s.trusted(addr)
}

// clientIP is the address of the client that made the request. It is the host part of the
// peer address unless the peer is in [server] trusted_proxies, in which case it is the
// rightmost X-Forwarded-For entry that is not itself a trusted proxy: each proxy appends
// the address it saw, so entries to the left of that one are whatever the client claimed.
// With no trusted proxies configured (the default) the headers are never read.
func (s *Server) clientIP(r *http.Request) string {
	peer, ok := peerAddr(r)
	if !ok {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return r.RemoteAddr
		}
		return host
	}
	if !s.fromTrustedProxy(r) {
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for _, h := range slices.Backward(hops) {
		addr, err := netip.ParseAddr(strings.TrimSpace(h))
		if err != nil {
			break // not an address: nothing further left can be believed either
		}
		if addr = addr.Unmap(); !s.trusted(addr) {
			return addr.String()
		}
	}
	return peer.String()
}

// isHTTPS reports whether the client's connection was HTTPS: the server has no TLS of its
// own, so this is true only when a trusted proxy says so in X-Forwarded-Proto (its last
// value is the one the nearest proxy set).
func (s *Server) isHTTPS(r *http.Request) bool {
	if !s.fromTrustedProxy(r) {
		return false
	}
	vals := strings.Split(strings.Join(r.Header.Values("X-Forwarded-Proto"), ","), ",")
	return strings.EqualFold(strings.TrimSpace(vals[len(vals)-1]), "https")
}
