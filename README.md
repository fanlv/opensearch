# opensearch

`opensearch` provides a small CLI and agent skill for open Web search and public
HTTP(S) page scraping. It is not related to the OpenSearch search-engine product:
the name here means "open Web search".

The repository contains:

- `opensearch-cli`: a JSON-first command line tool.
- `skills/opensearch`: an agent skill that calls `opensearch-cli`.

## Features

- Search public Web sources through Exa MCP.
- Search works without `EXA_API_KEY`; when set, the key is passed to Exa MCP as
  an optional credential.
- Scrape anonymous public HTTP(S) pages into Markdown, text, or cleaned HTML.
- Stable JSON envelopes for agent consumption.
- SSRF protections for scrape requests, including public-DNS validation and
  redirect re-checks.
- Automatic large-output spillover to files, with a compact stdout summary.

## Requirements

- Go, for building and testing `opensearch-cli`.
- Node.js / `npx`, for installing the skill with `npx skills`.
- `$(HOME)/.local/bin` on `PATH` if you use the default CLI install location.

## Build And Test

```bash
make build
make test
```

These are the normal local development checks. They do not run the external
agent integration flow.

## Advanced Verification

The smoke targets are optional integration checks. Use them before release, when
changing installation behavior, or when verifying the external providers and
agent runtime end to end.

```bash
make smoke
make smoke-exa
make smoke-codex-exec
make smoke-strict
```

- `make smoke` runs unit tests, builds the CLI, checks help/version output,
  verifies search without `EXA_API_KEY`, scrapes `https://example.com`, checks
  SSRF blocking, verifies output-file behavior, and checks Codex skill
  visibility when `codex` is installed.
- `make smoke-exa` performs a real Exa MCP search. It does not require
  `EXA_API_KEY`, but it does require network access to Exa MCP.
- `make smoke-codex-exec` runs an end-to-end Codex execution against the
  `opensearch` skill. It requires the Codex CLI and network access.
- `make smoke-strict` runs all smoke targets.

## Install

Install both the CLI and the skill:

```bash
make install
```

The default install log is concise. Use verbose output when troubleshooting:

```bash
make install VERBOSE=1
```

Install only the CLI:

```bash
make install-cli
```

By default, `install-cli` writes:

```text
$(HOME)/.local/bin/opensearch-cli
```

Override the destination with:

```bash
make install-cli INSTALL_BIN_DIR=/path/to/bin
```

Install only the skill:

```bash
make install-skill
```

`install-skill` uses `npx skills add` and installs globally through:

```text
$(HOME)/.agents/skills/opensearch
```

The default target agents are:

```text
claude-code codex opencode trae trae-cn
```

Choose a different set with:

```bash
make install-skill SKILL_AGENTS="codex claude-code"
```

Install to every agent supported by `npx skills`:

```bash
make install-skill-all
```

Copy instead of symlinking:

```bash
make install-skill-copy
```

List the skills discoverable from this repository:

```bash
make install-skill-list
```

Clean up the old Codex-only copy location:

```bash
make remove-legacy-codex-skill
```

This avoids duplicate `opensearch` skill loading when both the legacy
`$(HOME)/.codex/skills/opensearch` copy and the new global skill installation
exist.

## CLI Usage

```bash
opensearch-cli --help
opensearch-cli --version
```

Search:

```bash
opensearch-cli search "weather in Beijing today"
opensearch-cli search -n 3 "latest Go release notes"
opensearch-cli search --include-domain go.dev "Go memory model"
opensearch-cli search --exclude-domain example.com "public web search"
```

Scrape:

```bash
opensearch-cli scrape https://example.com
opensearch-cli scrape --format text https://example.com
opensearch-cli scrape --format html --no-main-content https://example.com
opensearch-cli scrape -o page.json https://example.com
```

Every normal command writes one JSON object to stdout. Help and version commands
are human-readable.

## Output Shape

Successful command:

```json
{
  "success": true,
  "data": {},
  "error": null,
  "metadata": {
    "command": "search"
  }
}
```

Failed command:

```json
{
  "success": false,
  "data": null,
  "error": {
    "code": "INVALID_ARGUMENT",
    "message": "..."
  },
  "metadata": {
    "command": "search"
  }
}
```

For `scrape`, individual URLs can fail while the top-level command still
returns `success: true`; inspect each item result.

## Environment

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `EXA_API_KEY` | no | none | Optional Exa MCP API key for `search` |
| `OPENSEARCH_OUTPUT_DIR` | no | `.opensearch/` | Directory for automatic large-result files |
| `OPENSEARCH_USER_AGENT` | no | `opensearch-cli/<version>` | Scrape User-Agent |
| `OPENSEARCH_SCRAPE_CONCURRENCY` | no | `4` | Default scrape concurrency |

`search` ignores scrape-only environment variables. Proxy variables
(`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`) are intentionally ignored by both
search provider requests and scrape target requests.

## References

- Skill entrypoint: `skills/opensearch/SKILL.md`
- CLI reference: `skills/opensearch/references/cli.md`
- Output schema notes: `skills/opensearch/references/output-schema.md`
