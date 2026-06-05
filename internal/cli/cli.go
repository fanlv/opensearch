// Package cli implements root command dispatch, common timing/cancellation
// behavior, and stable JSON envelope output for opensearch-cli.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/fanlv/opensearch/internal/config"
	"github.com/fanlv/opensearch/internal/output"
	"github.com/fanlv/opensearch/internal/result"
	"github.com/fanlv/opensearch/internal/scrape"
	"github.com/fanlv/opensearch/internal/search"
)

// newSearchClient builds the search client; tests replace it with a stub.
var newSearchClient = func(apiKey string) searchRunner {
	return search.NewClient(apiKey)
}

// searchRunner is the minimal client interface used by cli.
type searchRunner interface {
	Search(ctx context.Context, p *search.Params) ([]search.RawResult, error)
}

type commandResult struct {
	env        *result.Envelope
	outputOpts output.Options
}

// Version is set by main via ldflags.
var Version = "0.1.0-dev"

// Run is the CLI entry point. Except --help and --version, every invocation
// writes one JSON object to stdout and returns a stable exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "--help", "-h", "help":
			printUsage(stdout)
			return result.ExitOK
		case "--version", "-v", "version":
			fmt.Fprintf(stdout, "opensearch-cli %s\n", Version)
			return result.ExitOK
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	start := time.Now()
	res := dispatch(ctx, args)
	env := res.env
	env.Metadata.DurationMs = time.Since(start).Milliseconds()

	if err := emit(stdout, env, res.outputOpts); err != nil {
		fmt.Fprintf(stderr, "opensearch-cli: failed to write output: %v\n", err)
		return result.ExitError
	}
	return env.ExitCode()
}

// dispatch selects and runs the subcommand. Unknown commands keep metadata.command null.
func dispatch(ctx context.Context, args []string) commandResult {
	if len(args) == 0 {
		return commandResult{env: result.NewFailure(nil, result.CodeInvalidArgument, "missing subcommand: expected 'search' or 'scrape'")}
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "search":
		return runSearch(ctx, rest)
	case "scrape":
		return runScrape(ctx, rest)
	default:
		if strings.HasPrefix(sub, "-") {
			return commandResult{env: result.NewFailure(nil, result.CodeInvalidArgument,
				fmt.Sprintf("unexpected option %q before subcommand", sub))}
		}
		return commandResult{env: result.NewFailure(nil, result.CodeInvalidArgument,
			fmt.Sprintf("unknown subcommand %q: expected 'search' or 'scrape'", sub))}
	}
}

// runSearch parses args, calls the provider, normalizes results, and emits output.
func runSearch(ctx context.Context, args []string) commandResult {
	cmd := result.CommandPtr(result.CommandSearch)

	cfg, err := config.LoadSearch(os.Getenv, Version)
	if err != nil {
		return commandResult{env: result.NewFailure(cmd, result.CodeInvalidArgument, err.Error())}
	}
	p, err := search.ParseArgs(args)
	if err != nil {
		// Command-level failures only print one failure JSON to stdout and
		// never write a result file, so no output options are set.
		return commandResult{env: result.NewFailure(cmd, result.CodeInvalidArgument, err.Error())}
	}
	if ctxCanceled(ctx) {
		return commandResult{env: result.NewFailure(cmd, result.CodeCanceled, "canceled before execution")}
	}

	raw, err := newSearchClient(cfg.ExaAPIKey).Search(ctx, p)
	if err != nil {
		if ctxCanceled(ctx) {
			return commandResult{env: result.NewFailure(cmd, result.CodeCanceled, "canceled during execution")}
		}
		var perr *search.ProviderError
		if errors.As(err, &perr) {
			return commandResult{env: result.NewFailure(cmd, perr.Code, perr.Msg)}
		}
		return commandResult{env: result.NewFailure(cmd, result.CodeInternalError, "search failed unexpectedly")}
	}

	results := search.NormalizeResults(raw, p)
	env := result.NewSuccess(cmd, search.Data{Results: results})
	count := len(results)
	env.Metadata.ResultCount = &count
	return commandResult{
		env: env,
		outputOpts: output.Options{
			ExplicitPath: p.OutputPath,
			AutoDir:      cfg.OutputDir,
			Summarize:    search.SummarizeEnvelope,
		},
	}
}

// runScrape parses args, validates inputs, and returns per-URL item results.
func runScrape(ctx context.Context, args []string) commandResult {
	cmd := result.CommandPtr(result.CommandScrape)

	cfg, err := config.LoadScrape(os.Getenv, Version)
	if err != nil {
		return commandResult{env: result.NewFailure(cmd, result.CodeInvalidArgument, err.Error())}
	}
	p, err := scrape.ParseArgs(args, cfg.ScrapeWorkers)
	if err != nil {
		// Command-level failures only print one failure JSON to stdout and
		// never write a result file, so no output options are set.
		return commandResult{env: result.NewFailure(cmd, result.CodeInvalidArgument, err.Error())}
	}
	p.UserAgent = cfg.UserAgent
	if ctxCanceled(ctx) {
		return commandResult{env: result.NewFailure(cmd, result.CodeCanceled, "canceled before execution")}
	}

	data, canceled := scrape.BuildInputResultsWithCancel(ctx, p)
	if canceled {
		return commandResult{env: result.NewFailure(cmd, result.CodeCanceled, "canceled during execution")}
	}
	env := result.NewSuccess(cmd, data)
	count := len(data.Results)
	env.Metadata.ResultCount = &count
	return commandResult{
		env: env,
		outputOpts: output.Options{
			ExplicitPath: p.OutputPath,
			AutoDir:      cfg.OutputDir,
			Summarize:    scrape.SummarizeEnvelope,
		},
	}
}

func ctxCanceled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// emit writes the envelope and handles explicit or automatic file output.
func emit(stdout io.Writer, env *result.Envelope, opts output.Options) error {
	return output.Write(stdout, env, opts)
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `opensearch-cli searches and scrapes public HTTP(S) web pages.

Usage:
  opensearch-cli --help
  opensearch-cli --version
  opensearch-cli search [options] <query>
  opensearch-cli scrape [options] <url> [url...]

Commands:
  search    Search public web sources using Exa MCP
  scrape    Fetch and convert public HTTP(S) pages

Global options:
  -h, --help       Show this help text
  -v, --version    Show CLI version

Output:
  Every invocation (except --help/--version) prints a single JSON object to
  stdout. Large results are written to a file and summarized on stdout.
`)
}
