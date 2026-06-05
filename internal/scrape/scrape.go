// Package scrape implements the CLI-facing scrape parameter and per-URL result
// pipeline. This package owns URL input validation, public-target SSRF guards,
// anonymous HTTP fetching, bounded batch scheduling, content extraction,
// sanitization, and format conversion.
package scrape

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fanlv/opensearch/internal/config"
	"github.com/fanlv/opensearch/internal/result"
	"github.com/fanlv/opensearch/internal/urlnorm"
)

const (
	FormatMarkdown = "markdown"
	FormatText     = "text"
	FormatHTML     = "html"

	DefaultPerURLTimeoutSeconds = 30
	MinPerURLTimeoutSeconds     = 1
	MaxPerURLTimeoutSeconds     = 120

	DefaultTotalTimeoutSeconds = 180
	MinTotalTimeoutSeconds     = 1
	MaxTotalTimeoutSeconds     = 600

	MinURLs = 1
	MaxURLs = 20

	invalidURLPreviewBytes = 512
)

// Params is the validated scrape command configuration.
type Params struct {
	URLs              []string
	Format            string
	MainContent       bool
	PerURLTimeoutSecs int
	TotalTimeoutSecs  int
	Concurrency       int
	OutputPath        string
	UserAgent         string
}

// Data is scrape's successful command-level payload. Individual URL results can
// still fail, while the top-level envelope remains success=true.
type Data struct {
	Results []Result `json:"results"`
}

// Result is one per-URL scrape result in deduplicated input order.
type Result struct {
	Success        bool                   `json:"success"`
	URL            string                 `json:"url"`
	FinalURL       string                 `json:"finalUrl"`
	Title          string                 `json:"title"`
	TitleTruncated bool                   `json:"titleTruncated,omitempty"`
	Format         string                 `json:"format"`
	Content        string                 `json:"content"`
	Metadata       map[string]interface{} `json:"metadata"`
	Error          *result.Error          `json:"error"`
}

// BuildInputResults applies the shared URL normalization rules and runs public
// URLs through the bounded batch scrape pipeline. Invalid / blocked URLs are
// represented as per-item failures; valid duplicate URLs are collapsed by the
// fragment-less dedup key and keep the first position. Successful and failed
// items are returned in deduplicated input order, independent of completion
// order.
func BuildInputResults(ctx context.Context, p *Params) Data {
	data, _ := BuildInputResultsWithCancel(ctx, p)
	return data
}

// BuildInputResultsWithCancel is the CLI-facing variant of BuildInputResults.
// The second return value reports whether the parent command context was
// externally canceled. Batch total timeout remains a per-item TASK_TIMEOUT and
// does not set this flag.
func BuildInputResultsWithCancel(ctx context.Context, p *Params) (Data, bool) {
	batchCtx, cancel := context.WithTimeout(ctx, time.Duration(effectiveTotalTimeoutSecs(p))*time.Second)
	defer cancel()

	seen := make(map[string]struct{}, len(p.URLs))
	results := make([]Result, 0, len(p.URLs))
	tasks := make([]scrapeTask, 0, len(p.URLs))

	for _, raw := range p.URLs {
		norm, err := urlnorm.Normalize(raw)
		if err != nil {
			results = append(results, invalidURLResult(raw, p.Format))
			continue
		}
		if _, ok := seen[norm.ForDedup]; ok {
			continue
		}
		seen[norm.ForDedup] = struct{}{}

		idx := len(results)
		results = append(results, taskTimeoutResult(norm.ForDedup, p.Format))
		tasks = append(tasks, scrapeTask{index: idx, norm: norm})
	}
	if len(tasks) == 0 {
		return Data{Results: results}, ctx.Err() != nil
	}

	jobs := make(chan scrapeTask)
	workers := minInt(effectiveConcurrency(p), len(tasks))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range jobs {
				results[task.index] = runScrapeTask(batchCtx, p, task)
			}
		}()
	}

	for _, task := range tasks {
		select {
		case <-batchCtx.Done():
			close(jobs)
			wg.Wait()
			return Data{Results: results}, ctx.Err() != nil
		case jobs <- task:
		}
	}
	close(jobs)
	wg.Wait()

	return Data{Results: results}, ctx.Err() != nil
}

type scrapeTask struct {
	index int
	norm  *urlnorm.Normalized
}

func runScrapeTask(batchCtx context.Context, p *Params, task scrapeTask) Result {
	start := time.Now()
	itemDeadline := start.Add(time.Duration(effectivePerURLTimeoutSecs(p)) * time.Second)
	batchDeadline, hasBatchDeadline := batchCtx.Deadline()
	itemCtx, itemCancel := context.WithTimeout(batchCtx, time.Duration(effectivePerURLTimeoutSecs(p))*time.Second)
	defer itemCancel()

	item, fetchErr := fetchURL(itemCtx, task.norm, p)
	if fetchErr == nil {
		return item
	}
	code := itemErrorCode(fetchErr)
	if code == result.CodeScrapeTimeout && batchCtx.Err() != nil && batchTimeoutWon(hasBatchDeadline, batchDeadline, itemDeadline) {
		code = result.CodeTaskTimeout
	}
	finalURL := task.norm.ForDedup
	if current := errorFinalURL(fetchErr); current != "" {
		finalURL = current
	}
	return failedResult(task.norm.ForDedup, finalURL, p.Format, code, fetchErr.Error())
}

func batchTimeoutWon(hasBatchDeadline bool, batchDeadline, itemDeadline time.Time) bool {
	return hasBatchDeadline && !itemDeadline.Before(batchDeadline)
}

func taskTimeoutResult(url, format string) Result {
	return failedResult(url, url, format, result.CodeTaskTimeout, "batch total timeout reached")
}

func effectivePerURLTimeoutSecs(p *Params) int {
	if p != nil && p.PerURLTimeoutSecs > 0 {
		return p.PerURLTimeoutSecs
	}
	return DefaultPerURLTimeoutSeconds
}

func effectiveTotalTimeoutSecs(p *Params) int {
	if p != nil && p.TotalTimeoutSecs > 0 {
		return p.TotalTimeoutSecs
	}
	return DefaultTotalTimeoutSeconds
}

func effectiveConcurrency(p *Params) int {
	if p != nil && p.Concurrency > 0 {
		return p.Concurrency
	}
	return config.DefaultScrapeWorkers
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func failedResult(url, finalURL, format, code, msg string) Result {
	return Result{
		Success:  false,
		URL:      url,
		FinalURL: finalURL,
		Title:    "",
		Format:   format,
		Content:  "",
		Metadata: map[string]interface{}{},
		Error:    &result.Error{Code: code, Message: msg},
	}
}

func invalidURLResult(raw, format string) Result {
	preview, truncated := boundedInvalidURLPreview(raw)
	item := failedResult(preview, "", format, result.CodeInvalidURL, "invalid URL")
	if truncated {
		item.Metadata["urlTruncated"] = true
	}
	return item
}

func boundedInvalidURLPreview(raw string) (string, bool) {
	if len(raw) <= invalidURLPreviewBytes {
		return raw, false
	}
	limit := invalidURLPreviewBytes
	for limit > 0 && !utf8.RuneStart(raw[limit]) {
		limit--
	}
	if limit <= 0 {
		return "…", true
	}
	return raw[:limit] + "…", true
}

func validateFormat(v string) error {
	switch v {
	case FormatMarkdown, FormatText, FormatHTML:
		return nil
	default:
		return invalidArg("--format must be one of markdown, text, or html")
	}
}

func validateConcurrency(n int) error {
	if n < config.MinScrapeWorkers || n > config.MaxScrapeWorkers {
		return invalidArg("--concurrency must be within %d-%d", config.MinScrapeWorkers, config.MaxScrapeWorkers)
	}
	return nil
}

func invalidArg(format string, args ...interface{}) error {
	return fmt.Errorf("%s", fmt.Sprintf(format, args...))
}

func takeValue(args []string, i int, flag string) (string, int, error) {
	if i+1 >= len(args) {
		return "", 0, invalidArg("%s requires a value", flag)
	}
	return args[i+1], i + 2, nil
}

func trimOptionValue(raw, prefix string) string {
	return strings.TrimSpace(strings.TrimPrefix(raw, prefix))
}
