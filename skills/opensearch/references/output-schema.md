# Output schema and errors

Except help/version aliases (`--help`, `-h`, `help`, `--version`, `-v`, `version`), `opensearch-cli` prints one JSON object to stdout. Successful large or explicitly persisted results may also write a complete JSON file.

## Top-level envelope

```json
{
  "success": true,
  "data": {},
  "error": null,
  "metadata": {
    "command": "search",
    "durationMs": 123,
    "resultCount": 3,
    "outputPath": "/abs/path/result.json",
    "contentOmitted": true
  }
}
```

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `success` | boolean | yes | Command-level success. For `scrape`, this does not imply every URL succeeded. |
| `data` | object/array/null | yes | Successful command result; `null` on command-level failure. |
| `error` | object/null | yes | Stable command-level error; `null` on command-level success. |
| `metadata` | object | yes | Command metadata, timing, output-file and omission flags. |

## Metadata

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `command` | `"search"` / `"scrape"` / `null` | yes | Recognized subcommand; `null` when no subcommand is determined. |
| `durationMs` | integer | yes | Elapsed wall-clock time in milliseconds. |
| `resultCount` | integer | no | Number of search results or scrape result items when applicable. |
| `outputPath` | string | no | Absolute path to complete JSON when output was written to file. |
| `contentOmitted` | boolean | no | `true` when stdout is a summary rather than the complete result, including explicit `-o/--output` or automatic omission of large `snippet`/`content` and other variable-length fields. |

If `contentOmitted=true`, consumers must read `outputPath` and use the complete file envelope instead of the stdout summary before using any command data or error details. If the file cannot be read, treat the command as failed.

## Error object

```json
{
  "code": "INVALID_ARGUMENT",
  "message": "human-readable stable explanation"
}
```

`message` is safe for models and users. It must not contain `EXA_API_KEY`, provider raw response bodies, credentials, or sensitive environment values.

## `search` data

```json
{
  "results": [
    {
      "title": "Page title",
      "titleTruncated": true,
      "url": "https://example.com/page",
      "publishedDate": "2026-06-04T00:00:00Z",
      "publishedDateTruncated": true,
      "snippet": "optional extractive highlight"
    }
  ]
}
```

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `results` | array | yes | Candidate sources after URL normalization, domain filtering, and deduplication. |
| `title` | string | no | Provider title, length-limited to 1024 UTF-8 bytes if needed. |
| `titleTruncated` | boolean | no | Present and `true` when `title` was truncated by the CLI length limit. |
| `url` | string | yes | Normalized HTTP(S) URL; never truncated into an invalid URL. |
| `publishedDate` | string | no | Provider publication timestamp/string when available; length-limited to 128 UTF-8 bytes if needed. |
| `publishedDateTruncated` | boolean | no | Present and `true` when `publishedDate` was truncated by the CLI length limit. |
| `snippet` | string | no | Optional provider highlight; complete search results also cap it at 4096 UTF-8 bytes, and stdout summaries may omit it. |

An empty `results` array is command-level success.

## `scrape` data

```json
{
  "results": [
    {
      "success": true,
      "url": "https://example.com/original",
      "finalUrl": "https://example.com/final",
      "title": "Page title",
      "titleTruncated": true,
      "format": "markdown",
      "content": "# cleaned body",
      "metadata": {
        "mainContentExtracted": true,
        "fallbackUsed": false,
        "mediaType": "text/html",
        "contentType": "text/html; charset=utf-8",
        "contentTypeTruncated": true,
        "contentEncoding": "identity",
        "statusCode": 200,
        "bytes": 1256
      },
      "error": null
    }
  ]
}
```

Per-item failure shape:

```json
{
  "success": false,
  "url": "https://example.com/original",
  "finalUrl": "https://example.com/original",
  "title": "",
  "format": "markdown",
  "content": "",
  "metadata": {},
  "error": {
    "code": "HTTP_STATUS_ERROR",
    "message": "received non-2xx HTTP status"
  }
}
```

| Field | Type | Required | Meaning |
| --- | --- | --- | --- |
| `results` | array | yes | Per-URL results in deduplicated input order. |
| `success` | boolean | yes | Per-item success. |
| `url` | string | yes | Original normalized input URL for the item; for unnormalizable invalid input, a bounded diagnostic preview. |
| `finalUrl` | string | yes | Final URL after redirects on success; best-effort normalized/current URL on fetch failures; empty when no safe normalized/final URL exists. |
| `title` | string | no | Extracted page title when available, length-limited to 1024 UTF-8 bytes if needed. |
| `titleTruncated` | boolean | no | Present and `true` when `title` was truncated by the CLI length limit. |
| `format` | `markdown` / `text` / `html` | yes | Output format. |
| `content` | string | yes in complete results; may be omitted in stdout summaries | Cleaned/converted body on success; empty string on failure. |
| `metadata` | object | no | Per-item diagnostics. On success it includes `mediaType` (parsed media type), `contentType` (raw response `Content-Type`, length-limited to 1024 UTF-8 bytes with `contentTypeTruncated` when shortened), `contentEncoding` (`identity`/`gzip`/`br`), `statusCode`, `bytes` (decoded body size), and HTML extraction flags `mainContentExtracted` / `fallbackUsed`. Empty (`{}`) on most failures; may carry bounded diagnostics such as `urlTruncated` for unnormalizable input. |
| `error` | object/null | yes | Per-item error when `success=false`. |

Top-level `success=true` for `scrape` means the batch was scheduled and returned item statuses. It may contain zero successful items.

## File output and omission

- Direct stdout threshold: complete serialized JSON up to 256 KiB (exactly 262144 bytes) is returned directly. This threshold is fixed at 256 KiB / 262144 bytes and is not configurable via any flag or environment variable.
- If command-level `success=true` and either `-o/--output` is specified or full JSON exceeds 256 KiB (262144 bytes), the complete JSON is written to a file and stdout returns a summary envelope. Command-level failures are printed only to stdout and do not write result files.
- Explicit output path:
  - Existing regular files are atomically replaced.
  - Directories, symlinks, non-regular files, and unsafe paths are rejected with `OUTPUT_WRITE_ERROR`.
- Automatic output path:
  - Directory is `OPENSEARCH_OUTPUT_DIR` or `.opensearch/`.
  - File names are non-conflicting, e.g. `opensearch-00000.json`, `opensearch-00001.json`.
  - Existing files are never overwritten.
- Explicit output writes use temp-file + fsync + rename replacement. Automatic output writes use temp-file + fsync + atomic no-replace hard link creation. Failures must not leave files that look like complete results.
- Result files contain complete JSON and record their own absolute path in `metadata.outputPath`.
- Stdout summary omission order:
  1. Omit or shorten large `snippet` / `content` fields.
  2. If still too large, omit nonessential variable-length metadata.
  3. In extreme cases, preserve required fields: result item order, URL/finalUrl, success state, and error code.
- Valid URLs must never be truncated into invalid values, and result items must not be dropped just to fit the threshold. For invalid inputs that cannot be normalized, bounded diagnostic previews may be used instead of echoing the full raw input.

## Command-level error codes

| Code | Retryable | Exit | Meaning |
| --- | --- | --- | --- |
| `INVALID_ARGUMENT` | no | 2 | Invalid command, option, config value, argument count, domain conflict, time range, or output path argument. |
| `CONFIG_REQUIRED` | no | 2 | Required command configuration is missing. `search` does not require `EXA_API_KEY`. |
| `PROVIDER_AUTH_ERROR` | no | 1 | Provider rejected authentication/authorization. |
| `PROVIDER_RATE_LIMITED` | yes, within skill retry budget | 1 | Provider rate limit. |
| `PROVIDER_UNAVAILABLE` | yes, within skill retry budget | 1 | Provider timeout/network/service unavailable, including HTTP `408`. |
| `PROVIDER_ERROR` | maybe, within skill retry budget if transient | 1 | Other stable provider failure or invalid provider response. |
| `OUTPUT_WRITE_ERROR` | no | 1 | Could not safely write explicit or automatic output file. |
| `CANCELED` | no | 1 | Process cancellation; no partial command-level result should be used. |
| `INTERNAL_ERROR` | no | 1 | Unexpected implementation failure. |

`--help`, `--version`, and any command-level `success=true` exit `0`.

## Per-URL scrape error codes

Per-URL scrape errors live in each result item’s `error`. They do not by themselves make top-level `success=false`.

| Code | Retryable | Meaning |
| --- | --- | --- |
| `INVALID_URL` | no | URL or redirect target is invalid, non-HTTP(S), ambiguous, has userinfo, or violates URL rules. |
| `SSRF_BLOCKED` | no | Host, DNS result, redirect, or constrained-connect check indicates a non-public/restricted target. |
| `HTTP_STATUS_ERROR` | no | Missing redirect location, unsupported 3xx, or final non-2xx status. |
| `SCRAPE_TIMEOUT` | maybe | Per-URL timeout expired. |
| `TASK_TIMEOUT` | maybe | Batch timeout expired before item completion. |
| `NETWORK_ERROR` | maybe | DNS/connect/TLS/read/decode transport failure or corrupt supported encoding. |
| `TOO_MANY_REDIRECTS` | no | Redirect loop or more than 5 redirects. |
| `RESPONSE_TOO_LARGE` | no | `Content-Length` or decompressed body exceeds 5 MB. |
| `UNSUPPORTED_CONTENT_TYPE` | no | Missing/unrecognized/unsupported content type. |
| `UNSUPPORTED_CONTENT_ENCODING` | no | Unsupported, repeated, or multilayer content encoding. |
| `UNSUPPORTED_CHARSET` | no | Non-UTF-8 charset or invalid UTF-8. |
| `EMPTY_CONTENT` | no | Cleaned/extracted body is empty. |
| `CONVERSION_ERROR` | no | HTML/Markdown cleaning, parsing, extraction, or conversion failed. |

Transient per-item errors only indicate that the underlying failure may be temporary. The skill does not directly retry the same URL by default; use successful bodies, try other already returned candidates, or spend the shared search retry budget for substitute sources when the task requires it. Do not retry deterministic validation, SSRF, unsupported content, or content-too-large failures.
