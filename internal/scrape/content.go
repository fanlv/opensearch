package scrape

import (
	"bytes"
	"context"
	"fmt"
	stdhtml "html"
	"mime"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fanlv/opensearch/internal/result"
	nethtml "golang.org/x/net/html"
)

const utf8BOM = "\ufeff"

const (
	scrapeTitleMaxBytes = 1024
	contentTypeMaxBytes = 1024
)

var (
	whitespaceRE              = regexp.MustCompile(`\s+`)
	markdownDangerousHTMLRE   = regexp.MustCompile(`(?is)<\s*(script|style|noscript|template|iframe|object|embed|applet|canvas|svg|math|form|button|select|textarea|head|meta|link|base|img|video|audio|source|track|picture)\b[^>]*>.*?<\s*/\s*(script|style|noscript|template|iframe|object|embed|applet|canvas|svg|math|form|button|select|textarea|head|meta|link|base|img|video|audio|source|track|picture)\s*>`)
	markdownDangerousSingleRE = regexp.MustCompile(`(?is)<\s*/?\s*(script|style|noscript|template|iframe|object|embed|applet|canvas|svg|math|form|input|button|select|textarea|head|meta|link|base|img|video|audio|source|track|picture)\b[^>]*>`)
	markdownAnyHTMLTagRE      = regexp.MustCompile(`(?s)<[^>]*>`)
	markdownRefDefRE          = regexp.MustCompile(`(?m)^[ \t]{0,3}\[([^\]]+)\]:[ \t]*(\S+)(?:[ \t]+.*)?$`)
	markdownRefImageRE        = regexp.MustCompile(`!\[([^\]]*)\]\[([^\]]*)\]`)
	markdownRefLinkRE         = regexp.MustCompile(`\[([^\]]+)\]\[([^\]]*)\]`)
	markdownAutoLinkRE        = regexp.MustCompile(`<([^<>\s]+)>`)
	markdownFenceRE           = regexp.MustCompile("(?s)```.*?```")

	droppedHTMLTags = map[string]struct{}{
		"script": {}, "style": {}, "noscript": {}, "template": {},
		"iframe": {}, "object": {}, "embed": {}, "applet": {},
		"canvas": {}, "svg": {}, "math": {},
		"form": {}, "input": {}, "button": {}, "select": {}, "textarea": {},
		"head": {}, "meta": {}, "link": {}, "base": {},
		"img": {}, "video": {}, "audio": {}, "source": {}, "track": {}, "picture": {},
	}
)

var renderHTMLNode = nethtml.Render

func processContent(ctx context.Context, raw []byte, contentTypeHeader, format string, mainContent bool) (string, string, bool, map[string]interface{}, error) {
	if err := checkScrapeContext(ctx); err != nil {
		return "", "", false, nil, err
	}
	mediaType, params, err := parseSupportedContentType(contentTypeHeader)
	if err != nil {
		return "", "", false, nil, err
	}
	text, err := decodeUTF8(raw, params["charset"])
	if err != nil {
		return "", "", false, nil, err
	}
	if err := checkScrapeContext(ctx); err != nil {
		return "", "", false, nil, err
	}

	meta := map[string]interface{}{"mediaType": mediaType}
	switch mediaType {
	case "text/plain":
		content := convertPlainText(text, format)
		if isBlank(content) {
			return "", "", false, nil, &targetError{code: result.CodeEmptyContent, msg: "content is empty"}
		}
		return content, "", false, meta, nil
	case "text/markdown", "text/x-markdown":
		content, err := convertMarkdown(ctx, text, format)
		if err != nil {
			return "", "", false, nil, err
		}
		if isBlank(content) {
			return "", "", false, nil, &targetError{code: result.CodeEmptyContent, msg: "content is empty"}
		}
		return content, "", false, meta, nil
	case "text/html", "application/xhtml+xml":
		content, title, titleTruncated, htmlMeta, err := convertHTML(ctx, text, format, mainContent)
		if err != nil {
			return "", "", false, nil, err
		}
		for k, v := range htmlMeta {
			meta[k] = v
		}
		return content, title, titleTruncated, meta, nil
	default:
		return "", "", false, nil, &targetError{code: result.CodeUnsupportedContentType, msg: "unsupported content type"}
	}
}

func isSupportedMediaType(mediaType string) bool {
	switch mediaType {
	case "text/plain", "text/markdown", "text/x-markdown", "text/html", "application/xhtml+xml":
		return true
	default:
		return false
	}
}

func parseSupportedContentType(header string) (string, map[string]string, error) {
	mediaType, params, err := parseContentType(header)
	if err != nil {
		return "", nil, err
	}
	if !isSupportedMediaType(mediaType) {
		return "", nil, &targetError{code: result.CodeUnsupportedContentType, msg: "unsupported content type"}
	}
	return mediaType, params, nil
}

func parseContentType(header string) (string, map[string]string, error) {
	if strings.TrimSpace(header) == "" {
		return "", nil, &targetError{code: result.CodeUnsupportedContentType, msg: "missing content type"}
	}
	mediaType, params, err := mime.ParseMediaType(header)
	if err != nil {
		return "", nil, &targetError{code: result.CodeUnsupportedContentType, msg: "invalid content type"}
	}
	return strings.ToLower(mediaType), params, nil
}

func decodeUTF8(raw []byte, charset string) (string, error) {
	if charset != "" && !strings.EqualFold(strings.TrimSpace(charset), "utf-8") {
		return "", &targetError{code: result.CodeUnsupportedCharset, msg: "unsupported charset"}
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if !utf8.Valid(raw) {
		return "", &targetError{code: result.CodeUnsupportedCharset, msg: "invalid utf-8 content"}
	}
	return strings.TrimPrefix(string(raw), utf8BOM), nil
}

func convertPlainText(text, format string) string {
	switch format {
	case FormatMarkdown:
		return escapeMarkdownText(text)
	case FormatHTML:
		return stdhtml.EscapeString(text)
	default:
		return text
	}
}

func convertMarkdown(ctx context.Context, text, format string) (string, error) {
	if err := checkScrapeContext(ctx); err != nil {
		return "", err
	}
	text = sanitizeMarkdown(text)
	if err := checkScrapeContext(ctx); err != nil {
		return "", err
	}
	switch format {
	case FormatText:
		return markdownToText(text), nil
	case FormatHTML:
		htmlOut, err := markdownToHTML(ctx, text)
		if err != nil {
			return "", err
		}
		return sanitizeGeneratedHTML(ctx, htmlOut)
	default:
		return text, nil
	}
}

func sanitizeGeneratedHTML(ctx context.Context, src string) (string, error) {
	if err := checkScrapeContext(ctx); err != nil {
		return "", err
	}
	doc, err := nethtml.Parse(strings.NewReader(src))
	if err != nil {
		return "", &targetError{code: result.CodeConversionError, msg: "failed to parse generated HTML"}
	}
	if err := sanitizeHTMLTree(ctx, doc); err != nil {
		return "", err
	}
	root := findElement(doc, "body")
	if root == nil {
		root = doc
	}
	return convertHTMLNode(ctx, root, FormatHTML)
}

func convertHTML(ctx context.Context, src, format string, mainContent bool) (string, string, bool, map[string]interface{}, error) {
	if err := checkScrapeContext(ctx); err != nil {
		return "", "", false, nil, err
	}
	doc, err := nethtml.Parse(strings.NewReader(src))
	if err != nil {
		return "", "", false, nil, &targetError{code: result.CodeConversionError, msg: "failed to parse HTML"}
	}
	if err := checkScrapeContext(ctx); err != nil {
		return "", "", false, nil, err
	}
	title, titleTruncated := truncateUTF8Bytes(normalizeSpace(firstText(findElement(doc, "title"))), scrapeTitleMaxBytes)
	if err := sanitizeHTMLTree(ctx, doc); err != nil {
		return "", "", false, nil, err
	}

	root := findElement(doc, "body")
	if root == nil {
		root = doc
	}
	mainExtracted := false
	fallbackUsed := false
	if mainContent {
		candidate, err := firstNonEmptyElement(ctx, doc, "main", "article")
		if err != nil {
			return "", "", false, nil, err
		}
		if candidate != nil {
			root = candidate
			mainExtracted = true
		} else {
			fallbackUsed = true
		}
	}

	content, err := convertHTMLNode(ctx, root, format)
	if err != nil {
		return "", "", false, nil, err
	}
	if isBlank(content) && mainExtracted {
		fallbackUsed = true
		mainExtracted = false
		root = findElement(doc, "body")
		if root == nil {
			root = doc
		}
		content, err = convertHTMLNode(ctx, root, format)
		if err != nil {
			return "", "", false, nil, err
		}
	}
	if isBlank(content) {
		return "", "", false, nil, &targetError{code: result.CodeEmptyContent, msg: "content is empty"}
	}

	meta := map[string]interface{}{
		"mainContentExtracted": mainExtracted,
		"fallbackUsed":         fallbackUsed,
	}
	if titleTruncated {
		meta["titleTruncated"] = true
	}
	return content, title, titleTruncated, meta, nil
}

func convertHTMLNode(ctx context.Context, n *nethtml.Node, format string) (string, error) {
	if err := checkScrapeContext(ctx); err != nil {
		return "", err
	}
	switch format {
	case FormatText:
		return visibleText(ctx, n)
	case FormatHTML:
		var b strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := checkScrapeContext(ctx); err != nil {
				return "", err
			}
			if err := renderHTMLNode(&b, c); err != nil {
				return "", &targetError{code: result.CodeConversionError, msg: "failed to render HTML"}
			}
		}
		return b.String(), nil
	default:
		return htmlToMarkdown(ctx, n)
	}
}

func firstNonEmptyElement(ctx context.Context, root *nethtml.Node, names ...string) (*nethtml.Node, error) {
	for _, name := range names {
		if err := checkScrapeContext(ctx); err != nil {
			return nil, err
		}
		if n := findElement(root, name); n != nil {
			text, err := visibleText(ctx, n)
			if err != nil {
				return nil, err
			}
			if !isBlank(text) {
				return n, nil
			}
		}
	}
	return nil, nil
}

func findElement(root *nethtml.Node, name string) *nethtml.Node {
	if root == nil {
		return nil
	}
	if root.Type == nethtml.ElementNode && strings.EqualFold(root.Data, name) {
		return root
	}
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if n := findElement(c, name); n != nil {
			return n
		}
	}
	return nil
}

func visibleText(ctx context.Context, n *nethtml.Node) (string, error) {
	text, err := visibleTextRaw(ctx, n)
	if err != nil {
		return "", err
	}
	return normalizeSpace(text), nil
}

func visibleTextRaw(ctx context.Context, n *nethtml.Node) (string, error) {
	if err := checkScrapeContext(ctx); err != nil {
		return "", err
	}
	if n == nil {
		return "", nil
	}
	if n.Type == nethtml.TextNode {
		return n.Data, nil
	}
	if n.Type == nethtml.ElementNode {
		switch strings.ToLower(n.Data) {
		case "script", "style", "noscript", "template", "head", "svg", "canvas", "iframe", "object", "embed", "applet", "math", "form", "input", "button", "select", "textarea", "meta", "link", "base", "img", "video", "audio", "source", "track", "picture":
			return "", nil
		}
	}
	var parts []string
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		s, err := visibleTextRaw(ctx, c)
		if err != nil {
			return "", err
		}
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " "), nil
}

func firstText(n *nethtml.Node) string {
	if n == nil {
		return ""
	}
	if n.Type == nethtml.TextNode {
		return n.Data
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if s := firstText(c); s != "" {
			return s
		}
	}
	return ""
}

func htmlToMarkdown(ctx context.Context, n *nethtml.Node) (string, error) {
	out, err := markdownFromNode(ctx, n)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func markdownFromNode(ctx context.Context, n *nethtml.Node) (string, error) {
	if err := checkScrapeContext(ctx); err != nil {
		return "", err
	}
	if n == nil {
		return "", nil
	}
	if n.Type == nethtml.TextNode {
		return escapeMarkdownText(normalizeInline(n.Data)), nil
	}
	if n.Type == nethtml.ElementNode {
		tag := strings.ToLower(n.Data)
		switch tag {
		case "script", "style", "noscript", "template", "head", "svg", "canvas", "iframe", "object", "embed", "applet", "math", "form", "input", "button", "select", "textarea", "meta", "link", "base", "img", "video", "audio", "source", "track", "picture":
			return "", nil
		case "br":
			return "\n", nil
		case "h1", "h2", "h3", "h4", "h5", "h6":
			level := int(tag[1] - '0')
			child, err := childMarkdown(ctx, n)
			return block(strings.Repeat("#", level) + " " + child), err
		case "p", "section", "article", "main", "div":
			child, err := childMarkdown(ctx, n)
			return block(child), err
		case "li":
			child, err := childMarkdown(ctx, n)
			return "- " + strings.TrimSpace(child) + "\n", err
		case "ul", "ol":
			child, err := childMarkdown(ctx, n)
			return block(child), err
		case "a":
			child, err := childMarkdown(ctx, n)
			if err != nil {
				return "", err
			}
			label := strings.TrimSpace(child)
			if label == "" {
				label = strings.TrimSpace(attr(n, "href"))
			}
			if href := strings.TrimSpace(attr(n, "href")); href != "" && isSafeLinkURL(href) {
				return fmt.Sprintf("[%s](%s)", escapeMarkdownLabel(label), escapeMarkdownDestination(href)), nil
			}
			return label, nil
		}
	}
	return childMarkdown(ctx, n)
}

func escapeMarkdownLabel(label string) string {
	label = strings.ReplaceAll(label, `\`, `\\`)
	label = strings.ReplaceAll(label, `[`, `\[`)
	label = strings.ReplaceAll(label, `]`, `\]`)
	return label
}

func escapeMarkdownText(text string) string {
	if text == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`<`, `&lt;`,
		`>`, `&gt;`,
		`[`, `\[`,
		`]`, `\]`,
		`(`, `\(`,
		`)`, `\)`,
		`!`, `\!`,
	)
	return replacer.Replace(text)
}

func escapeMarkdownDestination(href string) string {
	href = stdhtml.UnescapeString(strings.TrimSpace(href))
	parsed, _ := url.Parse(href)
	replacer := strings.NewReplacer(
		" ", "%20",
		"\t", "%09",
		"(", "%28",
		")", "%29",
		"[", "%5B",
		"]", "%5D",
		"<", "%3C",
		">", "%3E",
	)
	escaped := replacer.Replace(href)
	if parsed == nil || parsed.Scheme == "" {
		escaped = strings.ReplaceAll(escaped, ":", "%3A")
	}
	return escaped
}

func childMarkdown(ctx context.Context, n *nethtml.Node) (string, error) {
	var parts []string
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		s, err := markdownFromNode(ctx, c)
		if err != nil {
			return "", err
		}
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " ")), nil
}

func block(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return s + "\n\n"
}

func attr(n *nethtml.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func sanitizeHTMLTree(ctx context.Context, n *nethtml.Node) error {
	if err := checkScrapeContext(ctx); err != nil {
		return err
	}
	if n == nil {
		return nil
	}
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == nethtml.ElementNode && shouldDropHTMLElement(c.Data) {
			n.RemoveChild(c)
		} else {
			if err := sanitizeHTMLTree(ctx, c); err != nil {
				return err
			}
		}
		c = next
	}
	if n.Type == nethtml.ElementNode {
		n.Attr = sanitizeHTMLAttrs(n.Attr)
	}
	return nil
}

func shouldDropHTMLElement(tag string) bool {
	_, ok := droppedHTMLTags[strings.ToLower(tag)]
	return ok
}

func sanitizeHTMLAttrs(attrs []nethtml.Attribute) []nethtml.Attribute {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]nethtml.Attribute, 0, len(attrs))
	for _, a := range attrs {
		key := strings.ToLower(strings.TrimSpace(a.Key))
		switch {
		case key == "" || strings.HasPrefix(key, "on"):
			continue
		case key == "style" || key == "srcdoc":
			continue
		case key == "href":
			if !isSafeLinkURL(a.Val) {
				continue
			}
			out = append(out, a)
		case key == "src" || key == "srcset" || key == "action" || key == "formaction" || key == "poster" || key == "background" || key == "xlink:href":
			continue
		case key == "title" || key == "alt" || key == "lang" || key == "dir" || strings.HasPrefix(key, "aria-"):
			out = append(out, a)
		}
	}
	return out
}

func isSafeLinkURL(raw string) bool {
	raw = strings.TrimSpace(stdhtml.UnescapeString(raw))
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n\t") {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme == "" {
		// A scheme-relative reference (`//host/path`) targets an arbitrary external
		// host, so it is not a safe relative URL and must not be preserved as a link.
		if parsed.Host != "" {
			return false
		}
		return true
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "mailto":
		return true
	default:
		return false
	}
}

func sanitizeMarkdown(md string) string {
	fences := map[string]string{}
	md = markdownFenceRE.ReplaceAllStringFunc(md, func(match string) string {
		token := fmt.Sprintf("__OPENSEARCH_FENCE_%d__", len(fences))
		fences[token] = match
		return token
	})
	md = markdownDangerousHTMLRE.ReplaceAllString(md, " ")
	md = markdownDangerousSingleRE.ReplaceAllString(md, " ")
	md = sanitizeMarkdownReferenceLinks(md)
	md = markdownAutoLinkRE.ReplaceAllStringFunc(md, func(match string) string {
		parts := markdownAutoLinkRE.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		href := strings.TrimSpace(parts[1])
		parsed, err := url.Parse(stdhtml.UnescapeString(href))
		if err != nil || parsed.Scheme == "" || !isSafeLinkURL(href) {
			return match
		}
		return fmt.Sprintf("[%s](%s)", escapeMarkdownLabel(href), escapeMarkdownDestination(href))
	})
	md = markdownAnyHTMLTagRE.ReplaceAllString(md, " ")
	md = replaceInlineImages(md, func(string) string { return " " })
	md = replaceInlineLinks(md, func(label, href string) string {
		label = strings.TrimSpace(label)
		href = strings.TrimSpace(href)
		if !isSafeLinkURL(href) {
			return label
		}
		return fmt.Sprintf("[%s](%s)", escapeMarkdownLabel(label), escapeMarkdownDestination(href))
	})
	for token, fence := range fences {
		md = strings.ReplaceAll(md, token, fence)
	}
	return strings.TrimSpace(md)
}

func sanitizeMarkdownReferenceLinks(md string) string {
	defs := map[string]string{}
	md = markdownRefDefRE.ReplaceAllStringFunc(md, func(match string) string {
		parts := markdownRefDefRE.FindStringSubmatch(match)
		if len(parts) != 3 {
			return " "
		}
		id := normalizeReferenceID(parts[1])
		href := strings.Trim(strings.TrimSpace(parts[2]), "<>")
		if id != "" && isSafeLinkURL(href) {
			defs[id] = stdhtml.UnescapeString(href)
		}
		return " "
	})
	md = markdownRefImageRE.ReplaceAllString(md, " ")
	md = markdownRefLinkRE.ReplaceAllStringFunc(md, func(match string) string {
		parts := markdownRefLinkRE.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		label := strings.TrimSpace(parts[1])
		id := parts[2]
		if id == "" {
			id = label
		}
		if href, ok := defs[normalizeReferenceID(id)]; ok && isSafeLinkURL(href) {
			return fmt.Sprintf("[%s](%s)", escapeMarkdownLabel(label), escapeMarkdownDestination(href))
		}
		return label
	})
	return md
}

func normalizeReferenceID(id string) string {
	return strings.ToLower(normalizeSpace(id))
}

// markdownToText 从 Markdown 提取可见纯文本：移除内嵌 HTML、图片与链接语法，
// 并剥离 Markdown 标记符。标记符按位置剥离，避免误伤正文中的连字符 / 下划线
// （如 state-of-the-art、my_var）：
//   - `#` / `>` 仅在行首（ATX 标题 / 引用）剥离；
//   - `-` 仅在行首作为列表项 / 分隔线时剥离；
//   - `_` 仅作为单词边界处的强调标记剥离，保留词内下划线；
//   - 反引号、`*`、`~` 作为行内强调 / 代码 / 删除线标记整体剥离。
var (
	mdInlineHTMLRE    = regexp.MustCompile("(?s)<[^>]*>")
	mdLeadingHashRE   = regexp.MustCompile(`(?m)^[ \t]*#{1,6}[ \t]*`)
	mdLeadingQuoteRE  = regexp.MustCompile(`(?m)^[ \t]*>+[ \t]*`)
	mdLeadingBulletRE = regexp.MustCompile(`(?m)^[ \t]*-+[ \t]*`)
	mdEmphasisMarkRE  = regexp.MustCompile("[`*~]+")
	// 词边界处的下划线强调：前或后不是单词字符时剥离，保留 my_var 这类词内下划线。
	mdUnderscoreEmphasisRE = regexp.MustCompile(`(^|[^\w])_+|_+([^\w]|$)`)
)

func markdownToText(md string) string {
	md = mdInlineHTMLRE.ReplaceAllString(md, " ")
	md = replaceInlineImages(md, func(string) string { return " " })
	md = replaceInlineLinks(md, func(label, _ string) string { return label })
	md = mdLeadingHashRE.ReplaceAllString(md, " ")
	md = mdLeadingQuoteRE.ReplaceAllString(md, " ")
	md = mdLeadingBulletRE.ReplaceAllString(md, " ")
	md = mdEmphasisMarkRE.ReplaceAllString(md, " ")
	md = mdUnderscoreEmphasisRE.ReplaceAllStringFunc(md, func(m string) string {
		// 保留触发匹配的非单词字符（如空格 / 标点），只去掉下划线本身。
		return strings.ReplaceAll(m, "_", " ")
	})
	return normalizeSpace(md)
}

// replaceInlineImages 替换 `![alt](dest)` 形式的内联图片，destination 支持平衡括号。
func replaceInlineImages(md string, repl func(alt string) string) string {
	return replaceInlineLinkLike(md, true, func(alt, _ string) string { return repl(alt) })
}

// replaceInlineLinks 替换 `[label](dest)` 形式的内联链接，destination 支持平衡括号
// （如维基百科的 .../Go_(programming_language)），避免在第一个 ')' 处误截断。
func replaceInlineLinks(md string, repl func(label, dest string) string) string {
	return replaceInlineLinkLike(md, false, repl)
}

// replaceInlineLinkLike 扫描 markdown 文本，匹配内联链接 / 图片并交给 repl 处理。
// destination 用括号深度计数匹配，正确处理目标 URL 中的平衡括号；非平衡 / 残缺
// 形式不视为链接，原样保留。
func replaceInlineLinkLike(md string, image bool, repl func(label, dest string) string) string {
	var b strings.Builder
	for i := 0; i < len(md); {
		start := i
		if image {
			if md[i] != '!' || i+1 >= len(md) || md[i+1] != '[' {
				b.WriteByte(md[i])
				i++
				continue
			}
			i++ // 跳过 '!'
		} else if md[i] != '[' {
			b.WriteByte(md[i])
			i++
			continue
		}

		label, next, ok := scanBracketed(md, i, '[', ']')
		if !ok || next >= len(md) || md[next] != '(' {
			b.WriteByte(md[start])
			i = start + 1
			continue
		}
		dest, after, ok := scanBracketed(md, next, '(', ')')
		if !ok {
			b.WriteByte(md[start])
			i = start + 1
			continue
		}
		b.WriteString(repl(label, dest))
		i = after
	}
	return b.String()
}

// scanBracketed 从 s[start]（必须是 open）开始，按括号深度计数匹配到配对的 close，
// 返回括号内内容、配对 close 之后的下标，以及是否成功匹配。
func scanBracketed(s string, start int, open, close byte) (string, int, bool) {
	if start >= len(s) || s[start] != open {
		return "", start, false
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start+1 : i], i + 1, true
			}
		}
	}
	return "", start, false
}

func markdownToHTML(ctx context.Context, md string) (string, error) {
	lines := strings.Split(md, "\n")
	var b strings.Builder
	inList := false
	inCode := false
	var code strings.Builder
	var paragraph []string
	flushParagraph := func() {
		if len(paragraph) == 0 {
			return
		}
		fmt.Fprintf(&b, "<p>%s</p>\n", inlineMarkdownToHTML(strings.Join(paragraph, " ")))
		paragraph = nil
	}
	closeList := func() {
		if inList {
			b.WriteString("</ul>\n")
			inList = false
		}
	}
	for _, line := range lines {
		if err := checkScrapeContext(ctx); err != nil {
			return "", err
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			flushParagraph()
			closeList()
			if inCode {
				fmt.Fprintf(&b, "<pre><code>%s</code></pre>\n", stdhtml.EscapeString(strings.TrimSuffix(code.String(), "\n")))
				code.Reset()
				inCode = false
			} else {
				inCode = true
			}
			continue
		}
		if inCode {
			code.WriteString(line)
			code.WriteByte('\n')
			continue
		}
		if trimmed == "" {
			flushParagraph()
			closeList()
			continue
		}
		if level := markdownHeadingLevel(trimmed); level > 0 {
			flushParagraph()
			closeList()
			text := strings.TrimSpace(trimmed[level:])
			fmt.Fprintf(&b, "<h%d>%s</h%d>\n", level, inlineMarkdownToHTML(text), level)
			continue
		}
		if item, ok := markdownListItem(trimmed); ok {
			flushParagraph()
			if !inList {
				b.WriteString("<ul>\n")
				inList = true
			}
			fmt.Fprintf(&b, "<li>%s</li>\n", inlineMarkdownToHTML(item))
			continue
		}
		paragraph = append(paragraph, trimmed)
	}
	if inCode {
		fmt.Fprintf(&b, "<pre><code>%s</code></pre>\n", stdhtml.EscapeString(strings.TrimSuffix(code.String(), "\n")))
	}
	flushParagraph()
	closeList()
	return strings.TrimSpace(b.String()), nil
}

func markdownListItem(line string) (string, bool) {
	if len(line) < 3 || (line[0] != '-' && line[0] != '*') || !unicode.IsSpace(rune(line[1])) {
		return "", false
	}
	return strings.TrimSpace(line[2:]), true
}

func inlineMarkdownToHTML(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case s[i] == '`':
			if end := strings.IndexByte(s[i+1:], '`'); end >= 0 {
				code := s[i+1 : i+1+end]
				fmt.Fprintf(&b, "<code>%s</code>", stdhtml.EscapeString(code))
				i += end + 2
				continue
			}
		case strings.HasPrefix(s[i:], "**"):
			if end := strings.Index(s[i+2:], "**"); end >= 0 {
				inner := s[i+2 : i+2+end]
				fmt.Fprintf(&b, "<strong>%s</strong>", inlineMarkdownToHTML(inner))
				i += end + 4
				continue
			}
		case s[i] == '[':
			if closeLabel := strings.IndexByte(s[i+1:], ']'); closeLabel >= 0 {
				labelEnd := i + 1 + closeLabel
				if labelEnd+1 < len(s) && s[labelEnd+1] == '(' {
					if closeURL := strings.IndexByte(s[labelEnd+2:], ')'); closeURL >= 0 {
						urlEnd := labelEnd + 2 + closeURL
						label := s[i+1 : labelEnd]
						href := strings.TrimSpace(s[labelEnd+2 : urlEnd])
						if isSafeLinkURL(href) {
							fmt.Fprintf(&b, `<a href="%s">%s</a>`, stdhtml.EscapeString(stdhtml.UnescapeString(href)), inlineMarkdownToHTML(label))
						} else {
							b.WriteString(inlineMarkdownToHTML(label))
						}
						i = urlEnd + 1
						continue
					}
				}
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(stdhtml.EscapeString(string(r)))
		i += size
	}
	return b.String()
}

func markdownHeadingLevel(line string) int {
	level := 0
	for level < len(line) && level < 6 && line[level] == '#' {
		level++
	}
	if level > 0 && level < len(line) && unicode.IsSpace(rune(line[level])) {
		return level
	}
	return 0
}

func normalizeInline(s string) string {
	return normalizeSpace(s)
}

func normalizeSpace(s string) string {
	return strings.TrimSpace(whitespaceRE.ReplaceAllString(s, " "))
}

func isBlank(s string) bool {
	return strings.TrimSpace(s) == ""
}

func checkScrapeContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fetchError(err)
	}
	return nil
}

func truncateUTF8Bytes(s string, max int) (string, bool) {
	if max <= 0 {
		return "", s != ""
	}
	if len(s) <= max {
		return s, false
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}
