package urlnorm

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeValid(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantURL  string
		wantHost string
	}{
		{"basic https", "https://Example.com/Path", "https://example.com/Path", "example.com"},
		{"lowercase scheme host", "HTTP://EXAMPLE.COM/", "http://example.com/", "example.com"},
		{"strip default https port", "https://example.com:443/x", "https://example.com/x", "example.com"},
		{"strip default http port", "http://example.com:80/x", "http://example.com/x", "example.com"},
		{"keep non-default port", "https://example.com:8443/", "https://example.com:8443/", "example.com"},
		{"trailing root dot", "https://example.com./a", "https://example.com/a", "example.com"},
		{"ipv4 literal", "http://192.168.0.1/", "http://192.168.0.1/", "192.168.0.1"},
		{"ipv6 literal", "http://[2001:db8::1]/p", "http://[2001:db8::1]/p", "2001:db8::1"},
		{"query preserved", "https://e.com/s?b=2&a=1", "https://e.com/s?b=2&a=1", "e.com"},
		{"percent encoded unicode path", "https://e.com/%E4%B8%AD%E6%96%87", "https://e.com/%E4%B8%AD%E6%96%87", "e.com"},
		{"idn to ascii", "https://例え.テスト/", "https://xn--r8jz45g.xn--zckzah/", "xn--r8jz45g.xn--zckzah"},
		{"hex-looking domain bad de", "https://bad.de/", "https://bad.de/", "bad.de"},
		{"hex-looking domain feed de", "https://feed.de/path", "https://feed.de/path", "feed.de"},
		{"hex-looking domain face be", "https://face.be/", "https://face.be/", "face.be"},
		// 0x 前缀只在该 label 是单独的 IPv4 八位组候选时才导向 IPv4 校验；
		// 多标签域名的首段以 0x 开头仍是合法域名（回归 #1）。
		{"0x-prefixed domain", "https://0xdead.com/", "https://0xdead.com/", "0xdead.com"},
		{"0x-prefixed subdomain", "https://0xabc.example.com/", "https://0xabc.example.com/", "0xabc.example.com"},
		{"all-hex-letter domain", "https://cafe.babe/", "https://cafe.babe/", "cafe.babe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Normalize(c.in)
			if err != nil {
				t.Fatalf("Normalize(%q) unexpected error: %v", c.in, err)
			}
			if got.URL != c.wantURL {
				t.Errorf("URL = %q, want %q", got.URL, c.wantURL)
			}
			if got.Host != c.wantHost {
				t.Errorf("Host = %q, want %q", got.Host, c.wantHost)
			}
		})
	}
}

func TestNormalizeDedupIgnoresFragment(t *testing.T) {
	a, err := Normalize("https://e.com/p#frag")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Normalize("https://e.com/p")
	if err != nil {
		t.Fatal(err)
	}
	if a.ForDedup != b.ForDedup {
		t.Errorf("ForDedup mismatch: %q vs %q", a.ForDedup, b.ForDedup)
	}
	if a.URL == b.URL {
		t.Errorf("full URL should keep fragment, got identical %q", a.URL)
	}
}

func TestNormalizeInvalid(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"non-http scheme", "ftp://example.com/"},
		{"file scheme", "file:///etc/passwd"},
		{"javascript", "javascript:alert(1)"},
		{"userinfo", "https://user:pass@example.com/"},
		{"userinfo no pass", "https://user@example.com/"},
		{"missing host", "https:///path"},
		{"invalid utf8", string([]byte{'h', 't', 't', 'p', 's', ':', '/', '/', 'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'c', 'o', 'm', '/', 0xff})},
		{"backslash", "https://example.com\\@evil.com/"},
		{"control char", "https://example.com/\x01"},
		{"raw space in path", "https://example.com/a b"},
		{"raw space in query", "https://example.com/?q=a b"},
		{"raw unicode in path", "https://example.com/中文"},
		{"raw unicode in query", "https://example.com/?q=中文"},
		{"raw unicode in relative redirect", "/中文"},
		{"raw angle bracket", "https://example.com/<img>"},
		{"raw pipe", "https://example.com/a|b"},
		{"ipv4 three segments", "http://1.2.3/"},
		{"ipv4 leading zero", "http://010.0.0.1/"},
		{"ipv4 hex", "http://0x7f.0.0.1/"},
		{"ipv4 integer", "http://2130706433/"},
		{"ipv4 octet overflow", "http://256.0.0.1/"},
		{"port leading zero", "http://1.2.3.4:080/"},
		{"port multiple leading zeros", "http://1.2.3.4:00080/"},
		{"empty port", "http://example.com:/path"},
		{"empty https port", "https://example.com:/"},
		{"empty ipv6 port", "http://[2001:db8::1]:/"},
		{"ipv6 zone id", "http://[fe80::1%25eth0]/"},
		{"bad percent", "https://example.com/%zz"},
		{"truncated percent", "https://example.com/%2"},
		{"encoded null", "https://example.com/%00"},
		{"encoded slash in path", "https://example.com/a%2fb"},
		{"encoded backslash in path", "https://example.com/a%5cb"},
		{"encoded question in path", "https://example.com/a%3fb"},
		{"encoded fragment in query", "https://example.com/a?q=%23frag"},
		{"encoded at in query", "https://example.com/a?q=%40evil"},
		{"opaque", "http:example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Normalize(c.in)
			if err == nil {
				t.Fatalf("Normalize(%q) = nil error, want invalid", c.in)
			}
			if !errors.Is(err, ErrInvalidURL) {
				t.Errorf("error %v should wrap ErrInvalidURL", err)
			}
		})
	}
}

func TestNormalizeOverlongRejected(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", maxURLBytes)
	if _, err := Normalize(long); err == nil {
		t.Error("expected overlong URL to be rejected")
	}
}

func TestNormalizeIPv4MappedIPv6(t *testing.T) {
	got, err := Normalize("http://[::ffff:1.2.3.4]/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.URL != "http://1.2.3.4/" || got.Host != "1.2.3.4" {
		t.Fatalf("IPv4-mapped IPv6 normalized to URL=%q host=%q, want IPv4 literal", got.URL, got.Host)
	}
	plain, err := Normalize("http://1.2.3.4/")
	if err != nil {
		t.Fatalf("unexpected IPv4 error: %v", err)
	}
	if got.ForDedup != plain.ForDedup {
		t.Fatalf("IPv4-mapped IPv6 dedup key = %q, want %q", got.ForDedup, plain.ForDedup)
	}
}
