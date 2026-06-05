package search

import (
	"strings"
	"time"

	"github.com/fanlv/opensearch/internal/urlnorm"
)

// NormalizeResults applies the search result contract:
//   - Normalize every URL with the shared URL rules and drop invalid URLs.
//   - Re-apply include/exclude domain filters, with exclude taking precedence.
//   - Re-apply published date filters when requested.
//   - Deduplicate by normalized URL without fragment while preserving order.
//   - Cap at numResults and bound variable-length metadata.
func NormalizeResults(raws []RawResult, p *Params) []Result {
	out := make([]Result, 0, len(raws))
	seen := make(map[string]struct{}, len(raws))

	for _, r := range raws {
		n, err := urlnorm.Normalize(r.URL)
		if err != nil {
			continue
		}
		if len(p.ExcludeDomains) > 0 && matchesAny(n.Host, p.ExcludeDomains) {
			continue
		}
		if len(p.IncludeDomains) > 0 && !matchesAny(n.Host, p.IncludeDomains) {
			continue
		}
		if !matchesPublishedRange(r.PublishedDate, p) {
			continue
		}
		if _, dup := seen[n.ForDedup]; dup {
			continue
		}
		seen[n.ForDedup] = struct{}{}

		title, titleTruncated := truncateWithFlag(r.Title, titleMaxBytes)
		publishedDate, publishedDateTruncated := truncateWithFlag(r.PublishedDate, publishedDateMaxBytes)
		out = append(out, Result{
			Title:                  title,
			TitleTruncated:         titleTruncated,
			URL:                    n.URL,
			PublishedDate:          publishedDate,
			PublishedDateTruncated: publishedDateTruncated,
			Snippet:                truncate(r.Snippet, snippetMaxBytes),
		})
		if p.NumResults > 0 && len(out) >= p.NumResults {
			break
		}
	}
	return out
}

func matchesPublishedRange(value string, p *Params) bool {
	if p.PublishedAfter == nil && p.PublishedBefore == nil {
		return true
	}
	t, ok := parseProviderPublishedDate(value)
	if !ok {
		return false
	}
	if p.PublishedAfter != nil && t.Before(*p.PublishedAfter) {
		return false
	}
	if p.PublishedBefore != nil && t.After(*p.PublishedBefore) {
		return false
	}
	return true
}

func parseProviderPublishedDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// truncate cuts a string to maxBytes without splitting a UTF-8 sequence.
func truncate(s string, maxBytes int) string {
	out, _ := truncateWithFlag(s, maxBytes)
	return out
}

func truncateWithFlag(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && !utf8Boundary(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// utf8Boundary reports whether b starts a UTF-8 sequence.
func utf8Boundary(b byte) bool {
	return b&0xC0 != 0x80
}
