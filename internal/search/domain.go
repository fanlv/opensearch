package search

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"golang.org/x/net/idna"
)

// ErrInvalidArgs marks argument or domain validation failures.
var ErrInvalidArgs = errors.New("invalid argument")

func invalidArg(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgs, fmt.Sprintf(format, args...))
}

// idnaProfile matches urlnorm's strict lookup profile and converts IDNs to ASCII.
// Search domain filters only accept hostnames, so they get a focused validation pass.
var idnaProfile = idna.New(
	idna.MapForLookup(),
	idna.BidiRule(),
	idna.ValidateLabels(true),
)

// normalizeDomain normalizes an include/exclude domain into a lowercase ASCII
// hostname, ignores a trailing root dot, and rejects non-hostname forms.
func normalizeDomain(raw string) (string, error) {
	d := strings.TrimSpace(raw)
	if d == "" {
		return "", invalidArg("domain must not be empty")
	}
	// Only bare hostnames are allowed: no scheme, path, port, userinfo, or spaces.
	if strings.ContainsAny(d, "/\\:@?# ") {
		return "", invalidArg("domain %q must be a bare hostname (no scheme, port, path, or userinfo)", raw)
	}
	d = strings.TrimSuffix(d, ".")
	if d == "" || strings.HasPrefix(d, ".") || strings.Contains(d, "..") {
		return "", invalidArg("malformed domain %q", raw)
	}
	ascii, err := idnaProfile.ToASCII(d)
	if err != nil {
		return "", invalidArg("invalid domain %q: %v", raw, err)
	}
	ascii = strings.ToLower(ascii)
	if ascii == "" {
		return "", invalidArg("domain %q normalizes to empty", raw)
	}
	if _, err := netip.ParseAddr(ascii); err == nil {
		return "", invalidArg("domain %q must be a DNS hostname, not an IP literal", raw)
	}
	return ascii, nil
}

// normalizeDomainList normalizes, deduplicates in first-seen order, and enforces max.
func normalizeDomainList(raws []string, max int, label string) ([]string, error) {
	if len(raws) > max {
		return nil, invalidArg("at most %d %s domains are allowed", max, label)
	}
	seen := make(map[string]struct{}, len(raws))
	out := make([]string, 0, len(raws))
	for _, r := range raws {
		d, err := normalizeDomain(r)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out, nil
}

// domainMatches reports whether normalized host equals d or is a subdomain of d.
func domainMatches(host, d string) bool {
	if host == d {
		return true
	}
	return strings.HasSuffix(host, "."+d)
}

// matchesAny reports whether host matches any domain in the list.
func matchesAny(host string, domains []string) bool {
	for _, d := range domains {
		if domainMatches(host, d) {
			return true
		}
	}
	return false
}
