// Package search implements the search subcommand: argument parsing, Exa MCP
// provider calls, URL normalization, deduplication, domain/date filtering, and
// output data shaped for references/output-schema.md.
package search

import (
	"github.com/fanlv/opensearch/internal/result"
)

// These bounds keep variable-length metadata stable in direct stdout and
// summaries. URLs are never truncated here because truncating can make them
// invalid; URL handling lives in normalize.go.
const (
	snippetMaxBytes       = 4096
	titleMaxBytes         = 1024
	publishedDateMaxBytes = 128
)

// Result is a normalized candidate source in the search data payload. URL is
// required; title, publishedDate, and snippet are omitted when missing.
type Result struct {
	Title                  string `json:"title,omitempty"`
	TitleTruncated         bool   `json:"titleTruncated,omitempty"`
	URL                    string `json:"url"`
	PublishedDate          string `json:"publishedDate,omitempty"`
	PublishedDateTruncated bool   `json:"publishedDateTruncated,omitempty"`
	Snippet                string `json:"snippet,omitempty"`
}

// Data is envelope.data for successful search commands. Empty results are a
// successful no-result response.
type Data struct {
	Results []Result `json:"results"`
}

// ProviderError carries a stable command-level provider error code. Msg is
// safe for model/user output and must not include API keys or raw provider
// response bodies.
type ProviderError struct {
	Code string
	Msg  string
}

func (e *ProviderError) Error() string { return e.Msg }

func providerErr(code, msg string) *ProviderError {
	return &ProviderError{Code: code, Msg: msg}
}

// Provider error constructors centralize safe messages.
func errProviderAuth() *ProviderError {
	return providerErr(result.CodeProviderAuth, "search provider rejected authentication")
}

func errProviderRateLimited() *ProviderError {
	return providerErr(result.CodeProviderRateLimited, "search provider rate limited the request")
}

func errProviderUnavailable() *ProviderError {
	return providerErr(result.CodeProviderUnavailable, "search provider is unavailable (timeout, network, or service error)")
}

func errProviderInvalid() *ProviderError {
	return providerErr(result.CodeProviderError, "search provider returned an invalid or unexpected response")
}
