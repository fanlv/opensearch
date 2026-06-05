package scrape

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/fanlv/opensearch/internal/result"
	"github.com/fanlv/opensearch/internal/urlnorm"
)

type fakeResolver struct {
	addrs map[string][]netip.Addr
	err   error
}

func (f fakeResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.addrs[host], nil
}

func mustNorm(t *testing.T, raw string) *urlnorm.Normalized {
	t.Helper()
	norm, err := urlnorm.Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize(%q) error: %v", raw, err)
	}
	return norm
}

func TestValidatePublicTargetBlocksHostnamesWithoutDNS(t *testing.T) {
	cases := []string{
		"http://intranet/",
		"http://localhost/",
		"http://api.localhost/",
		"http://metadata.google.internal/",
		"http://foo.metadata.google.internal/",
		"http://instance-data.ec2.internal/",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			_, err := validatePublicTarget(context.Background(), mustNorm(t, raw), fakeResolver{})
			if itemErrorCode(err) != result.CodeSSRFBlocked {
				t.Fatalf("error = %v, want SSRF_BLOCKED", err)
			}
		})
	}
}

func TestValidatePublicTargetBlocksRestrictedAddressLiterals(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/",
		"http://168.63.129.16/",
		"http://100.64.0.1/",
		"http://192.0.2.1/",
		"http://[::1]/",
		"http://[fe80::1]/",
		"http://[fc00::1]/",
		"http://[2001:db8::1]/",
		"http://[3fff::1]/",
		"http://[5f00::1]/",
		"http://[2620:4f:8000::1]/",
		"http://[100:0:0:1::1]/",
		"http://[::ffff:192.168.1.1]/",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			_, err := validatePublicTarget(context.Background(), mustNorm(t, raw), fakeResolver{})
			if itemErrorCode(err) != result.CodeSSRFBlocked {
				t.Fatalf("error = %v, want SSRF_BLOCKED", err)
			}
		})
	}
}

func TestValidatePublicTargetAllowsPublicLiteralAndDNS(t *testing.T) {
	target, err := validatePublicTarget(context.Background(), mustNorm(t, "https://8.8.8.8/"), fakeResolver{})
	if err != nil {
		t.Fatalf("public IPv4 literal should pass: %v", err)
	}
	if len(target.Addrs) != 1 || target.Addrs[0].String() != "8.8.8.8" {
		t.Fatalf("target addrs = %+v, want 8.8.8.8", target.Addrs)
	}

	target, err = validatePublicTarget(context.Background(), mustNorm(t, "https://example.com/"), fakeResolver{
		addrs: map[string][]netip.Addr{"example.com": {netip.MustParseAddr("93.184.216.34")}},
	})
	if err != nil {
		t.Fatalf("public DNS result should pass: %v", err)
	}
	if len(target.Addrs) != 1 || target.Addrs[0].String() != "93.184.216.34" {
		t.Fatalf("target addrs = %+v, want 93.184.216.34", target.Addrs)
	}
}

func TestValidatePublicTargetBlocksAnyRestrictedDNSAnswer(t *testing.T) {
	_, err := validatePublicTarget(context.Background(), mustNorm(t, "https://example.com/"), fakeResolver{
		addrs: map[string][]netip.Addr{"example.com": {
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("127.0.0.1"),
		}},
	})
	if itemErrorCode(err) != result.CodeSSRFBlocked {
		t.Fatalf("error = %v, want SSRF_BLOCKED", err)
	}
}

func TestValidatePublicTargetMapsDNSErrorToNetworkError(t *testing.T) {
	_, err := validatePublicTarget(context.Background(), mustNorm(t, "https://example.com/"), fakeResolver{err: errors.New("boom")})
	if itemErrorCode(err) != result.CodeNetworkError {
		t.Fatalf("error = %v, want NETWORK_ERROR", err)
	}
}

func TestValidatePublicTargetMapsCanceledDNSToScrapeTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := validatePublicTarget(ctx, mustNorm(t, "https://example.com/"), fakeResolver{err: context.Canceled})
	if itemErrorCode(err) != result.CodeScrapeTimeout {
		t.Fatalf("error = %v, want SCRAPE_TIMEOUT", err)
	}
}

func TestBuildInputResultsMarksSSRFBlocked(t *testing.T) {
	data := BuildInputResults(context.Background(), &Params{URLs: []string{"http://127.0.0.1/"}, Format: FormatMarkdown, PerURLTimeoutSecs: DefaultPerURLTimeoutSeconds})
	if len(data.Results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(data.Results))
	}
	item := data.Results[0]
	if item.Success || item.Error == nil || item.Error.Code != result.CodeSSRFBlocked {
		t.Fatalf("item = %+v, want SSRF_BLOCKED failure", item)
	}
}

func TestResolvedTargetDialContextUsesOriginalValidatedAddressSet(t *testing.T) {
	sentinel := errors.New("dial sentinel")
	target := &ResolvedTarget{Host: "example.com", Addrs: []netip.Addr{netip.MustParseAddr("93.184.216.34")}}
	var dialed string
	dial := resolvedTargetDialContext(target, func(_ context.Context, _ string, address string) (net.Conn, error) {
		dialed = address
		return nil, sentinel
	})

	_, err := dial(context.Background(), "tcp", "example.com:443")
	if !errors.Is(err, sentinel) {
		t.Fatalf("dial error = %v, want sentinel", err)
	}
	if dialed != "93.184.216.34:443" {
		t.Fatalf("dialed = %q, want original validated address", dialed)
	}

	called := false
	dial = resolvedTargetDialContext(target, func(_ context.Context, _ string, _ string) (net.Conn, error) {
		called = true
		return nil, nil
	})
	_, err = dial(context.Background(), "tcp", "other.example:443")
	if itemErrorCode(err) != result.CodeSSRFBlocked {
		t.Fatalf("mismatched host error = %v, want SSRF_BLOCKED", err)
	}
	if called {
		t.Fatal("mismatched host must be rejected before dialing")
	}
}
