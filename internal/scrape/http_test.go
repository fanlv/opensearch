package scrape

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/fanlv/opensearch/internal/result"
	nethtml "golang.org/x/net/html"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func withFakeHTTPClient(t *testing.T, fn roundTripFunc) {
	t.Helper()
	orig := newHTTPClient
	newHTTPClient = func() *http.Client {
		return &http.Client{
			Transport: fn,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	t.Cleanup(func() { newHTTPClient = orig })
}

func response(status int, headers map[string]string, body []byte) *http.Response {
	h := make(http.Header)
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode:    status,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func scrapeOne(t *testing.T, raw string) Result {
	t.Helper()
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{raw},
		Format:            FormatMarkdown,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
		UserAgent:         "opensearch-cli/test",
	})
	if len(data.Results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(data.Results))
	}
	return data.Results[0]
}

func TestFetchURLSuccessAnonymousGET(t *testing.T) {
	withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", req.Method)
		}
		if req.Header.Get("User-Agent") != "opensearch-cli/test" {
			t.Fatalf("user agent = %q", req.Header.Get("User-Agent"))
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Fatalf("anonymous GET should not send credentials: headers=%v", req.Header)
		}
		return response(http.StatusOK, map[string]string{"Content-Type": "text/plain"}, []byte("hello world")), nil
	})

	item := scrapeOne(t, "https://8.8.8.8/path#fragment")
	if !item.Success || item.Error != nil {
		t.Fatalf("item should succeed: %+v", item)
	}
	if item.URL != "https://8.8.8.8/path" || item.FinalURL != "https://8.8.8.8/path" {
		t.Fatalf("url/finalUrl = %q/%q", item.URL, item.FinalURL)
	}
	if item.Content != "hello world" {
		t.Fatalf("content = %q", item.Content)
	}
}

func TestDefaultHTTPClientIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("NO_PROXY", "*")

	client := newHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("default scrape HTTP client must ignore proxy environment variables")
	}
	if transport.DialContext == nil {
		t.Fatal("default scrape HTTP client must use a DNS-bound dialer")
	}
	if transport.TLSHandshakeTimeout != 0 || transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("fixed transport phase timeouts must not override per-URL timeout: tls=%s header=%s", transport.TLSHandshakeTimeout, transport.ResponseHeaderTimeout)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "phase timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func TestBuildInputResultsReportsExternalCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		cancel()
		<-req.Context().Done()
		return nil, req.Context().Err()
	})

	data, canceled := BuildInputResultsWithCancel(ctx, &Params{
		URLs:              []string{"https://8.8.8.8/slow"},
		Format:            FormatMarkdown,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
		TotalTimeoutSecs:  DefaultTotalTimeoutSeconds,
	})
	if !canceled {
		t.Fatalf("external cancellation flag = false, want true; data=%+v", data)
	}
	if len(data.Results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(data.Results))
	}
}

func TestPerURLTimeoutStartsWhenWorkerBeginsTask(t *testing.T) {
	withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/slow":
			time.Sleep(1100 * time.Millisecond)
			return response(http.StatusOK, map[string]string{"Content-Type": "text/plain"}, []byte("slow")), nil
		case "/fast":
			return response(http.StatusOK, map[string]string{"Content-Type": "text/plain"}, []byte("fast")), nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})

	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/slow", "https://8.8.8.8/fast"},
		Format:            FormatMarkdown,
		PerURLTimeoutSecs: 1,
		TotalTimeoutSecs:  5,
		Concurrency:       1,
	})
	if len(data.Results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(data.Results))
	}
	if !data.Results[1].Success || data.Results[1].Content != "fast" {
		t.Fatalf("queued second URL should get a fresh per-URL timeout when it starts: %+v", data.Results[1])
	}
}

func TestBuildInputResultsDeduplicatesIPv4MappedIPv6(t *testing.T) {
	var requests int32
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&requests, 1)
		return response(http.StatusOK, map[string]string{"Content-Type": "text/plain"}, []byte("ok")), nil
	})

	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/path", "https://[::ffff:8.8.8.8]/path"},
		Format:            FormatMarkdown,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
		TotalTimeoutSecs:  DefaultTotalTimeoutSeconds,
	})
	if len(data.Results) != 1 {
		t.Fatalf("len(results) = %d, want 1 deduplicated result", len(data.Results))
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
	}
}

func TestFetchURLFollowsAllowedRedirectAndBlocksUnsafeTarget(t *testing.T) {
	seen := 0
	withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		seen++
		switch req.URL.String() {
		case "https://8.8.8.8/start":
			return response(http.StatusFound, map[string]string{"Location": "https://1.1.1.1/final"}, nil), nil
		case "https://1.1.1.1/final":
			return response(http.StatusOK, map[string]string{"Content-Type": "text/plain; charset=utf-8"}, []byte("redirected")), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL.String())
			return nil, nil
		}
	})

	item := scrapeOne(t, "https://8.8.8.8/start")
	if !item.Success || item.FinalURL != "https://1.1.1.1/final" || item.Content != "redirected" {
		t.Fatalf("redirect result = %+v", item)
	}
	if seen != 2 {
		t.Fatalf("request count = %d, want 2", seen)
	}

	withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://8.8.8.8/start" {
			t.Fatalf("unsafe redirect target should be blocked before request, got %s", req.URL.String())
		}
		return response(http.StatusFound, map[string]string{"Location": "http://127.0.0.1/private"}, nil), nil
	})
	blocked := scrapeOne(t, "https://8.8.8.8/start")
	if blocked.Success || blocked.Error == nil || blocked.Error.Code != result.CodeSSRFBlocked {
		t.Fatalf("unsafe redirect should be SSRF_BLOCKED: %+v", blocked)
	}
	if blocked.FinalURL != "http://127.0.0.1/private" {
		t.Fatalf("unsafe redirect failure finalUrl = %q, want redirected target", blocked.FinalURL)
	}
}

func TestFetchURLFailureAfterRedirectKeepsCurrentFinalURL(t *testing.T) {
	withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case "https://8.8.8.8/start":
			return response(http.StatusFound, map[string]string{"Location": "https://1.1.1.1/missing"}, nil), nil
		case "https://1.1.1.1/missing":
			return response(http.StatusNotFound, nil, []byte("missing")), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL.String())
			return nil, nil
		}
	})

	item := scrapeOne(t, "https://8.8.8.8/start")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeHTTPStatusError {
		t.Fatalf("redirected 404 should fail with HTTP_STATUS_ERROR: %+v", item)
	}
	if item.URL != "https://8.8.8.8/start" || item.FinalURL != "https://1.1.1.1/missing" {
		t.Fatalf("url/finalUrl = %q/%q, want original/current", item.URL, item.FinalURL)
	}
}

func TestFetchURLFollowsProtocolRelativeIDNRedirect(t *testing.T) {
	origResolver := defaultResolver
	defaultResolver = fakeResolver{addrs: map[string][]netip.Addr{
		"xn--r8jz45g.xn--zckzah": {netip.MustParseAddr("93.184.216.34")},
	}}
	t.Cleanup(func() { defaultResolver = origResolver })

	withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case "https://8.8.8.8/start":
			return response(http.StatusFound, map[string]string{"Location": "//例え.テスト/path"}, nil), nil
		case "https://xn--r8jz45g.xn--zckzah/path":
			return response(http.StatusOK, map[string]string{"Content-Type": "text/plain"}, []byte("idn redirect")), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL.String())
			return nil, nil
		}
	})

	item := scrapeOne(t, "https://8.8.8.8/start")
	if !item.Success || item.Error != nil {
		t.Fatalf("protocol-relative IDN redirect should succeed: %+v", item)
	}
	if item.FinalURL != "https://xn--r8jz45g.xn--zckzah/path" {
		t.Fatalf("finalUrl = %q, want punycode redirect target", item.FinalURL)
	}
	if item.Content != "idn redirect" {
		t.Fatalf("content = %q", item.Content)
	}
}

func TestFetchURLMapsRedirectLocationErrors(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		wantCode string
	}{
		{
			name:     "missing location",
			headers:  nil,
			wantCode: result.CodeHTTPStatusError,
		},
		{
			name:     "empty location",
			headers:  map[string]string{"Location": "   "},
			wantCode: result.CodeHTTPStatusError,
		},
		{
			name:     "unsupported scheme location",
			headers:  map[string]string{"Location": "ftp://example.com/file"},
			wantCode: result.CodeInvalidURL,
		},
		{
			name:     "raw space location",
			headers:  map[string]string{"Location": "/a b"},
			wantCode: result.CodeInvalidURL,
		},
		{
			name:     "raw angle bracket location",
			headers:  map[string]string{"Location": "/a<b"},
			wantCode: result.CodeInvalidURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != "https://8.8.8.8/start" {
					t.Fatalf("unexpected URL %s", req.URL.String())
				}
				return response(http.StatusFound, tt.headers, nil), nil
			})

			item := scrapeOne(t, "https://8.8.8.8/start")
			if item.Success || item.Error == nil || item.Error.Code != tt.wantCode {
				t.Fatalf("redirect error code = %+v, want %s", item, tt.wantCode)
			}
		})
	}
}

func TestFetchURLDetectsRedirectLoopAndLimit(t *testing.T) {
	t.Run("loop", func(t *testing.T) {
		withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://8.8.8.8/a":
				return response(http.StatusFound, map[string]string{"Location": "https://1.1.1.1/b"}, nil), nil
			case "https://1.1.1.1/b":
				return response(http.StatusFound, map[string]string{"Location": "https://8.8.8.8/a"}, nil), nil
			default:
				t.Fatalf("unexpected URL %s", req.URL.String())
				return nil, nil
			}
		})

		item := scrapeOne(t, "https://8.8.8.8/a")
		if item.Success || item.Error == nil || item.Error.Code != result.CodeTooManyRedirects {
			t.Fatalf("redirect loop should be TOO_MANY_REDIRECTS: %+v", item)
		}
	})

	t.Run("limit", func(t *testing.T) {
		var seen int
		withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
			seen++
			path := strings.TrimPrefix(req.URL.Path, "/")
			return response(http.StatusFound, map[string]string{"Location": "/" + path + "x"}, nil), nil
		})

		item := scrapeOne(t, "https://8.8.8.8/start")
		if item.Success || item.Error == nil || item.Error.Code != result.CodeTooManyRedirects {
			t.Fatalf("redirect limit should be TOO_MANY_REDIRECTS: %+v", item)
		}
		if seen != maxRedirects+1 {
			t.Fatalf("request count = %d, want %d", seen, maxRedirects+1)
		}
	})
}

func TestFetchURLMapsHTTPAndEncodingErrors(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusNotFound, nil, []byte("missing")), nil
	})
	item := scrapeOne(t, "https://8.8.8.8/missing")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeHTTPStatusError {
		t.Fatalf("404 should map to HTTP_STATUS_ERROR: %+v", item)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Encoding": "gzip, br", "Content-Type": "text/plain"}, []byte("bad")), nil
	})
	item = scrapeOne(t, "https://8.8.8.8/encoded")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeUnsupportedContentEncoding {
		t.Fatalf("multi encoding should map to UNSUPPORTED_CONTENT_ENCODING: %+v", item)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		resp := response(http.StatusOK, map[string]string{"Content-Encoding": "gzip", "Content-Type": "text/plain"}, []byte("bad"))
		resp.Header.Add("Content-Encoding", "br")
		return resp, nil
	})
	item = scrapeOne(t, "https://8.8.8.8/repeated-encoding")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeUnsupportedContentEncoding {
		t.Fatalf("repeated encoding should map to UNSUPPORTED_CONTENT_ENCODING: %+v", item)
	}
}

func TestFetchURLReadsGzipAndEnforcesSizeLimit(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("compressed hello"))
	_ = zw.Close()

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Encoding": "gzip", "Content-Type": "text/plain"}, gz.Bytes()), nil
	})
	item := scrapeOne(t, "https://8.8.8.8/gzip")
	if !item.Success || item.Content != "compressed hello" {
		t.Fatalf("gzip result = %+v", item)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		resp := response(http.StatusOK, map[string]string{"Content-Type": "text/plain"}, nil)
		resp.ContentLength = maxResponseBytes + 1
		return resp, nil
	})
	item = scrapeOne(t, "https://8.8.8.8/large")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeResponseTooLarge {
		t.Fatalf("oversized response should be RESPONSE_TOO_LARGE: %+v", item)
	}

	var hugeGzip bytes.Buffer
	hzw := gzip.NewWriter(&hugeGzip)
	_, _ = hzw.Write(bytes.Repeat([]byte("a"), maxResponseBytes+1))
	_ = hzw.Close()
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Encoding": "gzip", "Content-Type": "text/plain"}, hugeGzip.Bytes()), nil
	})
	item = scrapeOne(t, "https://8.8.8.8/decompressed-large")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeResponseTooLarge {
		t.Fatalf("decompressed oversized response should be RESPONSE_TOO_LARGE: %+v", item)
	}
}

func TestFetchURLRejectsRepeatedGzipMembers(t *testing.T) {
	// A single Content-Encoding: gzip header whose body concatenates more than one
	// gzip member is a repeated encoding and must be rejected (§5.3), even though
	// Go's gzip reader would otherwise transparently decode it as one stream.
	var gz bytes.Buffer
	for _, part := range []string{"hello ", "world"} {
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write([]byte(part))
		_ = zw.Close()
	}
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Encoding": "gzip", "Content-Type": "text/plain"}, gz.Bytes()), nil
	})
	item := scrapeOne(t, "https://8.8.8.8/repeated-gzip")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeUnsupportedContentEncoding {
		t.Fatalf("repeated gzip members should be UNSUPPORTED_CONTENT_ENCODING: %+v", item)
	}
}

func TestFetchURLReadsBrotli(t *testing.T) {
	var br bytes.Buffer
	bw := brotli.NewWriter(&br)
	_, _ = bw.Write([]byte("brotli hello"))
	_ = bw.Close()

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Encoding": "br", "Content-Type": "text/plain"}, br.Bytes()), nil
	})
	item := scrapeOne(t, "https://8.8.8.8/br")
	if !item.Success || item.Content != "brotli hello" || item.Metadata["contentEncoding"] != "br" {
		t.Fatalf("brotli result = %+v", item)
	}
}

func TestFetchURLTimeout(t *testing.T) {
	withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})

	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/slow"},
		Format:            FormatMarkdown,
		PerURLTimeoutSecs: 1,
	})
	if len(data.Results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(data.Results))
	}
	item := data.Results[0]
	if item.Success || item.Error == nil || item.Error.Code != result.CodeScrapeTimeout {
		t.Fatalf("timeout should be SCRAPE_TIMEOUT: %+v", item)
	}
	if !strings.Contains(strings.ToLower(item.Error.Message), "timed") {
		t.Fatalf("timeout message = %q", item.Error.Message)
	}
}

func TestFetchURLMapsTransportTimeoutToScrapeTimeout(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return nil, timeoutErr{}
	})
	item := scrapeOne(t, "https://8.8.8.8/timeout")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeScrapeTimeout {
		t.Fatalf("transport timeout should be SCRAPE_TIMEOUT: %+v", item)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.Join(timeoutErr{}, errors.New("wrapped"))
	})
	item = scrapeOne(t, "https://8.8.8.8/timeout-wrapped")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeScrapeTimeout {
		t.Fatalf("wrapped transport timeout should be SCRAPE_TIMEOUT: %+v", item)
	}
}

func TestBuildInputResultsRunsBatchWithBoundedConcurrencyAndKeepsOrder(t *testing.T) {
	var active int32
	var maxActive int32
	withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		cur := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if cur <= old || atomic.CompareAndSwapInt32(&maxActive, old, cur) {
				break
			}
		}
		defer atomic.AddInt32(&active, -1)

		time.Sleep(50 * time.Millisecond)
		return response(http.StatusOK, map[string]string{"Content-Type": "text/plain"}, []byte(req.URL.Path)), nil
	})

	data := BuildInputResults(context.Background(), &Params{
		URLs: []string{
			"https://8.8.8.8/first",
			"https://8.8.4.4/second",
			"https://1.1.1.1/third",
		},
		Format:            FormatMarkdown,
		PerURLTimeoutSecs: 5,
		TotalTimeoutSecs:  5,
		Concurrency:       2,
	})
	if len(data.Results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(data.Results))
	}
	if got := atomic.LoadInt32(&maxActive); got != 2 {
		t.Fatalf("max concurrent requests = %d, want 2", got)
	}
	for i, want := range []string{"/first", "/second", "/third"} {
		item := data.Results[i]
		if !item.Success || item.Content != want {
			t.Fatalf("result[%d] = %+v, want success content %q", i, item, want)
		}
	}
}

func TestFetchURLProcessesContentTypesAndFormats(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "text/html; charset=utf-8"}, []byte(`<!doctype html><html><head><title>Example</title></head><body><nav>menu</nav><main><h1>Hello</h1><p>Readable <a href="https://example.com">link</a>.</p></main></body></html>`)), nil
	})
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/html"},
		Format:            FormatMarkdown,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item := data.Results[0]
	if !item.Success || item.Title != "Example" || !strings.Contains(item.Content, "# Hello") || !strings.Contains(item.Content, "[link](https://example.com)") {
		t.Fatalf("HTML markdown result = %+v", item)
	}
	if item.Metadata["mainContentExtracted"] != true || item.Metadata["fallbackUsed"] != false {
		t.Fatalf("HTML metadata = %+v", item.Metadata)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "text/markdown"}, []byte("# Title\n\nHello **world** [site](https://example.com)")), nil
	})
	data = BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/md"},
		Format:            FormatText,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item = data.Results[0]
	if !item.Success || !strings.Contains(item.Content, "Title") || !strings.Contains(item.Content, "Hello world site") {
		t.Fatalf("markdown text result = %+v", item)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "text/plain"}, append([]byte{0xEF, 0xBB, 0xBF}, []byte("<hello>")...)), nil
	})
	data = BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/plain"},
		Format:            FormatHTML,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item = data.Results[0]
	if !item.Success || item.Content != "&lt;hello&gt;" {
		t.Fatalf("plain html result = %+v", item)
	}
}

func TestFetchURLEscapesPlainTextForMarkdownOutput(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		body := `<img src="https://tracker.example/pixel">
[x](javascript:alert(1))
![pixel](https://tracker.example/pixel)`
		return response(http.StatusOK, map[string]string{"Content-Type": "text/plain; charset=utf-8"}, []byte(body)), nil
	})
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/plain-md-text"},
		Format:            FormatMarkdown,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item := data.Results[0]
	if !item.Success {
		t.Fatalf("plain markdown result should succeed: %+v", item)
	}
	if strings.Contains(item.Content, "<img") || strings.Contains(item.Content, "[x](javascript:") || strings.Contains(item.Content, "![pixel](https://") {
		t.Fatalf("plain text markdown output should neutralize markdown/html injection: %s", item.Content)
	}
	if !strings.Contains(item.Content, `&lt;img src="https://tracker.example/pixel"&gt;`) || !strings.Contains(item.Content, `\[x\]\(javascript:alert\(1\)\)`) || !strings.Contains(item.Content, `\!\[pixel\]\(https://tracker.example/pixel\)`) {
		t.Fatalf("plain text markdown output should keep escaped source text: %s", item.Content)
	}
}

func TestFetchURLRejectsUnsupportedContentTypeBeforeCharset(t *testing.T) {
	for _, contentType := range []string{"application/pdf", "application/json"} {
		t.Run(contentType, func(t *testing.T) {
			withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, map[string]string{"Content-Type": contentType}, []byte{0xff, 0xfe, 0x00}), nil
			})
			item := scrapeOne(t, "https://8.8.8.8/unsupported")
			if item.Success || item.Error == nil || item.Error.Code != result.CodeUnsupportedContentType {
				t.Fatalf("unsupported %s should be UNSUPPORTED_CONTENT_TYPE before charset validation: %+v", contentType, item)
			}
		})
	}
}

func TestFetchURLMainContentFallbackAndDisable(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "text/html; charset=utf-8"}, []byte(`<html><body><nav>menu</nav><main><h1>Main</h1></main><p>tail</p></body></html>`)), nil
	})
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/no-main-content"},
		Format:            FormatText,
		MainContent:       false,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item := data.Results[0]
	if !item.Success || !strings.Contains(item.Content, "menu") || !strings.Contains(item.Content, "tail") {
		t.Fatalf("disabled main-content extraction should keep full body: %+v", item)
	}
	if item.Metadata["mainContentExtracted"] != false || item.Metadata["fallbackUsed"] != false {
		t.Fatalf("disabled main-content metadata = %+v", item.Metadata)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "text/html; charset=utf-8"}, []byte(`<html><body><nav>menu</nav><p>body text</p></body></html>`)), nil
	})
	data = BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/fallback"},
		Format:            FormatText,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item = data.Results[0]
	if !item.Success || !strings.Contains(item.Content, "body text") {
		t.Fatalf("missing main/article should fall back to body: %+v", item)
	}
	if item.Metadata["mainContentExtracted"] != false || item.Metadata["fallbackUsed"] != true {
		t.Fatalf("fallback metadata = %+v", item.Metadata)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "text/html; charset=utf-8"}, []byte(`<html><body><main>   </main><p>fallback text</p></body></html>`)), nil
	})
	data = BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/empty-main"},
		Format:            FormatText,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item = data.Results[0]
	if !item.Success || !strings.Contains(item.Content, "fallback text") || item.Metadata["fallbackUsed"] != true {
		t.Fatalf("empty main should fall back to body: %+v", item)
	}
}

func TestFetchURLHTMLEmptyAndConversionError(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "text/html; charset=utf-8"}, []byte(`<html><body><main>   </main></body></html>`)), nil
	})
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/empty-html"},
		Format:            FormatText,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item := data.Results[0]
	if item.Success || item.Error == nil || item.Error.Code != result.CodeEmptyContent {
		t.Fatalf("empty HTML should be EMPTY_CONTENT: %+v", item)
	}

	orig := renderHTMLNode
	renderHTMLNode = func(io.Writer, *nethtml.Node) error { return errors.New("render failed") }
	t.Cleanup(func() { renderHTMLNode = orig })
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "text/html; charset=utf-8"}, []byte(`<html><body><p>hello</p></body></html>`)), nil
	})
	data = BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/conversion-error"},
		Format:            FormatHTML,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item = data.Results[0]
	if item.Success || item.Error == nil || item.Error.Code != result.CodeConversionError {
		t.Fatalf("HTML render failure should be CONVERSION_ERROR: %+v", item)
	}
}

func TestFetchURLBoundsTitleAndContentTypeMetadata(t *testing.T) {
	longTitle := strings.Repeat("题", scrapeTitleMaxBytes)
	longContentType := "text/html; charset=utf-8; x=" + strings.Repeat("a", contentTypeMaxBytes)
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		body := `<!doctype html><html><head><title>` + longTitle + `</title></head><body><main><p>ok</p></main></body></html>`
		return response(http.StatusOK, map[string]string{"Content-Type": longContentType}, []byte(body)), nil
	})

	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/bounded"},
		Format:            FormatMarkdown,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item := data.Results[0]
	if !item.Success {
		t.Fatalf("bounded metadata result should succeed: %+v", item)
	}
	if len(item.Title) > scrapeTitleMaxBytes || item.Title == longTitle || !item.TitleTruncated {
		t.Fatalf("title was not bounded/truncated: len=%d truncated=%v", len(item.Title), item.TitleTruncated)
	}
	if got, _ := item.Metadata["contentType"].(string); len(got) > contentTypeMaxBytes || got == longContentType {
		t.Fatalf("contentType was not bounded: len=%d", len(got))
	}
	if item.Metadata["contentTypeTruncated"] != true || item.Metadata["titleTruncated"] != true {
		t.Fatalf("truncation markers missing: %+v", item.Metadata)
	}
}

func TestProcessContentReturnsScrapeTimeoutWhenContextCanceledBeforeConversion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, _, err := processContent(ctx, []byte(`<html><body><main>late</main></body></html>`), "text/html", FormatMarkdown, true)
	if itemErrorCode(err) != result.CodeScrapeTimeout {
		t.Fatalf("canceled conversion should map to SCRAPE_TIMEOUT, got %v", err)
	}
}

func TestScrapeSummaryPreservesMetadata(t *testing.T) {
	count := 1
	env := result.NewSuccess(result.CommandPtr(result.CommandScrape), Data{Results: []Result{{Success: true, URL: "https://example.com", FinalURL: "https://example.com", Content: "large"}}})
	env.Metadata.DurationMs = 123
	env.Metadata.ResultCount = &count

	summary := SummarizeEnvelope(env)
	if summary.Metadata.DurationMs != 123 || summary.Metadata.ResultCount == nil || *summary.Metadata.ResultCount != 1 {
		t.Fatalf("summary metadata was not preserved: %+v", summary.Metadata)
	}
	data := summary.Data.(Data)
	if data.Results[0].Content != "" {
		t.Fatalf("summary should omit content, got %q", data.Results[0].Content)
	}
}

func TestFetchURLSanitizesHTMLForHTMLOutput(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		body := `<!doctype html><html><head><title>Unsafe</title><base href="https://evil.example/"></head><body><main><h1 style="color:red" onclick="steal()">Hello</h1><script>alert(1)</script><iframe srcdoc="<script>alert(1)</script>"></iframe><form action="https://evil.example"><input name="token"><button>send</button></form><img src="https://tracker.example/pixel" onerror="steal()"><a href="javascript:alert(1)" onclick="steal()" style="color:red">bad</a><a href="https://example.com/path" title="safe">good</a></main></body></html>`
		return response(http.StatusOK, map[string]string{"Content-Type": "text/html; charset=utf-8"}, []byte(body)), nil
	})
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/html"},
		Format:            FormatHTML,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item := data.Results[0]
	if !item.Success || item.Title != "Unsafe" {
		t.Fatalf("HTML result should succeed with title: %+v", item)
	}
	for _, banned := range []string{"script", "iframe", "srcdoc", "form", "input", "button", "img", "onclick", "style=", "javascript:", "tracker.example", "evil.example"} {
		if strings.Contains(strings.ToLower(item.Content), banned) {
			t.Fatalf("HTML output contains banned %q: %s", banned, item.Content)
		}
	}
	if !strings.Contains(item.Content, "bad") || !strings.Contains(item.Content, `href="https://example.com/path"`) || !strings.Contains(item.Content, "good") {
		t.Fatalf("HTML output should preserve safe text and links: %s", item.Content)
	}
}

func TestFetchURLSanitizesHTMLLinksForMarkdownOutput(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		body := `<html><body><main><p><a href="javascript:alert(1)">bad</a> <a href="data:text/html,evil">data</a> <a href="//evil.com/x">netpath</a> <a href="/foo) [bad](javascript:alert(1))">relative</a> <a href="https://example.com">good</a></p></main></body></html>`
		return response(http.StatusOK, map[string]string{"Content-Type": "text/html"}, []byte(body)), nil
	})
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/html-md"},
		Format:            FormatMarkdown,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item := data.Results[0]
	if !item.Success {
		t.Fatalf("markdown result should succeed: %+v", item)
	}
	if strings.Contains(strings.ToLower(item.Content), "javascript:") || strings.Contains(strings.ToLower(item.Content), "data:") {
		t.Fatalf("markdown output should drop dangerous link protocols: %s", item.Content)
	}
	// Scheme-relative URLs (`//host/path`) target an arbitrary external host and
	// must be downgraded to plain text, not preserved as a Markdown link.
	if strings.Contains(item.Content, "evil.com") || strings.Contains(item.Content, "[netpath]") {
		t.Fatalf("markdown output should downgrade scheme-relative links: %s", item.Content)
	}
	if !strings.Contains(item.Content, "netpath") {
		t.Fatalf("markdown output should keep scheme-relative link label as text: %s", item.Content)
	}
	if !strings.Contains(item.Content, "bad") || !strings.Contains(item.Content, "data") || !strings.Contains(item.Content, "[relative](/foo%29%20%5Bbad%5D%28javascript%3Aalert%281%29%29)") || !strings.Contains(item.Content, "[good](https://example.com)") {
		t.Fatalf("markdown output should keep labels and safe links: %s", item.Content)
	}
}

func TestFetchURLEscapesHTMLTextForMarkdownOutput(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		body := `<html><body><main><p>&lt;img src="https://tracker.example/pixel"&gt;</p><p>[x](javascript:alert(1))</p><p>![pixel](https://tracker.example/pixel)</p></main></body></html>`
		return response(http.StatusOK, map[string]string{"Content-Type": "text/html"}, []byte(body)), nil
	})
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/html-md-text"},
		Format:            FormatMarkdown,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item := data.Results[0]
	if !item.Success {
		t.Fatalf("markdown result should succeed: %+v", item)
	}
	for _, banned := range []string{"<img", "[x](javascript", "![pixel]"} {
		if strings.Contains(item.Content, banned) {
			t.Fatalf("markdown text output contains active syntax %q: %s", banned, item.Content)
		}
	}
	for _, want := range []string{`&lt;img`, `\[x\]\(javascript:alert\(1\)\)`, `\!\[pixel\]\(https://tracker.example/pixel\)`} {
		if !strings.Contains(item.Content, want) {
			t.Fatalf("markdown text output missing escaped text %q: %s", want, item.Content)
		}
	}
}

func TestFetchURLSanitizesMarkdownInput(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		body := "# Title\n\n<script>alert(1)</script>\n<img src=\"https://tracker.example/pixel\" onerror=\"steal()\">\n![pixel](https://tracker.example/pixel)\n![ref pixel][img]\n[img]: https://tracker.example/pixel\n[bad](javascript:alert(1)) [entity][x] [good](https://example.com)\n[x]: javascript&#58;alert(1)"
		return response(http.StatusOK, map[string]string{"Content-Type": "text/markdown"}, []byte(body)), nil
	})
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/md"},
		Format:            FormatMarkdown,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item := data.Results[0]
	if !item.Success {
		t.Fatalf("markdown result should succeed: %+v", item)
	}
	for _, banned := range []string{"script", "alert", "img", "onerror", "javascript:", "tracker.example", "!["} {
		if strings.Contains(strings.ToLower(item.Content), banned) {
			t.Fatalf("markdown output contains banned %q: %s", banned, item.Content)
		}
	}
	if !strings.Contains(item.Content, "# Title") || !strings.Contains(item.Content, "bad") || !strings.Contains(item.Content, "entity") || !strings.Contains(item.Content, "[good](https://example.com)") {
		t.Fatalf("markdown output should keep safe text and links: %s", item.Content)
	}
}

func TestFetchURLConvertsMarkdownToHTML(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		body := "# Title\n\nHello **world** with [Go](https://go.dev) and [bad](javascript:alert(1)) and `code`.\n\n- one\n- two\n\n```\n<raw>\n```"
		return response(http.StatusOK, map[string]string{"Content-Type": "text/markdown"}, []byte(body)), nil
	})
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/md-html"},
		Format:            FormatHTML,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item := data.Results[0]
	if !item.Success {
		t.Fatalf("markdown html result should succeed: %+v", item)
	}
	for _, want := range []string{"<h1>Title</h1>", "<strong>world</strong>", `<a href="https://go.dev">Go</a>`, "<ul>", "<li>one</li>", "<code>code</code>", "&lt;raw&gt;"} {
		if !strings.Contains(item.Content, want) {
			t.Fatalf("markdown HTML missing %q: %s", want, item.Content)
		}
	}
	for _, banned := range []string{"javascript:", "alert(1)"} {
		if strings.Contains(strings.ToLower(item.Content), banned) {
			t.Fatalf("markdown HTML should be sanitized after conversion, found %q: %s", banned, item.Content)
		}
	}
}

func TestFetchURLPreservesBalancedParenLinkDestinations(t *testing.T) {
	// 回归：markdown 内联链接 / 图片的 destination 含平衡括号时（如维基百科
	// .../Go_(programming_language)）不得在第一个 ')' 处误截断或泄露游离 ')'。
	body := "See [Go](https://en.wikipedia.org/wiki/Go_(programming_language)) for details."
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "text/markdown"}, []byte(body)), nil
	})
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/paren"},
		Format:            FormatMarkdown,
		MainContent:       true,
		PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds,
	})
	item := data.Results[0]
	if !item.Success {
		t.Fatalf("markdown paren result should succeed: %+v", item)
	}
	want := "[Go](https://en.wikipedia.org/wiki/Go_%28programming_language%29)"
	if !strings.Contains(item.Content, want) {
		t.Fatalf("balanced-paren destination was mangled, want %q in: %s", want, item.Content)
	}
	if strings.Contains(item.Content, "language)) ") || strings.Contains(item.Content, "language) for") {
		t.Fatalf("stray ')' leaked into output: %s", item.Content)
	}
}

func TestFetchURLRejectsUnsupportedContentAndCharset(t *testing.T) {
	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, nil, []byte("hello")), nil
	})
	item := scrapeOne(t, "https://8.8.8.8/no-content-type")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeUnsupportedContentType {
		t.Fatalf("missing content type should be UNSUPPORTED_CONTENT_TYPE: %+v", item)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "text/plain; charset=iso-8859-1"}, []byte("hello")), nil
	})
	item = scrapeOne(t, "https://8.8.8.8/bad-charset")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeUnsupportedCharset {
		t.Fatalf("bad charset should be UNSUPPORTED_CHARSET: %+v", item)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "application/json"}, []byte(`{"ok":true}`)), nil
	})
	item = scrapeOne(t, "https://8.8.8.8/json")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeUnsupportedContentType {
		t.Fatalf("json should be UNSUPPORTED_CONTENT_TYPE: %+v", item)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		resp := response(http.StatusOK, map[string]string{"Content-Type": "application/json"}, []byte(`{"ok":true}`))
		resp.ContentLength = maxResponseBytes + 1
		return resp, nil
	})
	item = scrapeOne(t, "https://8.8.8.8/huge-json")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeUnsupportedContentType {
		t.Fatalf("unsupported content type should be rejected before size checks: %+v", item)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "application/json", "Content-Encoding": "gzip"}, []byte("not-gzip")), nil
	})
	item = scrapeOne(t, "https://8.8.8.8/bad-gzip-json")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeUnsupportedContentType {
		t.Fatalf("unsupported content type should be rejected before decoding: %+v", item)
	}

	withFakeHTTPClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, map[string]string{"Content-Type": "text/plain"}, []byte("   \n\t  ")), nil
	})
	item = scrapeOne(t, "https://8.8.8.8/empty")
	if item.Success || item.Error == nil || item.Error.Code != result.CodeEmptyContent {
		t.Fatalf("empty content should be EMPTY_CONTENT: %+v", item)
	}
}

func TestPerURLTimeoutWinsWhenItFiresBeforeBatchTimeout(t *testing.T) {
	withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		time.Sleep(1500 * time.Millisecond)
		return nil, req.Context().Err()
	})

	start := time.Now()
	data := BuildInputResults(context.Background(), &Params{
		URLs:              []string{"https://8.8.8.8/slow"},
		Format:            FormatMarkdown,
		PerURLTimeoutSecs: 1,
		TotalTimeoutSecs:  2,
		Concurrency:       1,
	})
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("test did not reach the batch timeout window: %s", elapsed)
	}
	if len(data.Results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(data.Results))
	}
	item := data.Results[0]
	if item.Success || item.Error == nil || item.Error.Code != result.CodeScrapeTimeout {
		t.Fatalf("per-URL timeout should win when it fires first: %+v", item)
	}
}

func TestBuildInputResultsMarksUnfinishedItemsAsTaskTimeout(t *testing.T) {
	withFakeHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})

	start := time.Now()
	data := BuildInputResults(context.Background(), &Params{
		URLs: []string{
			"https://8.8.8.8/one",
			"https://8.8.4.4/two",
			"https://1.1.1.1/three",
		},
		Format:            FormatMarkdown,
		PerURLTimeoutSecs: 5,
		TotalTimeoutSecs:  1,
		Concurrency:       2,
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("batch timeout returned too late: %s", elapsed)
	}
	if len(data.Results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(data.Results))
	}
	for i, item := range data.Results {
		if item.Success || item.Error == nil || item.Error.Code != result.CodeTaskTimeout {
			t.Fatalf("result[%d] should be TASK_TIMEOUT: %+v", i, item)
		}
	}
}
