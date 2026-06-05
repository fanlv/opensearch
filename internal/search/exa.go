package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const exaMCPURL = "https://mcp.exa.ai/mcp"

// providerTimeout is the provider request timeout.
const providerTimeout = 25 * time.Second

// maxProviderBodyBytes bounds provider response reads.
const maxProviderBodyBytes = 8 * 1024 * 1024

// Doer abstracts HTTP execution so tests can inject a stub.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client calls Exa's MCP search endpoint. apiKey is optional and, when set,
// is appended as the exaApiKey query parameter used by the upstream MCP server.
type Client struct {
	apiKey string
	doer   Doer
}

// NewClient builds a default Client with providerTimeout and proxy env ignored.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		doer: &http.Client{
			Timeout:   providerTimeout,
			Transport: &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// NewClientWithDoer builds a Client with an injected Doer for tests.
func NewClientWithDoer(apiKey string, doer Doer) *Client {
	return &Client{apiKey: apiKey, doer: doer}
}

type mcpRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  mcpCallParams `json:"params"`
}

type mcpCallParams struct {
	Name      string        `json:"name"`
	Arguments mcpSearchArgs `json:"arguments"`
}

type mcpSearchArgs struct {
	Query      string `json:"query"`
	NumResults int    `json:"numResults,omitempty"`
}

type mcpResponse struct {
	Result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type exaResponse struct {
	Results json.RawMessage `json:"results"`
}

type exaResult struct {
	Title         json.RawMessage `json:"title"`
	URL           json.RawMessage `json:"url"`
	PublishedDate json.RawMessage `json:"publishedDate"`
	Summary       json.RawMessage `json:"summary"`
	Text          json.RawMessage `json:"text"`
	Highlights    json.RawMessage `json:"highlights"`
}

// RawResult is a provider candidate before URL normalization, deduplication,
// and domain filtering. It is exported so cli tests can inject stub clients.
type RawResult struct {
	Title         string
	URL           string
	PublishedDate string
	Snippet       string
}

// Search calls Exa MCP and returns raw candidates in provider order.
func (c *Client) Search(ctx context.Context, p *Params) ([]RawResult, error) {
	body := mcpRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: mcpCallParams{
			Name: "web_search_exa",
			Arguments: mcpSearchArgs{
				Query:      providerQuery(p),
				NumResults: providerNumResults(p.NumResults),
			},
		},
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, errProviderInvalid()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(buf))
	if err != nil {
		return nil, errProviderUnavailable()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := c.doer.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, errProviderUnavailable()
		}
		return nil, errProviderUnavailable()
	}
	defer resp.Body.Close()

	raw, err := readProviderBody(resp.Body)
	if err != nil {
		return nil, errProviderUnavailable()
	}

	if perr := mapStatus(resp.StatusCode); perr != nil {
		return nil, perr
	}

	text, ok := firstMCPText(raw)
	if !ok {
		return nil, errProviderInvalid()
	}
	results, err := parseExaResults(text)
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (c *Client) endpoint() string {
	if strings.TrimSpace(c.apiKey) == "" {
		return exaMCPURL
	}
	u, err := url.Parse(exaMCPURL)
	if err != nil {
		return exaMCPURL
	}
	q := u.Query()
	q.Set("exaApiKey", c.apiKey)
	u.RawQuery = q.Encode()
	return u.String()
}

func readProviderBody(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, int64(maxProviderBodyBytes+1)))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxProviderBodyBytes {
		return nil, errors.New("provider response too large")
	}
	return raw, nil
}

func providerNumResults(n int) int {
	if n <= 0 {
		return defaultNumResult
	}
	if n > maxNumResult {
		return maxNumResult
	}
	return n
}

func providerQuery(p *Params) string {
	var b strings.Builder
	b.WriteString(p.Query)
	for _, d := range p.IncludeDomains {
		b.WriteString(" site:")
		b.WriteString(d)
	}
	for _, d := range p.ExcludeDomains {
		b.WriteString(" -site:")
		b.WriteString(d)
	}
	return b.String()
}

func firstMCPText(raw []byte) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if text, ok := parseMCPPayload(trimmed); ok {
			return text, true
		}
	}

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if text, ok := parseMCPPayload([]byte(payload)); ok {
			return text, true
		}
	}
	return "", false
}

func parseMCPPayload(raw []byte) (string, bool) {
	var parsed mcpResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", false
	}
	if parsed.Error != nil {
		return "", false
	}
	for _, item := range parsed.Result.Content {
		if item.Text != "" {
			return item.Text, true
		}
	}
	return "", false
}

func parseExaResults(text string) ([]RawResult, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return []RawResult{}, nil
	}
	if strings.HasPrefix(trimmed, "{") {
		return parseExaJSONResults([]byte(trimmed))
	}
	return parseExaTextResults(trimmed), nil
}

func parseExaJSONResults(raw []byte) ([]RawResult, error) {
	var parsed exaResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, errProviderInvalid()
	}
	if len(parsed.Results) == 0 || bytes.Equal(parsed.Results, []byte("null")) {
		return nil, errProviderInvalid()
	}

	var items []json.RawMessage
	if err := json.Unmarshal(parsed.Results, &items); err != nil {
		return nil, errProviderInvalid()
	}

	out := make([]RawResult, 0, len(items))
	for _, item := range items {
		if r, ok := parseExaResult(item); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func parseExaTextResults(text string) []RawResult {
	var out []RawResult
	var cur *RawResult
	var highlights []string
	inHighlights := false

	flush := func() {
		if cur == nil {
			return
		}
		cur.Snippet = strings.TrimSpace(strings.Join(highlights, "\n"))
		if cur.URL != "" {
			out = append(out, *cur)
		}
		cur = nil
		highlights = nil
		inHighlights = false
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Title:"):
			flush()
			cur = &RawResult{Title: strings.TrimSpace(strings.TrimPrefix(trimmed, "Title:"))}
		case cur == nil:
			continue
		case strings.HasPrefix(trimmed, "URL:"):
			cur.URL = strings.TrimSpace(strings.TrimPrefix(trimmed, "URL:"))
			inHighlights = false
		case strings.HasPrefix(trimmed, "Published:"):
			published := strings.TrimSpace(strings.TrimPrefix(trimmed, "Published:"))
			if !strings.EqualFold(published, "N/A") {
				cur.PublishedDate = published
			}
			inHighlights = false
		case strings.HasPrefix(trimmed, "Highlights:"):
			inHighlights = true
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "Highlights:"))
			if rest != "" {
				highlights = append(highlights, rest)
			}
		case trimmed == "---":
			flush()
		case inHighlights:
			highlights = append(highlights, line)
		}
	}
	flush()
	return out
}

func parseExaResult(raw json.RawMessage) (RawResult, bool) {
	var r exaResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return RawResult{}, false
	}
	urlValue, ok := optionalString(r.URL)
	if !ok || urlValue == "" {
		return RawResult{}, false
	}
	title, _ := optionalString(r.Title)
	publishedDate, _ := optionalString(r.PublishedDate)
	summary, _ := optionalString(r.Summary)
	text, _ := optionalString(r.Text)
	snippet := summary
	if snippet == "" {
		snippet = joinHighlightMessages(r.Highlights)
	}
	if snippet == "" {
		snippet = text
	}
	return RawResult{
		Title:         title,
		URL:           urlValue,
		PublishedDate: publishedDate,
		Snippet:       snippet,
	}, true
}

func optionalString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", true
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func joinHighlightMessages(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	highlights := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := optionalString(item); ok {
			highlights = append(highlights, text)
		}
	}
	return joinHighlights(highlights)
}

// mapStatus maps HTTP status codes to stable provider error codes. Only 200 is
// accepted because the MCP endpoint returns completed tool results synchronously.
func mapStatus(code int) *ProviderError {
	switch {
	case code == http.StatusOK:
		return nil
	case code >= 200 && code < 300:
		return errProviderInvalid()
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return errProviderAuth()
	case code == http.StatusTooManyRequests:
		return errProviderRateLimited()
	case code == http.StatusRequestTimeout || code >= 500:
		return errProviderUnavailable()
	default:
		return errProviderInvalid()
	}
}

func joinHighlights(hs []string) string {
	parts := make([]string, 0, len(hs))
	for _, h := range hs {
		if h != "" {
			parts = append(parts, h)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " … " + p
	}
	return out
}
