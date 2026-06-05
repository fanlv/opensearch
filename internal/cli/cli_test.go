package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fanlv/opensearch/internal/result"
	"github.com/fanlv/opensearch/internal/search"
)

// runCapture executes Run and returns exit code, stdout, and stderr.
func runCapture(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// parseEnv decodes stdout as a single valid envelope JSON object.
func parseEnv(t *testing.T, stdout string) *result.Envelope {
	t.Helper()
	var env result.Envelope
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout contains more than one JSON value:\n%s", stdout)
	}
	return &env
}

func TestRunHelpAndVersion(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		code, out, _ := runCapture(t, []string{arg})
		if code != result.ExitOK {
			t.Errorf("%s exit = %d, want 0", arg, code)
		}
		if strings.HasPrefix(strings.TrimSpace(out), "{") {
			t.Errorf("%s should print human text, not JSON", arg)
		}
	}
	for _, arg := range []string{"--version", "-v", "version"} {
		code, out, _ := runCapture(t, []string{arg})
		if code != result.ExitOK {
			t.Errorf("%s exit = %d, want 0", arg, code)
		}
		if !strings.Contains(out, "opensearch-cli") {
			t.Errorf("%s output missing version banner: %q", arg, out)
		}
	}
}

func TestRunNoArgs(t *testing.T) {
	code, out, _ := runCapture(t, nil)
	env := parseEnv(t, out)
	if env.Success {
		t.Error("no-arg run should fail")
	}
	if env.Error.Code != result.CodeInvalidArgument {
		t.Errorf("error code = %q, want INVALID_ARGUMENT", env.Error.Code)
	}
	if env.Metadata.Command != nil {
		t.Errorf("metadata.command should be null, got %v", *env.Metadata.Command)
	}
	if code != result.ExitUsage {
		t.Errorf("exit = %d, want %d", code, result.ExitUsage)
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	code, out, _ := runCapture(t, []string{"bogus"})
	env := parseEnv(t, out)
	if env.Success || env.Error.Code != result.CodeInvalidArgument {
		t.Errorf("unknown subcommand should be INVALID_ARGUMENT, got %+v", env.Error)
	}
	if env.Metadata.Command != nil {
		t.Error("metadata.command should be null for unknown subcommand")
	}
	if code != result.ExitUsage {
		t.Errorf("exit = %d, want %d", code, result.ExitUsage)
	}
}

func TestRunSearchDoesNotRequireKey(t *testing.T) {
	t.Setenv("EXA_API_KEY", "")
	withFakeSearchClientExpectKey(t, "", func(context.Context, *search.Params) ([]search.RawResult, error) {
		return []search.RawResult{{Title: "Example", URL: "https://example.com/", Snippet: "ok"}}, nil
	})

	code, out, _ := runCapture(t, []string{"search", "hello"})
	env := parseEnv(t, out)
	if code != result.ExitOK || !env.Success {
		t.Fatalf("search without key should succeed through provider path, code=%d env=%+v", code, env)
	}
	if env.Metadata.Command == nil || *env.Metadata.Command != result.CommandSearch {
		t.Error("metadata.command should be search")
	}
	if env.Metadata.ResultCount == nil || *env.Metadata.ResultCount != 1 {
		t.Fatalf("resultCount = %v, want 1", env.Metadata.ResultCount)
	}
}

func TestRunScrapeInvalidConcurrency(t *testing.T) {
	t.Setenv("OPENSEARCH_SCRAPE_CONCURRENCY", "99")
	_, out, _ := runCapture(t, []string{"scrape", "https://example.com"})
	env := parseEnv(t, out)
	if env.Success || env.Error.Code != result.CodeInvalidArgument {
		t.Errorf("invalid concurrency should be INVALID_ARGUMENT (no silent fallback), got %+v", env.Error)
	}
}

func TestRunScrapeInvalidConcurrencyEnvNotSilencedByCLIFlag(t *testing.T) {
	// Invalid scrape env config must fail even when --concurrency is provided.
	// The CLI value only overrides a valid env default.
	t.Setenv("OPENSEARCH_SCRAPE_CONCURRENCY", "bad")
	code, out, _ := runCapture(t, []string{"scrape", "--concurrency", "1", "http://127.0.0.1"})
	env := parseEnv(t, out)
	if code != result.ExitUsage || env.Success || env.Error.Code != result.CodeInvalidArgument {
		t.Fatalf("invalid concurrency env must not be silenced by --concurrency, code=%d env=%+v", code, env)
	}
}

func TestRunSearchParseErrorDoesNotWriteExplicitOutputFile(t *testing.T) {
	t.Setenv("EXA_API_KEY", "test-key")
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "search-error.json")

	code, out, _ := runCapture(t, []string{"search", "-o", outputPath, "--num-results", "999", "golang"})
	stdoutEnv := parseEnv(t, out)
	if code != result.ExitUsage || stdoutEnv.Success || stdoutEnv.Error.Code != result.CodeInvalidArgument {
		t.Fatalf("stdout env = %+v code=%d, want INVALID_ARGUMENT usage failure", stdoutEnv, code)
	}
	// Command-level failures only write one JSON object to stdout and do not
	// write result files or set omission metadata.
	if stdoutEnv.Metadata.ContentOmitted || stdoutEnv.Metadata.OutputPath != "" {
		t.Fatalf("failure metadata = %+v, want no file output markers", stdoutEnv.Metadata)
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("failure must not write the -o file, stat err = %v", err)
	}
}

func TestRunScrapeParseErrorDoesNotWriteExplicitOutputFile(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "scrape-error.json")

	code, out, _ := runCapture(t, []string{"scrape", "-o", outputPath, "--format", "json", "https://example.com"})
	stdoutEnv := parseEnv(t, out)
	if code != result.ExitUsage || stdoutEnv.Success || stdoutEnv.Error.Code != result.CodeInvalidArgument {
		t.Fatalf("stdout env = %+v code=%d, want INVALID_ARGUMENT usage failure", stdoutEnv, code)
	}
	if stdoutEnv.Metadata.ContentOmitted || stdoutEnv.Metadata.OutputPath != "" {
		t.Fatalf("failure metadata = %+v, want no file output markers", stdoutEnv.Metadata)
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("failure must not write the -o file, stat err = %v", err)
	}
}

func TestRunScrapeInvalidURLAsItemFailure(t *testing.T) {
	code, out, _ := runCapture(t, []string{"scrape", "http://exa\\mple.com"})
	env := parseEnv(t, out)
	if code != result.ExitOK || !env.Success {
		t.Fatalf("scrape invalid URL should be command success with item failure, code=%d env=%+v", code, env)
	}
	if env.Metadata.Command == nil || *env.Metadata.Command != result.CommandScrape {
		t.Fatalf("metadata.command = %v, want scrape", env.Metadata.Command)
	}
	if env.Metadata.ResultCount == nil || *env.Metadata.ResultCount != 1 {
		t.Fatalf("resultCount = %v, want 1", env.Metadata.ResultCount)
	}

	data := env.Data.(map[string]interface{})
	items := data["results"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(items))
	}
	item := items[0].(map[string]interface{})
	if item["success"].(bool) {
		t.Fatalf("invalid URL item should fail: %+v", item)
	}
	errObj := item["error"].(map[string]interface{})
	if errObj["code"] != result.CodeInvalidURL {
		t.Fatalf("item error code = %v, want INVALID_URL", errObj["code"])
	}
}

func TestRunScrapeOverlongInvalidURLIsBounded(t *testing.T) {
	longURL := "https://example.com/" + strings.Repeat("a", 9000)
	code, out, _ := runCapture(t, []string{"scrape", longURL})
	env := parseEnv(t, out)
	if code != result.ExitOK || !env.Success {
		t.Fatalf("scrape invalid URL should be command success with item failure, code=%d env=%+v", code, env)
	}
	data := env.Data.(map[string]interface{})
	items := data["results"].([]interface{})
	item := items[0].(map[string]interface{})
	if len(item["url"].(string)) > 600 {
		t.Fatalf("invalid URL preview should be bounded, len=%d", len(item["url"].(string)))
	}
	if item["finalUrl"].(string) != "" {
		t.Fatalf("invalid URL finalUrl should be empty, got %q", item["finalUrl"])
	}
	metadata := item["metadata"].(map[string]interface{})
	if metadata["urlTruncated"] != true {
		t.Fatalf("overlong invalid URL should set urlTruncated metadata: %+v", metadata)
	}
	if strings.Contains(out, strings.Repeat("a", 1000)) {
		t.Fatalf("stdout should not contain the full overlong URL")
	}
}

func TestRunScrapeRawSpaceURLIsInvalid(t *testing.T) {
	code, out, _ := runCapture(t, []string{"scrape", "https://example.com/a b"})
	env := parseEnv(t, out)
	if code != result.ExitOK || !env.Success {
		t.Fatalf("scrape invalid URL should be command success with item failure, code=%d env=%+v", code, env)
	}
	data := env.Data.(map[string]interface{})
	item := data["results"].([]interface{})[0].(map[string]interface{})
	if item["success"].(bool) {
		t.Fatalf("raw space URL item should fail: %+v", item)
	}
	errObj := item["error"].(map[string]interface{})
	if errObj["code"] != result.CodeInvalidURL {
		t.Fatalf("item error code = %v, want INVALID_URL", errObj["code"])
	}
	if item["url"] == "https://example.com/a%20b" {
		t.Fatalf("raw space URL should not be silently normalized: %+v", item)
	}
}

func TestDispatchScrapeCanceledBeforeExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := dispatch(ctx, []string{"scrape", "https://example.com"})
	if res.env.Success || res.env.Error == nil || res.env.Error.Code != result.CodeCanceled {
		t.Fatalf("canceled scrape should be command-level CANCELED, got %+v", res.env)
	}
	if res.env.Data != nil {
		t.Fatalf("canceled command must not return partial data: %+v", res.env.Data)
	}
}

func TestRunScrapeDeduplicatesNormalizedURLs(t *testing.T) {
	code, out, _ := runCapture(t, []string{"scrape", "HTTP://127.0.0.1:80/a#one", "http://127.0.0.1/a#two"})
	env := parseEnv(t, out)
	if code != result.ExitOK || !env.Success {
		t.Fatalf("scrape should be command success, code=%d env=%+v", code, env)
	}
	data := env.Data.(map[string]interface{})
	items := data["results"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("deduped result count = %d, want 1", len(items))
	}
	item := items[0].(map[string]interface{})
	if item["url"] != "http://127.0.0.1/a" {
		t.Fatalf("normalized url = %v, want http://127.0.0.1/a", item["url"])
	}
	errObj := item["error"].(map[string]interface{})
	if errObj["code"] != result.CodeSSRFBlocked {
		t.Fatalf("blocked deduped URL error code = %v, want SSRF_BLOCKED", errObj["code"])
	}
}

func TestRunScrapeURLCountBounds(t *testing.T) {
	code, out, _ := runCapture(t, []string{"scrape"})
	env := parseEnv(t, out)
	if code != result.ExitUsage || env.Success || env.Error.Code != result.CodeInvalidArgument {
		t.Fatalf("zero URL should be INVALID_ARGUMENT usage failure, code=%d env=%+v", code, env)
	}

	args := []string{"scrape"}
	for i := 0; i < 21; i++ {
		args = append(args, "https://example.com/page"+string(rune('a'+i)))
	}
	code, out, _ = runCapture(t, args)
	env = parseEnv(t, out)
	if code != result.ExitUsage || env.Success || env.Error.Code != result.CodeInvalidArgument {
		t.Fatalf(">20 URLs should be INVALID_ARGUMENT usage failure, code=%d env=%+v", code, env)
	}
}

func TestRunSearchKeyNotLeaked(t *testing.T) {
	t.Setenv("EXA_API_KEY", "test-key")
	t.Setenv("OPENSEARCH_SCRAPE_CONCURRENCY", "abc") // search must not parse scrape-only config
	withFakeSearchClient(t, func(context.Context, *search.Params) ([]search.RawResult, error) {
		return nil, &search.ProviderError{Code: result.CodeProviderAuth, Msg: "auth failed"}
	})
	_, out, errb := runCapture(t, []string{"search", "q"})
	if strings.Contains(out, "test-key") || strings.Contains(errb, "test-key") {
		t.Error("EXA_API_KEY leaked into output")
	}
	env := parseEnv(t, out)
	if env.Error == nil || env.Error.Code != result.CodeProviderAuth {
		t.Fatalf("search should ignore scrape-only config and reach provider path, got %+v", env.Error)
	}
}

type fakeSearchRunner struct {
	fn func(context.Context, *search.Params) ([]search.RawResult, error)
}

func (f fakeSearchRunner) Search(ctx context.Context, p *search.Params) ([]search.RawResult, error) {
	return f.fn(ctx, p)
}

func withFakeSearchClient(t *testing.T, fn func(context.Context, *search.Params) ([]search.RawResult, error)) {
	t.Helper()
	withFakeSearchClientExpectKey(t, "test-key", fn)
}

func withFakeSearchClientExpectKey(t *testing.T, expectedKey string, fn func(context.Context, *search.Params) ([]search.RawResult, error)) {
	t.Helper()
	orig := newSearchClient
	newSearchClient = func(apiKey string) searchRunner {
		if apiKey != expectedKey {
			t.Fatalf("api key = %q, want %q", apiKey, expectedKey)
		}
		return fakeSearchRunner{fn: fn}
	}
	t.Cleanup(func() { newSearchClient = orig })
}

func TestRunSearchSuccess(t *testing.T) {
	t.Setenv("EXA_API_KEY", "test-key")
	withFakeSearchClient(t, func(_ context.Context, p *search.Params) ([]search.RawResult, error) {
		if p.Query != "golang release" {
			t.Fatalf("query = %q, want golang release", p.Query)
		}
		if p.NumResults != 2 {
			t.Fatalf("numResults = %d, want 2", p.NumResults)
		}
		return []search.RawResult{
			{Title: "Go", URL: "HTTPS://go.dev/doc/#top", Snippet: "official"},
			{Title: "Duplicate", URL: "https://go.dev/doc/"},
			{Title: "Other", URL: "https://example.com/path", Snippet: "example"},
		}, nil
	})

	code, out, _ := runCapture(t, []string{"search", "--num-results", "2", "golang", "release"})
	env := parseEnv(t, out)
	if code != result.ExitOK || !env.Success {
		t.Fatalf("search should succeed, code=%d env=%+v", code, env)
	}
	if env.Metadata.Command == nil || *env.Metadata.Command != result.CommandSearch {
		t.Fatalf("metadata.command = %v, want search", env.Metadata.Command)
	}
	if env.Metadata.ResultCount == nil || *env.Metadata.ResultCount != 2 {
		t.Fatalf("resultCount = %v, want 2", env.Metadata.ResultCount)
	}

	data := env.Data.(map[string]interface{})
	results := data["results"].([]interface{})
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	first := results[0].(map[string]interface{})
	if first["url"] != "https://go.dev/doc/#top" {
		t.Fatalf("first url = %v, want normalized https://go.dev/doc/#top", first["url"])
	}
}

func TestRunSearchProviderError(t *testing.T) {
	t.Setenv("EXA_API_KEY", "test-key")
	withFakeSearchClient(t, func(context.Context, *search.Params) ([]search.RawResult, error) {
		return nil, &search.ProviderError{Code: result.CodeProviderRateLimited, Msg: "search provider rate limited the request"}
	})

	code, out, _ := runCapture(t, []string{"search", "hello"})
	env := parseEnv(t, out)
	if code != result.ExitError {
		t.Fatalf("exit = %d, want %d", code, result.ExitError)
	}
	if env.Success || env.Error.Code != result.CodeProviderRateLimited {
		t.Fatalf("provider error = %+v, want PROVIDER_RATE_LIMITED", env.Error)
	}
}

func TestRunSearchExplicitOutputFile(t *testing.T) {
	t.Setenv("EXA_API_KEY", "test-key")
	withFakeSearchClient(t, func(context.Context, *search.Params) ([]search.RawResult, error) {
		return []search.RawResult{{Title: "Go", URL: "https://go.dev/", Snippet: "large snippet"}}, nil
	})

	dir := t.TempDir()
	outputPath := filepath.Join(dir, "result.json")
	stdoutPath := filepath.Join(dir, "stdout.json")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	code := Run([]string{"search", "-o", outputPath, "golang"}, stdout, &bytes.Buffer{})
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	if code != result.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}

	stdoutBytes, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	stdoutEnv := parseEnv(t, string(stdoutBytes))
	if !stdoutEnv.Metadata.ContentOmitted || stdoutEnv.Metadata.OutputPath != outputPath {
		t.Fatalf("stdout metadata = %+v, want omitted with output path", stdoutEnv.Metadata)
	}
	if strings.Contains(string(stdoutBytes), "large snippet") {
		t.Fatalf("stdout summary should omit snippets: %s", stdoutBytes)
	}

	fileBytes, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fileBytes), "large snippet") {
		t.Fatalf("full output file should keep snippet: %s", fileBytes)
	}
}

func TestRunSearchExplicitOutputFileWithBufferStdout(t *testing.T) {
	t.Setenv("EXA_API_KEY", "test-key")
	withFakeSearchClient(t, func(context.Context, *search.Params) ([]search.RawResult, error) {
		return []search.RawResult{{Title: "Go", URL: "https://go.dev/", Snippet: "large snippet"}}, nil
	})

	dir := t.TempDir()
	outputPath := filepath.Join(dir, "result.json")
	code, out, _ := runCapture(t, []string{"search", "-o", outputPath, "golang"})
	if code != result.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	stdoutEnv := parseEnv(t, out)
	if !stdoutEnv.Metadata.ContentOmitted || stdoutEnv.Metadata.OutputPath != outputPath {
		t.Fatalf("stdout metadata = %+v, want omitted with output path", stdoutEnv.Metadata)
	}
	if strings.Contains(out, "large snippet") {
		t.Fatalf("stdout summary should omit snippets: %s", out)
	}
	fileBytes, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fileBytes), "large snippet") {
		t.Fatalf("full output file should keep snippet: %s", fileBytes)
	}
}
