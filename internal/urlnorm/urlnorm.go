// Package urlnorm 实现方案 §5.1 的统一 URL 规则：把输入 URL 规范化为稳定形式，
// 或判定为无效。规范化结果用于 search 结果归一化、scrape 输入去重以及每次请求前的
// 安全校验。本包不做 SSRF / DNS 校验（方案 §6.1，属步骤 6）。
package urlnorm

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// maxURLBytes 是原始值与规范化值各自的 UTF-8 字节上限（方案 §5.1）。
const maxURLBytes = 8192

// ErrInvalidURL 表示输入不是一个可安全使用的 HTTP(S) URL。具体原因见 error 文本。
var ErrInvalidURL = errors.New("invalid url")

func invalid(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidURL, reason)
}

// idnaProfile 使用 Lookup 级别的严格校验，把国际化域名转为 ASCII（punycode）。
var idnaProfile = idna.New(
	idna.MapForLookup(),
	idna.BidiRule(),
	idna.ValidateLabels(true),
)

// Normalized 是规范化后的 URL，附带去除 fragment 后的形式，便于抓取与去重比较。
type Normalized struct {
	// URL 是规范化后的完整字符串（保留 fragment）。
	URL string
	// ForDedup 是忽略 fragment 后的规范化字符串，用于 scrape 去重与 SSRF 校验。
	ForDedup string
	// Host 是规范化后的主机名（小写、ASCII、无末尾根域点、无端口）。
	Host string
	// Scheme 是 "http" 或 "https"。
	Scheme string
}

// Normalize 按 §5.1 规则规范化并校验一个 URL。失败返回包装了 ErrInvalidURL 的错误。
func Normalize(raw string) (*Normalized, error) {
	if raw == "" {
		return nil, invalid("empty")
	}
	if err := ValidateRawInput(raw); err != nil {
		return nil, err
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, invalid("parse failed: " + err.Error())
	}

	// scheme 仅允许 http / https，转小写。
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, invalid("scheme must be http or https")
	}
	u.Scheme = scheme

	// 必须含 authority 且不得含用户信息。
	if u.User != nil {
		return nil, invalid("must not contain userinfo")
	}
	if u.Host == "" {
		return nil, invalid("missing host")
	}
	// Opaque 形式（如 http:foo）非法。
	if u.Opaque != "" {
		return nil, invalid("opaque url not allowed")
	}

	host, normHost, err := normalizeHostPort(u)
	if err != nil {
		return nil, err
	}
	u.Host = normHost

	// path / query 顺序与语义保持不变；仅校验百分号编码合法性，不做解码或重排。
	if err := validatePercentEncoding(u.EscapedPath()); err != nil {
		return nil, err
	}
	if u.RawQuery != "" {
		if err := validatePercentEncoding(u.RawQuery); err != nil {
			return nil, err
		}
	}

	full := u.String()
	if len(full) > maxURLBytes {
		return nil, invalid("normalized value exceeds 8192 bytes")
	}

	// 去 fragment 形式用于抓取与去重比较。
	dedupURL := *u
	dedupURL.Fragment = ""
	dedupURL.RawFragment = ""

	return &Normalized{
		URL:      full,
		ForDedup: dedupURL.String(),
		Host:     host,
		Scheme:   scheme,
	}, nil
}

// ValidateRawInput 校验 URL 原始输入中不能被 url.URL.String 静默修正的歧义形式。
// 该校验必须发生在标准库 URL 解析和序列化之前；重定向 Location 即使是相对引用，
// 也要先复用这部分原始字符规则，再解析到绝对 URL 后进入 Normalize。
func ValidateRawInput(raw string) error {
	if !utf8.ValidString(raw) {
		return invalid("raw value is not valid utf-8")
	}
	if len(raw) > maxURLBytes {
		return invalid("raw value exceeds 8192 bytes")
	}
	// 控制字符、裸空格以及会被 url.URL.String 静默转义的歧义字符一律拒绝，
	// 避免解析歧义、请求走私和把不同原始资源合并成同一规范化 URL。
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(` <>"{}|^`+"`", r) {
			return invalid("contains unsafe raw character")
		}
	}
	if rawURLPathQueryFragmentContainsNonASCII(raw) {
		return invalid("contains unsafe raw character")
	}
	// 反斜杠不是合法 URL 字符，浏览器会把它当作 '/'，这里直接拒绝以避免歧义。
	if strings.ContainsRune(raw, '\\') {
		return invalid("contains backslash")
	}
	return nil
}

func rawURLPathQueryFragmentContainsNonASCII(raw string) bool {
	tail := raw
	if authorityStart, ok := rawAuthorityStart(raw); ok {
		rel := raw[authorityStart:]
		if boundary := strings.IndexAny(rel, "/?#"); boundary >= 0 {
			tail = rel[boundary:]
		} else {
			tail = ""
		}
	}
	for _, r := range tail {
		if r > 0x7f {
			return true
		}
	}
	return false
}

func rawAuthorityStart(raw string) (int, bool) {
	if strings.HasPrefix(raw, "//") {
		return len("//"), true
	}
	if schemeIdx := strings.Index(raw, "://"); schemeIdx >= 0 {
		return schemeIdx + len("://"), true
	}
	return 0, false
}

// normalizeHostPort 规范化 host（含 IP 字面量 / IDN）与端口，返回不含端口的规范主机名
// 与 "host[:port]" 形式。移除 HTTP/HTTPS 默认端口。
func normalizeHostPort(u *url.URL) (host string, hostPort string, err error) {
	if hasExplicitEmptyPort(u.Host) {
		return "", "", invalid("empty port")
	}
	rawHost := u.Hostname()
	port := u.Port()

	if rawHost == "" {
		return "", "", invalid("missing host")
	}

	// 校验端口：严格规范十进制、在 1..65535，移除默认端口。
	// 前导零（如 080）会被某些解析器当作八进制，属歧义 authority，拒绝（方案 §5.1）。
	if port != "" {
		if len(port) > 1 && port[0] == '0' {
			return "", "", invalid("port has leading zero")
		}
		p, perr := strconv.Atoi(port)
		if perr != nil || p < 1 || p > 65535 {
			return "", "", invalid("invalid port")
		}
		if (u.Scheme == "http" && p == 80) || (u.Scheme == "https" && p == 443) {
			port = ""
		}
	}

	normHost, herr := normalizeHostname(rawHost)
	if herr != nil {
		return "", "", herr
	}

	// hostPort 用于回填 url.URL.Host，IPv6 字面量必须带方括号；
	// host 返回不带方括号的形式，便于 SSRF 校验与比较。
	if port == "" {
		if strings.Contains(normHost, ":") {
			return normHost, "[" + normHost + "]", nil
		}
		return normHost, normHost, nil
	}
	return normHost, netJoinHostPort(normHost, port), nil
}

func hasExplicitEmptyPort(hostport string) bool {
	if hostport == "" {
		return false
	}
	if strings.HasPrefix(hostport, "[") {
		end := strings.LastIndex(hostport, "]")
		return end >= 0 && end+1 < len(hostport) && hostport[end+1:] == ":"
	}
	return strings.HasSuffix(hostport, ":")
}

// netJoinHostPort 处理 IPv6 字面量加方括号；普通主机直接拼接。
func netJoinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// normalizeHostname 规范化主机名：区分 IPv6 字面量、IPv4 / IP 字面量、域名。
func normalizeHostname(rawHost string) (string, error) {
	// url.Hostname() 已去除 IPv6 的方括号；带 ':' 即为 IPv6 字面量候选。
	if strings.Contains(rawHost, ":") {
		return normalizeIPv6(rawHost)
	}

	// 末尾根域点：忽略（但 ".." 之类多点非法）。
	trimmed := strings.TrimSuffix(rawHost, ".")
	if trimmed == "" || strings.HasSuffix(trimmed, ".") {
		return "", invalid("malformed host")
	}

	// 全数字且含点的形式按 IPv4 字面量严格校验（只接受四段十进制）。
	if looksLikeIPv4Candidate(trimmed) {
		return normalizeIPv4(trimmed)
	}

	// 否则按域名处理：IDN 转 ASCII 并小写。
	ascii, err := idnaProfile.ToASCII(trimmed)
	if err != nil {
		return "", invalid("invalid hostname: " + err.Error())
	}
	ascii = strings.ToLower(ascii)
	if ascii == "" {
		return "", invalid("empty hostname after normalization")
	}
	return ascii, nil
}

// looksLikeIPv4Candidate 判断字符串是否“看起来像”要被当作 IPv4 的形式，
// 用于把可能被宽松解析器当作 IPv4（dotted-decimal / 整数 / 八进制 / 十六进制）的
// 输入导向严格 IPv4 校验后拒绝歧义形式。
//
// 规则：
//   - 含点的形式：仅当每个点分段都可被解释为 IPv4 八位组（纯十进制、八进制 0 前缀、
//     或 0x 十六进制）时才视为 IPv4 候选；只要有一段含非十六进制字母（如 com / de），
//     即为普通域名，不被误判为 IPv4（避免误伤 bad.de / 0xdead.com 这类合法域名）。
//   - 无点的单标签：仅纯十进制整数（如 2130706433）或显式 0x 十六进制（如 0x7f000001）
//     才视为 IPv4 候选；裸十六进制字母标签（如 dead / cafe）是普通域名，不误判。
func looksLikeIPv4Candidate(h string) bool {
	if h == "" {
		return false
	}
	if !strings.Contains(h, ".") {
		// 单标签：纯十进制或 0x 十六进制才可能被当作整数 IPv4。
		if isAllDigits(h) {
			return true
		}
		if strings.HasPrefix(h, "0x") || strings.HasPrefix(h, "0X") {
			return true
		}
		return false
	}
	for _, part := range strings.Split(h, ".") {
		if part == "" {
			return true // 出现空段，交给严格校验报错
		}
		if !looksLikeIPv4Octet(part) {
			return false
		}
	}
	return true
}

// looksLikeIPv4Octet 报告某个点分段是否可被宽松解析器当作 IPv4 八位组：
// 0x/0X 十六进制，或纯十进制数字（含八进制 0 前缀）。裸十六进制字母段（如 cafe）
// 不会被标准解析器当作八位组，按普通域名 label 处理，不视为 IPv4 候选。
func looksLikeIPv4Octet(part string) bool {
	if part == "" {
		return false
	}
	if strings.HasPrefix(part, "0x") || strings.HasPrefix(part, "0X") {
		rest := part[2:]
		if rest == "" {
			return true // 0x 空尾，交给严格校验报错
		}
		return isAllHexDigits(rest)
	}
	return isAllDigits(part)
}

func isAllHexDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r >= '0' && r <= '9' {
			continue
		}
		if (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// normalizeIPv4 仅接受四段十进制 IPv4，拒绝整数 / 八进制 / 十六进制 / 少于四段形式。
func normalizeIPv4(h string) (string, error) {
	parts := strings.Split(h, ".")
	if len(parts) != 4 {
		return "", invalid("ambiguous IPv4 literal (must be four dotted-decimal octets)")
	}
	for _, p := range parts {
		if !isAllDigits(p) {
			return "", invalid("IPv4 octet must be decimal")
		}
		// 前导零（如 010）会被某些解析器当作八进制，视为歧义，拒绝。
		if len(p) > 1 && p[0] == '0' {
			return "", invalid("IPv4 octet has leading zero")
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return "", invalid("IPv4 octet out of range")
		}
	}
	addr, err := netip.ParseAddr(h)
	if err != nil || !addr.Is4() {
		return "", invalid("invalid IPv4 literal")
	}
	return addr.String(), nil
}

// normalizeIPv6 规范化方括号 IPv6 字面量，拒绝 zone identifier。
func normalizeIPv6(h string) (string, error) {
	if strings.Contains(h, "%") {
		return "", invalid("IPv6 zone identifier not allowed")
	}
	addr, err := netip.ParseAddr(h)
	if err != nil || !addr.Is6() {
		return "", invalid("invalid IPv6 literal")
	}
	// IPv4-mapped IPv6 与对应 IPv4 字面量表示同一目标，URL 规范化与去重阶段即合并。
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	return addr.String(), nil
}

// validatePercentEncoding 校验百分号编码合法性：每个 '%' 后必须跟两个十六进制位。
// 不做解码或重排，保持 path/query 原样（方案 §5.1）。
func validatePercentEncoding(s string) error {
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		if i+2 >= len(s) {
			return invalid("truncated percent-encoding")
		}
		if !isHex(s[i+1]) || !isHex(s[i+2]) {
			return invalid("invalid percent-encoding")
		}
		// 拒绝编码后的主机分隔符 / 控制字符（如 %2f 在 path 中歧义、%00）。
		hi := unhex(s[i+1])
		lo := unhex(s[i+2])
		b := hi<<4 | lo
		if b < 0x20 || b == 0x7f {
			return invalid("encoded control character")
		}
		if isEncodedDelimiter(b) {
			return invalid("encoded delimiter character")
		}
		i += 2
	}
	return nil
}

func isEncodedDelimiter(b byte) bool {
	switch b {
	case ':', '/', '?', '#', '[', ']', '@', '\\':
		return true
	default:
		return false
	}
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
