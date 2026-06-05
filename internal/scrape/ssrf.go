package scrape

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/fanlv/opensearch/internal/result"
	"github.com/fanlv/opensearch/internal/urlnorm"
)

var (
	metadataHostnames = []string{
		"metadata.google.internal",
		"instance-data.ec2.internal",
	}

	blockedIPv4Prefixes = mustPrefixes(
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"168.63.129.16/32", // Azure wireserver/metadata: globally-routable but a cloud-metadata target.
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.0.0.0/24",
		"192.0.2.0/24",
		"192.31.196.0/24",
		"192.52.193.0/24",
		"192.88.99.0/24",
		"192.168.0.0/16",
		"192.175.48.0/24",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"224.0.0.0/4",
		"240.0.0.0/4",
	)

	blockedIPv6Prefixes = mustPrefixes(
		"::/128",
		"::1/128",
		"::ffff:0:0/96",
		"64:ff9b::/96",
		"64:ff9b:1::/48",
		"100::/64",
		"100:0:0:1::/64",
		"2001::/23",
		"2001:db8::/32",
		"2002::/16",
		"3fff::/20",
		"5f00::/16",
		"2620:4f:8000::/48",
		"fc00::/7",
		"fe80::/10",
		"ff00::/8",
	)
)

var defaultResolver netResolver = net.DefaultResolver

type netResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type targetError struct {
	code     string
	msg      string
	finalURL string
}

func (e *targetError) Error() string { return e.msg }

func ssrfBlocked(msg string) error    { return &targetError{code: result.CodeSSRFBlocked, msg: msg} }
func networkFailure(msg string) error { return &targetError{code: result.CodeNetworkError, msg: msg} }
func scrapeTimeout(msg string) error  { return &targetError{code: result.CodeScrapeTimeout, msg: msg} }

func withFinalURL(err error, finalURL string) error {
	if err == nil || finalURL == "" {
		return err
	}
	var te *targetError
	if errors.As(err, &te) {
		if te.finalURL == "" {
			copy := *te
			copy.finalURL = finalURL
			return &copy
		}
		return te
	}
	return err
}

func errorFinalURL(err error) string {
	var te *targetError
	if errors.As(err, &te) {
		return te.finalURL
	}
	return ""
}

func itemErrorCode(err error) string {
	var te *targetError
	if errors.As(err, &te) {
		return te.code
	}
	return result.CodeNetworkError
}

// ResolvedTarget is the public, DNS-validated address set for a URL target. The
// future HTTP fetcher must only connect to these addresses for this host.
type ResolvedTarget struct {
	Host  string
	Addrs []netip.Addr
}

func (t ResolvedTarget) contains(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, allowed := range t.Addrs {
		if allowed.Unmap() == addr {
			return true
		}
	}
	return false
}

// validatePublicTarget validates the scheme-normalized URL target against the
// public-target / SSRF contract. It rejects local metadata names, single-label
// hostnames, any IANA special-purpose address, and DNS answers containing any
// restricted address. Returned addresses are safe to bind future connections to.
func validatePublicTarget(ctx context.Context, norm *urlnorm.Normalized, r netResolver) (*ResolvedTarget, error) {
	return validatePublicHost(ctx, norm.Host, r)
}

func validatePublicHost(ctx context.Context, host string, r netResolver) (*ResolvedTarget, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return nil, ssrfBlocked("empty host is not a public target")
	}
	if isBlockedHostname(host) {
		return nil, ssrfBlocked("host is not a public target")
	}

	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if !isPublicAddress(addr) {
			return nil, ssrfBlocked("address is not a public target")
		}
		return &ResolvedTarget{Host: host, Addrs: []netip.Addr{addr}}, nil
	}

	if !strings.Contains(host, ".") {
		return nil, ssrfBlocked("single-label host is not a public target")
	}
	if r == nil {
		return nil, ssrfBlocked("DNS resolver is unavailable")
	}

	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		if ctx.Err() != nil {
			return nil, scrapeTimeout("DNS resolution timed out")
		}
		return nil, networkFailure("DNS resolution failed")
	}
	if len(addrs) == 0 {
		return nil, networkFailure("DNS resolution returned no addresses")
	}

	resolved := make([]netip.Addr, 0, len(addrs))
	seen := make(map[netip.Addr]struct{}, len(addrs))
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !isPublicAddress(addr) {
			return nil, ssrfBlocked("DNS resolved to a non-public target")
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		resolved = append(resolved, addr)
	}
	return &ResolvedTarget{Host: host, Addrs: resolved}, nil
}

func isBlockedHostname(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	for _, blocked := range metadataHostnames {
		if host == blocked || strings.HasSuffix(host, "."+blocked) {
			return true
		}
	}
	return false
}

func isPublicAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() {
		return false
	}
	prefixes := blockedIPv6Prefixes
	if addr.Is4() {
		prefixes = blockedIPv4Prefixes
	}
	for _, p := range prefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

type dialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// resolvedTargetDialContext binds one HTTP request to the exact address set
// returned by validatePublicTarget for that request/redirect target. It never
// performs an unconstrained second DNS lookup before opening the socket.
func resolvedTargetDialContext(target *ResolvedTarget, dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port == "" {
			return nil, ssrfBlocked("dial target cannot be constrained")
		}
		host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(host, "]"), "["), "."))
		if target == nil || host != target.Host {
			return nil, ssrfBlocked("dial target is outside validated address set")
		}
		if len(target.Addrs) == 0 {
			return nil, ssrfBlocked("dial target has no validated address")
		}
		for _, addr := range target.Addrs {
			if !target.contains(addr) {
				continue
			}
			conn, dialErr := dial(ctx, network, net.JoinHostPort(addr.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			err = dialErr
		}
		if err != nil {
			return nil, err
		}
		return nil, ssrfBlocked("dial target cannot be constrained")
	}
}

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		p, err := netip.ParsePrefix(v)
		if err != nil {
			panic(fmt.Sprintf("invalid prefix %s: %v", v, err))
		}
		prefixes = append(prefixes, p)
	}
	return prefixes
}
