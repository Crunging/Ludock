package auth

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

var (
	ipv4Literal  = regexp.MustCompile(`^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$`)
	ipv6Literal  = regexp.MustCompile(`(?i)^[0-9a-f:.]+$`)
	prefixLength = regexp.MustCompile(`^(0|[1-9]\d{0,2})$`)
)

type ipAddress struct {
	address string
	value   netip.Addr // always 16-byte form
	bits    int
}

// parseIP accepts strict literals only. IPv4 excludes shorthand, octal
// addresses, and ports. An IPv4 peer and its IPv4-mapped IPv6 form are one
// source.
func parseIP(value string) (ipAddress, bool) {
	if ipv4Literal.MatchString(value) {
		addr, err := netip.ParseAddr(value)
		if err != nil {
			return ipAddress{}, false
		}
		return ipAddress{value, netip.AddrFrom16(addr.As16()), 32}, true
	}
	if !strings.Contains(value, ":") || !ipv6Literal.MatchString(value) {
		return ipAddress{}, false
	}
	addr, err := netip.ParseAddr(value)
	if err != nil || !addr.Is6() {
		return ipAddress{}, false
	}
	address := addr.String()
	if addr.Is4In6() {
		address = addr.Unmap().String()
	}
	return ipAddress{address, addr, 128}, true
}

// ClientIP resolves the client address of a request. Only a configured proxy
// may supply the next hop; walking right to left stops before
// client-controlled prefixes in an appended forwarding chain.
type ClientIP struct{ trusted []netip.Prefix }

// ErrTrustedProxies rejects invalid LUDOCK_TRUSTED_PROXIES entries instead of
// broadening proxy access.
var ErrTrustedProxies = errors.New("LUDOCK_TRUSTED_PROXIES must contain comma-separated IP addresses or CIDRs")

// NewClientIP parses LUDOCK_TRUSTED_PROXIES.
func NewClientIP(configured string) (*ClientIP, error) {
	resolver := &ClientIP{}
	if strings.TrimSpace(configured) == "" {
		return resolver, nil
	}
	for _, entry := range strings.Split(configured, ",") {
		parts := strings.Split(strings.TrimSpace(entry), "/")
		ip, ok := parseIP(parts[0])
		if !ok || len(parts) > 2 {
			return nil, ErrTrustedProxies
		}
		length := ip.bits
		if len(parts) == 2 {
			if !prefixLength.MatchString(parts[1]) {
				return nil, ErrTrustedProxies
			}
			length, _ = strconv.Atoi(parts[1])
			if length > ip.bits {
				return nil, ErrTrustedProxies
			}
		}
		prefix, err := ip.value.Prefix(length + 128 - ip.bits)
		if err != nil {
			return nil, ErrTrustedProxies
		}
		resolver.trusted = append(resolver.trusted, prefix)
	}
	return resolver, nil
}

func (c *ClientIP) isTrusted(ip ipAddress) bool {
	for _, prefix := range c.trusted {
		if prefix.Contains(ip.value) {
			return true
		}
	}
	return false
}

// Resolve returns the client address for a peer address and the request's
// X-Forwarded-For header.
func (c *ClientIP) Resolve(forwardedFor, peerAddress string) string {
	if peerAddress == "" {
		return ""
	}
	peer, ok := parseIP(peerAddress)
	if !ok {
		return peerAddress
	}
	if !c.isTrusted(peer) || forwardedFor == "" {
		return peer.address
	}
	hops := strings.Split(forwardedFor, ",")
	if len(hops) > 32 {
		return peer.address
	}
	current := peer
	for index := len(hops) - 1; index >= 0 && c.isTrusted(current); index-- {
		next, ok := parseIP(strings.TrimSpace(hops[index]))
		// Malformed trusted metadata cannot select an attacker-chosen source.
		if !ok {
			return peer.address
		}
		current = next
	}
	return current.address
}

// Request resolves the client address of an HTTP request.
func (c *ClientIP) Request(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return c.Resolve(r.Header.Get("X-Forwarded-For"), host)
}
