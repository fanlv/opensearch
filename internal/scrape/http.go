package scrape

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/fanlv/opensearch/internal/result"
	"github.com/fanlv/opensearch/internal/urlnorm"
)

const (
	maxResponseBytes = 5 * 1024 * 1024
	maxRedirects     = 5
)

var newHTTPClient = func() *http.Client {
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			// DialContext is a placeholder: fetchURL always rebinds it to the
			// request's validated address set via bindHTTPClientToResolvedTarget
			// before any dial happens, so no connection uses this dialer directly.
			DialContext:           dialer.DialContext,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func fetchURL(ctx context.Context, start *urlnorm.Normalized, p *Params) (Result, error) {
	current := start
	visited := map[string]struct{}{}

	for redirects := 0; ; redirects++ {
		if redirects > maxRedirects {
			return Result{}, &targetError{code: result.CodeTooManyRedirects, msg: "too many redirects", finalURL: current.ForDedup}
		}
		if _, ok := visited[current.ForDedup]; ok {
			return Result{}, &targetError{code: result.CodeTooManyRedirects, msg: "redirect loop detected", finalURL: current.ForDedup}
		}
		visited[current.ForDedup] = struct{}{}

		resolved, err := validatePublicTarget(ctx, current, defaultResolver)
		if err != nil {
			return Result{}, withFinalURL(err, current.ForDedup)
		}
		client := newHTTPClient()
		bindHTTPClientToResolvedTarget(client, resolved)

		resp, err := anonymousGET(ctx, client, current.ForDedup, p.UserAgent)
		if err != nil {
			return Result{}, withFinalURL(fetchError(err), current.ForDedup)
		}

		if isRedirectStatus(resp.StatusCode) {
			next, err := redirectTarget(current.ForDedup, resp.Header.Get("Location"))
			resp.Body.Close()
			if err != nil {
				return Result{}, withFinalURL(err, current.ForDedup)
			}
			current = next
			continue
		}

		if resp.StatusCode >= 300 || resp.StatusCode < 200 {
			resp.Body.Close()
			return Result{}, &targetError{code: result.CodeHTTPStatusError, msg: fmt.Sprintf("HTTP status %d", resp.StatusCode), finalURL: current.ForDedup}
		}

		content, title, titleTruncated, meta, err := readResponseBody(ctx, resp, p)
		if err != nil {
			return Result{}, withFinalURL(err, current.ForDedup)
		}

		return Result{
			Success:        true,
			URL:            start.ForDedup,
			FinalURL:       current.ForDedup,
			Title:          title,
			TitleTruncated: titleTruncated,
			Format:         p.Format,
			Content:        content,
			Metadata:       meta,
			Error:          nil,
		}, nil
	}
}

func bindHTTPClientToResolvedTarget(client *http.Client, target *ResolvedTarget) {
	if client == nil || target == nil {
		return
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		return
	}
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	clone := transport.Clone()
	clone.DialContext = resolvedTargetDialContext(target, dialer.DialContext)
	client.Transport = clone
}

func anonymousGET(ctx context.Context, client *http.Client, target, userAgent string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	req.Header.Set("Accept-Encoding", "gzip, br")
	return client.Do(req)
}

func isRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func redirectTarget(baseRaw, location string) (*urlnorm.Normalized, error) {
	if strings.TrimSpace(location) == "" {
		return nil, &targetError{code: result.CodeHTTPStatusError, msg: "redirect location is missing"}
	}
	if err := urlnorm.ValidateRawInput(location); err != nil {
		return nil, &targetError{code: result.CodeInvalidURL, msg: "invalid redirect location"}
	}
	base, err := url.Parse(baseRaw)
	if err != nil {
		return nil, &targetError{code: result.CodeInvalidURL, msg: "invalid redirect base URL"}
	}
	loc, err := url.Parse(location)
	if err != nil {
		return nil, &targetError{code: result.CodeInvalidURL, msg: "invalid redirect location"}
	}
	next := base.ResolveReference(loc).String()
	norm, err := urlnorm.Normalize(next)
	if err != nil {
		return nil, &targetError{code: result.CodeInvalidURL, msg: "invalid redirect location"}
	}
	return norm, nil
}

func readResponseBody(ctx context.Context, resp *http.Response, p *Params) (string, string, bool, map[string]interface{}, error) {
	defer resp.Body.Close()
	if _, _, err := parseSupportedContentType(resp.Header.Get("Content-Type")); err != nil {
		return "", "", false, nil, err
	}
	if resp.ContentLength > maxResponseBytes {
		return "", "", false, nil, &targetError{code: result.CodeResponseTooLarge, msg: "response is too large"}
	}

	body, encoding, err := decodingReader(resp.Body, resp.Header.Values("Content-Encoding"))
	if err != nil {
		return "", "", false, nil, err
	}
	if closer, ok := body.(io.Closer); ok {
		defer closer.Close()
	}

	limited := io.LimitReader(body, maxResponseBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return "", "", false, nil, fetchError(err)
	}
	if len(b) > maxResponseBytes {
		return "", "", false, nil, &targetError{code: result.CodeResponseTooLarge, msg: "response is too large"}
	}
	if err := ctx.Err(); err != nil {
		return "", "", false, nil, fetchError(err)
	}
	contentType, contentTypeTruncated := truncateUTF8Bytes(resp.Header.Get("Content-Type"), contentTypeMaxBytes)

	meta := map[string]interface{}{
		"statusCode":      resp.StatusCode,
		"contentType":     contentType,
		"contentEncoding": encoding,
		"bytes":           len(b),
	}
	if contentTypeTruncated {
		meta["contentTypeTruncated"] = true
	}
	content, title, titleTruncated, contentMeta, err := processContent(ctx, b, resp.Header.Get("Content-Type"), p.Format, p.MainContent)
	if err != nil {
		return "", "", false, nil, err
	}
	for k, v := range contentMeta {
		meta[k] = v
	}
	return content, title, titleTruncated, meta, nil
}

func decodingReader(body io.Reader, headers []string) (io.Reader, string, error) {
	encodings := make([]string, 0, len(headers))
	for _, header := range headers {
		for _, part := range strings.Split(header, ",") {
			encoding := strings.TrimSpace(strings.ToLower(part))
			if encoding != "" {
				encodings = append(encodings, encoding)
			}
		}
	}
	if len(encodings) > 1 {
		return nil, "", &targetError{code: result.CodeUnsupportedContentEncoding, msg: "multiple content encodings are not supported"}
	}
	encoding := ""
	if len(encodings) == 1 {
		encoding = encodings[0]
	}
	if encoding == "" || encoding == "identity" {
		return body, "identity", nil
	}
	switch encoding {
	case "gzip":
		// gzip.NewReader wraps a plain io.Reader in an internal bufio.Reader that
		// reads ahead past the first member's boundary, which would make the
		// repeated-member probe below unreliable. Passing an io.ByteReader makes
		// gzip consume the source without that read-ahead, so Reset can still see a
		// trailing member.
		src := &byteReader{r: body}
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, "", &targetError{code: result.CodeNetworkError, msg: "invalid gzip response"}
		}
		// Only a single gzip member is supported. Disabling multistream makes the
		// reader stop at the first member's boundary; singleMemberGzipReader then
		// rejects any trailing member as a repeated encoding (§5.3).
		zr.Multistream(false)
		return &singleMemberGzipReader{zr: zr, src: src}, encoding, nil
	case "br":
		return brotli.NewReader(body), encoding, nil
	default:
		return nil, "", &targetError{code: result.CodeUnsupportedContentEncoding, msg: "unsupported content encoding"}
	}
}

// byteReader adapts an io.Reader into an io.ByteReader without buffering, so a
// gzip.Reader consuming it does not read ahead past a member boundary.
type byteReader struct {
	r   io.Reader
	buf [1]byte
}

func (b *byteReader) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *byteReader) ReadByte() (byte, error) {
	n, err := io.ReadFull(b.r, b.buf[:])
	if n == 1 {
		return b.buf[0], nil
	}
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return 0, err
}

// singleMemberGzipReader wraps a non-multistream gzip.Reader and rejects bodies
// that concatenate more than one gzip member. After the first member reaches
// EOF, Reset against the underlying source succeeds only when another member
// follows; that case is reported as UNSUPPORTED_CONTENT_ENCODING to match the
// single-layer gzip contract (§5.3).
type singleMemberGzipReader struct {
	zr  *gzip.Reader
	src io.Reader
}

func (r *singleMemberGzipReader) Read(p []byte) (int, error) {
	n, err := r.zr.Read(p)
	if errors.Is(err, io.EOF) {
		// First member finished. If another member follows, Reset returns nil and
		// the body is a repeated gzip; treat it as an unsupported encoding.
		if resetErr := r.zr.Reset(r.src); resetErr == nil {
			return n, &targetError{code: result.CodeUnsupportedContentEncoding, msg: "repeated gzip members are not supported"}
		}
		return n, io.EOF
	}
	return n, err
}

func (r *singleMemberGzipReader) Close() error { return r.zr.Close() }

func fetchError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &targetError{code: result.CodeScrapeTimeout, msg: "scrape timed out"}
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return &targetError{code: result.CodeScrapeTimeout, msg: "scrape timed out"}
	}
	if errors.Is(err, context.Canceled) {
		return &targetError{code: result.CodeScrapeTimeout, msg: "scrape canceled"}
	}
	if isTimeoutError(err) {
		return &targetError{code: result.CodeScrapeTimeout, msg: "scrape timed out"}
	}
	var te *targetError
	if errors.As(err, &te) {
		return te
	}
	return &targetError{code: result.CodeNetworkError, msg: "network error"}
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok && isTimeoutError(u.Unwrap()) {
		return true
	}
	type multiUnwrapper interface{ Unwrap() []error }
	if u, ok := err.(multiUnwrapper); ok {
		for _, child := range u.Unwrap() {
			if isTimeoutError(child) {
				return true
			}
		}
	}
	return false
}
