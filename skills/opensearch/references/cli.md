# opensearch-cli reference

`opensearch-cli` searches public Web sources and scrapes public, anonymous HTTP(S) pages. Except help/version aliases (`--help`, `-h`, `help`, `--version`, `-v`, `version`), every invocation writes exactly one JSON object to stdout.

## Installation and discovery

- Binary name: `opensearch-cli`.
- The binary must be discoverable through `PATH`.
- Verify with `opensearch-cli --version` and `opensearch-cli --help` before first use.
- Skill install is managed by `npx skills add`. The default `make install-skill`
  installs globally through `~/.agents/skills/opensearch` and supported agent
  symlinks for Claude Code, Codex, OpenCode, Trae, and Trae CN. Override
  `SKILL_AGENTS` to choose a different agent set.

## Commands

```bash
opensearch-cli --help
opensearch-cli --version
opensearch-cli search [options] <query>
opensearch-cli scrape [options] <url> [url...]
```

## Shared behavior

- Missing or unknown subcommands return command-level `INVALID_ARGUMENT` with `metadata.command=null`.
- Once a subcommand is recognized, `metadata.command` is `search` or `scrape`.
- Root help/version aliases (`--help`, `-h`, `help`, `--version`, `-v`, `version`) are human-readable and exit `0`; all other invocations use JSON.
- Complete JSON may be written to a file either by explicit `-o/--output` or automatic large-result spillover.
- Explicit `-o/--output` requires a non-empty path; `--output=` and blank values return `INVALID_ARGUMENT`.

## Shared URL rules

URL normalization and validation are used for search-result normalization, scrape input deduplication, redirects, and request safety checks.

- Raw and normalized full URL must each be at most 8192 UTF-8 bytes and fully parseable by the standard parser.
- Only `http` and `https` URLs are valid for scraping.
- Scheme and host are lowercased.
- Internationalized domains are converted to ASCII.
- A trailing root dot on hostnames is ignored.
- IP literals are normalized, including IPv4-mapped IPv6.
- Default ports are removed (`80` for HTTP, `443` for HTTPS).
- Fragment is ignored for scraping and deduplication.
- Path and query order/semantics are preserved. Percent encoding is not decoded or reordered to merge resources.
- Valid IPv4 literals must be four decimal octets. Integer, octal, hex, or fewer-than-four-octet IPv4 forms are invalid.
- IPv6 literals must use bracket form.
- Reject ambiguous or unsafe forms: userinfo for scraping, control characters, raw spaces and other raw characters that would be silently escaped by the URL serializer, invalid percent encoding, backslashes, encoded delimiters in path/query (`:`, `/`, `?`, `#`, `[`, `]`, `@`, `\\`), IPv6 zone identifiers, and authority forms that can be interpreted as different scheme/host/port/path.

## `search`

Search uses Exa MCP. `EXA_API_KEY` is optional; when present, it is sent to Exa MCP as the `exaApiKey` query parameter.

### Options

| Option | Meaning | Bounds/default |
| --- | --- | --- |
| `<query>` | Search query after trimming and joining positional tokens | `1..2048` Unicode characters |
| `-n, --num-results <n>` | Maximum returned candidates | default `8`, range `1..20` |
| `--published-after <rfc3339>` | Publication start filter | RFC 3339 |
| `--published-before <rfc3339>` | Publication end filter | RFC 3339; start must not be later than end |
| `--include-domain <domain>` | Include domain; repeatable | DNS hostname only, max 20 (counted before dedup) |
| `--exclude-domain <domain>` | Exclude domain; repeatable | DNS hostname only, max 20 (counted before dedup) |
| `-o, --output <path>` | Write complete JSON to this file | safe regular-file replacement only |

### Domain rules

- Domains are normalized to lowercase ASCII DNS hostnames with trailing root dot ignored; IP literals are rejected.
- `example.com` matches `example.com` and any subdomain.
- The same normalized domain cannot appear in both include and exclude sets; return `INVALID_ARGUMENT`.
- Parent and child domains may both appear; if a result matches both include and exclude, exclude wins.

### Result behavior

- Provider requests call the `web_search_exa` MCP tool.
- Provider request timeout is 25 seconds.
- The MCP request sends `query` and `numResults`. Domain constraints are appended to the query with `site:` / `-site:` terms and are also enforced again after results return.
- Exa MCP text highlights may populate `snippet`; missing snippets do not fail a result.
- Published date filters are enforced locally for results with parseable provider dates; results with unparseable or missing dates are dropped when a date filter is requested.
- Provider results are filtered again by domain rules, normalized by URL rules, deduplicated, and kept in provider order.
- Results without valid HTTP(S) URLs are dropped.
- No results is a successful command with an empty list.
- Provider auth, rejection, rate limit, timeout, network error, and invalid response are command-level errors.
- A `200` Provider response must contain a JSON `results` array; missing, `null`, or otherwise invalid `results` is `PROVIDER_ERROR`. `results: []` is the valid empty-result response.
- Search snippets are only candidate summaries, not page bodies.

## `scrape`

Scrape fetches anonymous public HTTP(S) pages. It does not require external configuration.

### Options

| Option | Meaning | Bounds/default |
| --- | --- | --- |
| `--format <markdown|text|html>` | Output format | default `markdown` |
| `--main-content` / `--no-main-content` | Enable/disable main-body extraction for HTML | default enabled |
| `--timeout <seconds>` | Per-URL timeout | default `30`, range `1..120` |
| `--total-timeout <seconds>` | Batch timeout | default `180`, range `1..600` |
| `--concurrency <n>` | Batch concurrency | default from env or `4`, range `1..16` |
| `-o, --output <path>` | Write complete JSON to this file | safe regular-file replacement only |

### Batch rules

- Each command accepts 1 to 20 URL inputs. Zero or more than 20 is `INVALID_ARGUMENT`.
- URL format/protocol/userinfo/SSRF checks are per item and do not block other items.
- Invalid URLs return per-item `INVALID_URL`; restricted targets return per-item `SSRF_BLOCKED`. Invalid URL outputs use a bounded diagnostic `url` preview and an empty `finalUrl` when no normalized/final URL is available.
- Normalized duplicate scrape URLs are fetched once and keep the first position.
- Output order matches deduplicated input order, independent of concurrent completion order.
- Even if every item fails, top-level `success` remains `true`; inspect per-item `success`.

### Timeout rules

- Per-URL timeout starts with that item’s URL/SSRF validation and runs through DNS, connect, TLS, redirect, response read, decompression, decode, parse, extraction, and conversion. It returns `SCRAPE_TIMEOUT`.
- Batch timeout starts after command argument validation and before scheduling. Completed results are kept; unfinished items return `TASK_TIMEOUT`.
- If both timeouts could apply, whichever fires first decides the item error code.

### Content types

| Content type | Behavior |
| --- | --- |
| `text/html`, `application/xhtml+xml` | Clean untrusted HTML, extract main body by default, convert to requested format. Extraction failure falls back to cleaned full document. |
| `text/plain` | Normalize UTF-8. Markdown/text output keeps text; HTML output escapes text. |
| `text/markdown`, `text/x-markdown` | Normalize UTF-8; clean embedded raw HTML and URLs. Markdown output returns cleaned Markdown; text extracts visible text; HTML converts then cleans. |
| Missing/unrecognized `Content-Type` | `UNSUPPORTED_CONTENT_TYPE`; no sniffing. |
| PDF, images, audio/video, JSON, XML, RSS, CSV, other binary/structured data | `UNSUPPORTED_CONTENT_TYPE`. |

Only UTF-8 and UTF-8 BOM are supported. Other charsets or invalid UTF-8 return `UNSUPPORTED_CHARSET`.

### HTTP and redirect rules

- Only anonymous `GET` requests are sent.
- Follow only `301`, `302`, `303`, `307`, and `308`.
- Missing/empty `Location` returns `HTTP_STATUS_ERROR`.
- Non-HTTP(S), unparsable, or invalid redirect targets return `INVALID_URL`.
- Restricted redirect targets return `SSRF_BLOCKED`.
- Each redirect hop repeats URL and SSRF validation.
- Redirect loops or more than 5 redirects return `TOO_MANY_REDIRECTS`.
- Other 3xx and final non-2xx responses return `HTTP_STATUS_ERROR` with no body.
- `Content-Length` and decompressed body must not exceed 5 MB; exceedance returns `RESPONSE_TOO_LARGE`.
- Supported encodings: `identity`, single-layer `gzip`, single-layer `br`.
- Multi-layer/repeated/other encodings return `UNSUPPORTED_CONTENT_ENCODING`; corrupt encoded content returns `NETWORK_ERROR`.

## Environment variables

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `EXA_API_KEY` | no | none | Optional Exa MCP API key; never printed or written |
| `OPENSEARCH_OUTPUT_DIR` | no | `.opensearch/` | Directory for automatic large-result files |
| `OPENSEARCH_USER_AGENT` | no | `opensearch-cli/<version>` | Scrape User-Agent; must be valid single-line HTTP header value |
| `OPENSEARCH_SCRAPE_CONCURRENCY` | no | `4` | Default scrape concurrency, range `1..16` |

Configuration precedence for non-sensitive setting values: CLI option > valid environment variable > default. Empty optional environment variables are treated as unset. Invalid or out-of-range values for a command's own configuration return `INVALID_ARGUMENT` even if a CLI option would otherwise override that setting; no silent fallback. `search` does not parse scrape-only variables such as `OPENSEARCH_USER_AGENT` or `OPENSEARCH_SCRAPE_CONCURRENCY`.

Proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`) are intentionally ignored by both `search` provider requests and `scrape` target requests.

## Public-target and SSRF safety

Scraping is limited to public HTTP(S) targets.

- URL must have a hostname and no userinfo.
- Block single-label hostnames such as `intranet`, except IP literals.
- Block `localhost` and its subdomains.
- Block cloud metadata hostnames including `metadata.google.internal`, `instance-data.ec2.internal`, and normalized equivalents.
- Block metadata and special-purpose addresses, including `169.254.169.254`, `168.63.129.16` (Azure wireserver), and IPv6/IPv4-mapped equivalents.
- DNS A/AAAA results must all be public global unicast and must not fall in IANA IPv4/IPv6 Special-Purpose Address Space categories: unspecified, loopback, private, shared address space, link-local, multicast, documentation, benchmark, reserved, and related non-public ranges.
- Initial requests and every redirect re-check DNS and bind the request to the verified public address set.
- Connections must use only verified addresses; if the implementation cannot constrain the actual connection to verified addresses, it must return `SSRF_BLOCKED` before sending a request.
- HTTP Host, TLS SNI, and certificate validation still use the normalized hostname.
- No cookies, Authorization headers, Proxy-Authorization headers, `.netrc`, client certificates, browser credentials, or system credentials are used. `EXA_API_KEY` is never sent to scraped targets.

Reference registries used by the denylist:

- IANA IPv4 Special-Purpose Address Registry: `https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry.xhtml`
- IANA IPv6 Special-Purpose Address Space Registry: `https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry.xhtml`

The hard-coded denylist prefixes in `internal/scrape/ssrf.go` should be reviewed and refreshed when IANA adds or changes special-purpose prefixes. The IPv6 denylist includes currently registered non-public/special ranges such as `64:ff9b::/96`, `64:ff9b:1::/48`, `100::/64`, `100:0:0:1::/64`, `2001::/23`, `2001:db8::/32`, `2002::/16`, `3fff::/20`, `5f00::/16`, `2620:4f:8000::/48`, `fc00::/7`, `fe80::/10`, and multicast/reserved equivalents.

## Untrusted content cleanup

Provider content, search snippets, and scraped bodies are untrusted. Output must not leak API keys or raw provider error bodies.

HTML input, Markdown embedded raw HTML, Markdown URLs, and generated HTML share the same safety boundary.

Remove or neutralize at least:

- Executable/active elements (removed entirely, including their subtree, regardless of attributes): `script`, `style`, `noscript`, `template`, `iframe`, `object`, `embed`, `applet`, `canvas`, `svg`, `math`.
- Network/submission/embed elements that auto-fetch or submit data: `form`, `input`, `button`, `select`, `textarea`, `meta refresh`, `link`, `base`, `img`, `video`, `audio`, `source`, `track`, `picture`.
- Event-handler attributes (`on*`), inline `style`, `srcdoc`, dangerous protocols (`javascript:`, `data:`, `vbscript:`), and parser-base-changing constructs.
- Markdown image syntax and raw HTML that would auto-fetch or execute.

Allowed preserved attributes are limited to safe text metadata such as `title`, `alt`, `lang`, `dir`, `aria-*`, and safe `href` values. Dropped attributes include event handlers (`on*`), `style`, `srcdoc`, `src`, `srcset`, `action`, `formaction`, `poster`, `background`, and `xlink:href`.

Markdown cleanup covers inline links/images, reference-style links/images, autolinks, and raw HTML. Image syntax is removed because it may auto-fetch. Unsafe links are downgraded to label text.

Safe links may be preserved only when they do not auto-request the target. Allowed URL protocols are relative URLs, `http`, `https`, and `mailto`; URL checks decode HTML entities before validating the scheme. Dangerous or ambiguous protocols such as `javascript:`, `data:`, and `vbscript:` are removed or neutralized.
When HTML links are converted to Markdown, link labels and destinations are escaped/encoded so relative URLs cannot break out of the Markdown destination and inject a second link.
