package search

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fanlv/opensearch/internal/result"
)

func TestParseArgsSearchContract(t *testing.T) {
	after := "2026-06-01T00:00:00+08:00"
	before := "2026-06-05T12:00:00+08:00"
	p, err := ParseArgs([]string{
		"--num-results=12",
		"--published-after", after,
		"--published-before", before,
		"--include-domain", "Example.COM.",
		"--include-domain", "example.com",
		"--exclude-domain=Bad.Example.com.",
		"-o", "/tmp/search.json",
		"  golang", "release  ",
	})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if p.Query != "golang release" {
		t.Fatalf("Query = %q, want trimmed joined query", p.Query)
	}
	if p.NumResults != 12 {
		t.Fatalf("NumResults = %d, want 12", p.NumResults)
	}
	if p.PublishedAfter == nil || p.PublishedAfter.Format(time.RFC3339) != after {
		t.Fatalf("PublishedAfter = %v, want %s", p.PublishedAfter, after)
	}
	if p.PublishedBefore == nil || p.PublishedBefore.Format(time.RFC3339) != before {
		t.Fatalf("PublishedBefore = %v, want %s", p.PublishedBefore, before)
	}
	if got := strings.Join(p.IncludeDomains, ","); got != "example.com" {
		t.Fatalf("IncludeDomains = %q, want normalized and deduped example.com", got)
	}
	if got := strings.Join(p.ExcludeDomains, ","); got != "bad.example.com" {
		t.Fatalf("ExcludeDomains = %q, want normalized bad.example.com", got)
	}
	if p.OutputPath != "/tmp/search.json" {
		t.Fatalf("OutputPath = %q", p.OutputPath)
	}
}

func TestParseArgsRejectsInvalidSearchInputs(t *testing.T) {
	tooManyDomains := []string{"--include-domain"}
	for i := 0; i < maxDomains+1; i++ {
		tooManyDomains = append(tooManyDomains, "d"+string(rune('a'+i))+".example")
		if i != maxDomains {
			tooManyDomains = append(tooManyDomains, "--include-domain")
		}
	}
	tooManyDomains = append(tooManyDomains, "query")

	cases := []struct {
		name string
		args []string
	}{
		{name: "empty query", args: []string{}},
		{name: "query too long", args: []string{strings.Repeat("x", queryMaxLen+1)}},
		{name: "num too small", args: []string{"-n", "0", "query"}},
		{name: "num too large", args: []string{"--num-results", "21", "query"}},
		{name: "bad time", args: []string{"--published-after", "2026-06-01", "query"}},
		{name: "empty published after equals", args: []string{"--published-after=", "query"}},
		{name: "empty published after value", args: []string{"--published-after", "", "query"}},
		{name: "empty published before equals", args: []string{"--published-before=", "query"}},
		{name: "empty published before value", args: []string{"--published-before", "", "query"}},
		{name: "after later than before", args: []string{"--published-after", "2026-06-05T00:00:00Z", "--published-before", "2026-06-01T00:00:00Z", "query"}},
		{name: "same include and exclude", args: []string{"--include-domain", "example.com", "--exclude-domain", "EXAMPLE.com.", "query"}},
		{name: "domain with scheme", args: []string{"--include-domain", "https://example.com", "query"}},
		{name: "domain ipv4 literal", args: []string{"--include-domain", "127.0.0.1", "query"}},
		{name: "domain ipv6 literal", args: []string{"--exclude-domain", "::1", "query"}},
		{name: "too many domains", args: tooManyDomains},
		{name: "empty output path", args: []string{"--output=", "query"}},
		{name: "blank output path", args: []string{"-o", "  ", "query"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseArgs(tc.args)
			if !errors.Is(err, ErrInvalidArgs) {
				t.Fatalf("ParseArgs error = %v, want ErrInvalidArgs", err)
			}
		})
	}
}

func TestParseArgsQueryLengthCountsCharacters(t *testing.T) {
	if _, err := ParseArgs([]string{strings.Repeat("测", queryMaxLen)}); err != nil {
		t.Fatalf("2048 multibyte characters should be accepted: %v", err)
	}
	if _, err := ParseArgs([]string{strings.Repeat("测", queryMaxLen+1)}); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("2049 characters should be rejected, got %v", err)
	}
}

func TestNormalizeResultsAppliesSearchContract(t *testing.T) {
	p := &Params{
		NumResults:     3,
		IncludeDomains: []string{"example.com"},
		ExcludeDomains: []string{"bad.example.com"},
	}
	longTitle := strings.Repeat("题", titleMaxBytes)
	longSnippet := strings.Repeat("摘", snippetMaxBytes)
	longPublishedDate := strings.Repeat("2", publishedDateMaxBytes+1)
	raw := []RawResult{
		{Title: "invalid", URL: "ftp://example.com/doc"},
		{Title: "excluded", URL: "https://bad.example.com/doc"},
		{Title: "outside", URL: "https://other.test/doc"},
		{Title: longTitle, URL: "HTTPS://EXAMPLE.com:443/a#one", PublishedDate: longPublishedDate, Snippet: longSnippet},
		{Title: "duplicate", URL: "https://example.com/a#two"},
		{Title: "subdomain", URL: "https://sub.example.com/path", Snippet: "ok"},
		{Title: "capped", URL: "https://example.com/third"},
		{Title: "over cap", URL: "https://example.com/fourth"},
	}

	got := NormalizeResults(raw, p)
	if len(got) != 3 {
		t.Fatalf("len(results) = %d, want 3: %+v", len(got), got)
	}
	if got[0].URL != "https://example.com/a#one" {
		t.Fatalf("first URL = %q, want normalized first non-duplicate URL", got[0].URL)
	}
	if got[1].URL != "https://sub.example.com/path" || got[2].URL != "https://example.com/third" {
		t.Fatalf("unexpected result order/filtering: %+v", got)
	}
	if got[0].Title == longTitle || len(got[0].Title) > titleMaxBytes || !utf8.ValidString(got[0].Title) {
		t.Fatalf("title was not safely truncated: len=%d valid=%v", len(got[0].Title), utf8.ValidString(got[0].Title))
	}
	if !got[0].TitleTruncated {
		t.Fatalf("titleTruncated = false, want true for overlong title")
	}
	if got[0].Snippet == longSnippet || len(got[0].Snippet) > snippetMaxBytes || !utf8.ValidString(got[0].Snippet) {
		t.Fatalf("snippet was not safely truncated: len=%d valid=%v", len(got[0].Snippet), utf8.ValidString(got[0].Snippet))
	}
	if got[0].PublishedDate == longPublishedDate || len(got[0].PublishedDate) > publishedDateMaxBytes || !got[0].PublishedDateTruncated {
		t.Fatalf("publishedDate was not bounded/truncated: len=%d truncated=%v", len(got[0].PublishedDate), got[0].PublishedDateTruncated)
	}
}

func TestNormalizeResultsAppliesPublishedDateFilters(t *testing.T) {
	after := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)
	p := &Params{
		NumResults:      10,
		PublishedAfter:  &after,
		PublishedBefore: &before,
	}
	raw := []RawResult{
		{Title: "too old", URL: "https://example.com/old", PublishedDate: "2026-05-31T23:59:59Z"},
		{Title: "date only", URL: "https://example.com/date", PublishedDate: "2026-06-10"},
		{Title: "nanoseconds", URL: "https://example.com/nano", PublishedDate: "2026-06-15T10:20:30.123Z"},
		{Title: "too new", URL: "https://example.com/new", PublishedDate: "2026-07-01T00:00:00Z"},
		{Title: "unknown", URL: "https://example.com/unknown"},
	}

	got := NormalizeResults(raw, p)
	if len(got) != 2 {
		t.Fatalf("len(results) = %d, want 2: %+v", len(got), got)
	}
	if got[0].URL != "https://example.com/date" || got[1].URL != "https://example.com/nano" {
		t.Fatalf("published filter results = %+v", got)
	}
}

func TestNewClientHTTPClientSafetySettings(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("NO_PROXY", "*")

	client := NewClient("key")
	hc, ok := client.doer.(*http.Client)
	if !ok {
		t.Fatalf("doer type = %T, want *http.Client", client.doer)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", hc.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("search HTTP client must ignore proxy environment variables")
	}
	if hc.CheckRedirect == nil {
		t.Fatal("search HTTP client must disable automatic redirects")
	}
	if err := hc.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect error = %v, want http.ErrUseLastResponse", err)
	}
}

type fakeDoer struct {
	fn func(*http.Request) (*http.Response, error)
}

func (f fakeDoer) Do(req *http.Request) (*http.Response, error) {
	return f.fn(req)
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func mcpTextResponse(t *testing.T, text string) string {
	t.Helper()
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]interface{}{
			"content": []map[string]string{{
				"type": "text",
				"text": text,
			}},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mcpSSETextResponse(t *testing.T, text string) string {
	t.Helper()
	return "event: message\ndata: " + mcpTextResponse(t, text) + "\n\n"
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (errReader) Close() error             { return nil }

func TestClientSearchBuildsExaRequestAndParsesHighlights(t *testing.T) {
	after := time.Date(2026, 6, 1, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))
	before := time.Date(2026, 6, 5, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	client := NewClientWithDoer("test-key", fakeDoer{fn: func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Scheme+"://"+req.URL.Host+req.URL.Path != exaMCPURL {
			t.Fatalf("request = %s %s", req.Method, req.URL.String())
		}
		if req.URL.Query().Get("exaApiKey") != "test-key" {
			t.Fatalf("exaApiKey query = %q", req.URL.Query().Get("exaApiKey"))
		}
		if req.Header.Get("x-api-key") != "" {
			t.Fatalf("x-api-key header should not be set, got %q", req.Header.Get("x-api-key"))
		}
		if req.Header.Get("Accept") != "application/json, text/event-stream" {
			t.Fatalf("Accept header = %q", req.Header.Get("Accept"))
		}
		var body mcpRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.JSONRPC != "2.0" || body.Method != "tools/call" || body.Params.Name != "web_search_exa" {
			t.Fatalf("unexpected request body: %+v", body)
		}
		args := body.Params.Arguments
		if args.Query != "golang release site:go.dev -site:pkg.go.dev" || args.NumResults != 5 {
			t.Fatalf("unexpected search args: %+v", args)
		}
		return response(http.StatusOK, mcpTextResponse(t, `{"results":[{"title":"Go","url":"https://go.dev/","publishedDate":"2026-06-01","highlights":["one","","two"]}]}`)), nil
	}})

	got, err := client.Search(context.Background(), &Params{
		Query:           "golang release",
		NumResults:      5,
		PublishedAfter:  &after,
		PublishedBefore: &before,
		IncludeDomains:  []string{"go.dev"},
		ExcludeDomains:  []string{"pkg.go.dev"},
	})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(got) != 1 || got[0].Snippet != "one … two" {
		t.Fatalf("results = %+v, want joined highlights", got)
	}
}

func TestClientSearchWithoutKeyUsesAnonymousMCPEndpoint(t *testing.T) {
	client := NewClientWithDoer("", fakeDoer{fn: func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != exaMCPURL {
			t.Fatalf("request URL = %q, want %q", req.URL.String(), exaMCPURL)
		}
		return response(http.StatusOK, mcpTextResponse(t, `{"results":[]}`)), nil
	}})

	if _, err := client.Search(context.Background(), &Params{Query: "q", NumResults: 1}); err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
}

func TestClientSearchParsesExaMCPTextResults(t *testing.T) {
	text := `Title: 北京-天气预报 - 中央气象台
URL: https://www.nmc.cn/publish/forecast/ABJ/beijing.html
Published: N/A
Author: N/A
Highlights:
北京-天气预报
[...]
雷阵雨

---

Title: [Meta] Enable smoke tests for OpenSearch distribution + plugins · Issue #5223
URL: https://github.com/opensearch-project/opensearch-build/issues/5223
Published: 2025-01-06T20:20:51.000Z
Author: zelinh
Highlights:
Introduce a smoke test framework to the CI/CD process.
[...]
Onboard Core and plugins for the Smoke test`

	client := NewClientWithDoer("", fakeDoer{fn: func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, mcpSSETextResponse(t, text)), nil
	}})

	got, err := client.Search(context.Background(), &Params{Query: "q", NumResults: 2})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("results = %+v, want two parsed text results", got)
	}
	if got[0].Title != "北京-天气预报 - 中央气象台" || got[0].URL != "https://www.nmc.cn/publish/forecast/ABJ/beijing.html" {
		t.Fatalf("first result parsed incorrectly: %+v", got[0])
	}
	if got[0].PublishedDate != "" || !strings.Contains(got[0].Snippet, "雷阵雨") {
		t.Fatalf("first result metadata parsed incorrectly: %+v", got[0])
	}
	if got[1].PublishedDate != "2025-01-06T20:20:51.000Z" || !strings.Contains(got[1].Snippet, "CI/CD") {
		t.Fatalf("second result metadata parsed incorrectly: %+v", got[1])
	}
}

func TestClientSearchAllowsEmptyResults(t *testing.T) {
	client := NewClientWithDoer("key", fakeDoer{fn: func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, mcpTextResponse(t, `{"results":[]}`)), nil
	}})

	got, err := client.Search(context.Background(), &Params{Query: "q", NumResults: 1})
	if err != nil {
		t.Fatalf("Search returned error for empty results: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("results = %+v, want empty list", got)
	}
}

func TestClientSearchMissingHighlightsDoesNotFailResult(t *testing.T) {
	client := NewClientWithDoer("key", fakeDoer{fn: func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, mcpTextResponse(t, `{"results":[{"title":"No Highlights","url":"https://example.com/page","publishedDate":"2026-06-01"}]}`)), nil
	}})

	got, err := client.Search(context.Background(), &Params{Query: "q", NumResults: 1})
	if err != nil {
		t.Fatalf("Search returned error for missing highlights: %v", err)
	}
	if len(got) != 1 || got[0].Title != "No Highlights" || got[0].URL != "https://example.com/page" {
		t.Fatalf("results = %+v, want retained result without highlights", got)
	}
	if got[0].Snippet != "" {
		t.Fatalf("snippet = %q, want empty", got[0].Snippet)
	}
}

func TestClientSearchSkipsMalformedResults(t *testing.T) {
	client := NewClientWithDoer("key", fakeDoer{fn: func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, mcpTextResponse(t, `{"results":[{"title":"Good","url":"https://example.com/page","publishedDate":"2026-06-01","highlights":["one",123,"two"]},{"title":"Bad URL","url":123},{"title":"Object URL","url":{"nested":true}}]}`)), nil
	}})

	got, err := client.Search(context.Background(), &Params{Query: "q", NumResults: 3})
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("results = %+v, want only valid result", got)
	}
	if got[0].URL != "https://example.com/page" || got[0].Snippet != "one … two" {
		t.Fatalf("valid result not preserved correctly: %+v", got[0])
	}
}

func TestClientSearchMapsProviderErrorsWithoutLeakingBody(t *testing.T) {
	cases := []struct {
		status int
		code   string
	}{
		{status: http.StatusUnauthorized, code: result.CodeProviderAuth},
		{status: http.StatusForbidden, code: result.CodeProviderAuth},
		{status: http.StatusRequestTimeout, code: result.CodeProviderUnavailable},
		{status: http.StatusTooManyRequests, code: result.CodeProviderRateLimited},
		{status: http.StatusBadGateway, code: result.CodeProviderUnavailable},
		{status: http.StatusBadRequest, code: result.CodeProviderError},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			client := NewClientWithDoer("secret-key", fakeDoer{fn: func(*http.Request) (*http.Response, error) {
				return response(tc.status, `{"provider":"raw secret details"}`), nil
			}})
			_, err := client.Search(context.Background(), &Params{Query: "q", NumResults: 1})
			perr, ok := err.(*ProviderError)
			if !ok {
				t.Fatalf("error = %T %[1]v, want *ProviderError", err)
			}
			if perr.Code != tc.code {
				t.Fatalf("error code = %q, want %q", perr.Code, tc.code)
			}
			if strings.Contains(perr.Error(), "secret") || strings.Contains(perr.Error(), "raw") {
				t.Fatalf("provider raw body leaked in error: %q", perr.Error())
			}
		})
	}
}

func TestClientSearchInvalidJSONIsProviderError(t *testing.T) {
	client := NewClientWithDoer("key", fakeDoer{fn: func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `not json`), nil
	}})
	_, err := client.Search(context.Background(), &Params{Query: "q", NumResults: 1})
	perr, ok := err.(*ProviderError)
	if !ok || perr.Code != result.CodeProviderError {
		t.Fatalf("error = %T %[1]v, want PROVIDER_ERROR", err)
	}
}

func TestClientSearchMissingResultsIsProviderError(t *testing.T) {
	for _, body := range []string{`{}`, `{"error":"provider failed"}`, `{"results":null}`} {
		t.Run(body, func(t *testing.T) {
			client := NewClientWithDoer("key", fakeDoer{fn: func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, mcpTextResponse(t, body)), nil
			}})
			_, err := client.Search(context.Background(), &Params{Query: "q", NumResults: 1})
			perr, ok := err.(*ProviderError)
			if !ok || perr.Code != result.CodeProviderError {
				t.Fatalf("error = %T %[1]v, want PROVIDER_ERROR", err)
			}
		})
	}
}

func TestClientSearchRejectsNonOK2xxStatus(t *testing.T) {
	client := NewClientWithDoer("key", fakeDoer{fn: func(*http.Request) (*http.Response, error) {
		return response(http.StatusCreated, `{"results":[]}`), nil
	}})
	_, err := client.Search(context.Background(), &Params{Query: "q", NumResults: 1})
	perr, ok := err.(*ProviderError)
	if !ok || perr.Code != result.CodeProviderError {
		t.Fatalf("error = %T %[1]v, want PROVIDER_ERROR", err)
	}
}

func TestClientSearchBodyReadErrorIsProviderUnavailable(t *testing.T) {
	client := NewClientWithDoer("key", fakeDoer{fn: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: errReader{}, Header: make(http.Header)}, nil
	}})
	_, err := client.Search(context.Background(), &Params{Query: "q", NumResults: 1})
	perr, ok := err.(*ProviderError)
	if !ok || perr.Code != result.CodeProviderUnavailable {
		t.Fatalf("error = %T %[1]v, want PROVIDER_UNAVAILABLE", err)
	}
}
