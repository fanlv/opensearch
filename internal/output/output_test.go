package output

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fanlv/opensearch/internal/result"
)

func tmpFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout-*.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func readBack(t *testing.T, f *os.File) *result.Envelope {
	t.Helper()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	var env result.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, b)
	}
	return &env
}

func smallEnv() *result.Envelope {
	return result.NewSuccess(result.CommandPtr(result.CommandScrape), map[string]string{"ok": "yes"})
}

func TestWriteSmallToStdout(t *testing.T) {
	f := tmpFile(t)
	if err := Write(f, smallEnv(), Options{}); err != nil {
		t.Fatal(err)
	}
	env := readBack(t, f)
	if !env.Success {
		t.Error("expected success")
	}
	if env.Metadata.ContentOmitted {
		t.Error("small result must not be marked omitted")
	}
	if env.Metadata.OutputPath != "" {
		t.Error("small result must not have outputPath")
	}
}

func TestWriteExplicitAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")
	if err := os.WriteFile(target, []byte("OLD CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := tmpFile(t)
	env := smallEnv()
	if err := Write(f, env, Options{ExplicitPath: target}); err != nil {
		t.Fatal(err)
	}
	// 完整结果应原子替换原文件。
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "OLD CONTENT") {
		t.Error("explicit target was not replaced")
	}
	var full result.Envelope
	if err := json.Unmarshal(b, &full); err != nil {
		t.Fatalf("result file not valid JSON: %v", err)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		t.Fatal(err)
	}
	if full.Metadata.OutputPath != absTarget {
		t.Fatalf("result file outputPath = %q, want %q", full.Metadata.OutputPath, absTarget)
	}
	if full.Metadata.ContentOmitted {
		t.Fatal("result file must contain complete content and must not mark contentOmitted")
	}
	// stdout 摘要应标记省略并带绝对路径。
	stdoutEnv := readBack(t, f)
	if !stdoutEnv.Metadata.ContentOmitted {
		t.Error("stdout summary should be marked omitted when written to file")
	}
	if !filepath.IsAbs(stdoutEnv.Metadata.OutputPath) {
		t.Errorf("outputPath must be absolute, got %q", stdoutEnv.Metadata.OutputPath)
	}
}

func TestWriteExplicitRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	f := tmpFile(t)
	env := smallEnv()
	if err := Write(f, env, Options{ExplicitPath: dir}); err != nil {
		t.Fatal(err)
	}
	stdoutEnv := readBack(t, f)
	if stdoutEnv.Success {
		t.Fatal("writing to a directory must fail")
	}
	if stdoutEnv.Error.Code != result.CodeOutputWriteErr {
		t.Errorf("error code = %q, want OUTPUT_WRITE_ERROR", stdoutEnv.Error.Code)
	}
}

func TestWriteExplicitRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	realFile := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(realFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(realFile, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	f := tmpFile(t)
	if err := Write(f, smallEnv(), Options{ExplicitPath: link}); err != nil {
		t.Fatal(err)
	}
	stdoutEnv := readBack(t, f)
	if stdoutEnv.Success || stdoutEnv.Error.Code != result.CodeOutputWriteErr {
		t.Errorf("symlink target must yield OUTPUT_WRITE_ERROR, got %+v", stdoutEnv.Error)
	}
}

func TestWriteAutoDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	// 预置 00000 占位，验证自动落盘避让到下一个名字。
	existing := filepath.Join(dir, "opensearch-00000.json")
	if err := os.WriteFile(existing, []byte("KEEP"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 用极小阈值强制走落盘路径。
	f := tmpFile(t)
	env := smallEnv()
	if err := Write(f, env, Options{AutoDir: dir, ThresholdBytes: 1}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "KEEP" {
		t.Error("auto write overwrote an existing file")
	}
	stdoutEnv := readBack(t, f)
	if !stdoutEnv.Metadata.ContentOmitted || stdoutEnv.Metadata.OutputPath == "" {
		t.Error("auto write should mark omitted with outputPath")
	}
	if filepath.Base(stdoutEnv.Metadata.OutputPath) == "opensearch-00000.json" {
		t.Error("auto write should have picked a non-conflicting name")
	}
}

func TestWriteAutoConcurrentWritersDoNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	const writers = 8
	var wg sync.WaitGroup
	paths := make(chan string, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var stdout bytes.Buffer
			env := result.NewSuccess(result.CommandPtr(result.CommandSearch), map[string]string{"writer": strings.Repeat(string(rune('a'+i)), 1024)})
			if err := Write(&stdout, env, Options{AutoDir: dir, ThresholdBytes: 1}); err != nil {
				t.Errorf("Write(%d) returned error: %v", i, err)
				return
			}
			var stdoutEnv result.Envelope
			if err := json.Unmarshal(stdout.Bytes(), &stdoutEnv); err != nil {
				t.Errorf("writer %d stdout is not JSON: %v\n%s", i, err, stdout.String())
				return
			}
			if !stdoutEnv.Success || stdoutEnv.Metadata.OutputPath == "" {
				t.Errorf("writer %d stdout env = %+v", i, &stdoutEnv)
				return
			}
			paths <- stdoutEnv.Metadata.OutputPath
		}(i)
	}
	wg.Wait()
	close(paths)

	seen := map[string]struct{}{}
	for p := range paths {
		if _, ok := seen[p]; ok {
			t.Fatalf("duplicate auto output path allocated: %s", p)
		}
		seen[p] = struct{}{}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("allocated output path missing: %s: %v", p, err)
		}
	}
	if len(seen) != writers {
		t.Fatalf("allocated files = %d, want %d", len(seen), writers)
	}
}

func TestWriteThresholdTriggersFile(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 2000)
	env := result.NewSuccess(result.CommandPtr(result.CommandSearch), map[string]string{"blob": big})
	f := tmpFile(t)
	if err := Write(f, env, Options{AutoDir: dir, ThresholdBytes: 100}); err != nil {
		t.Fatal(err)
	}
	stdoutEnv := readBack(t, f)
	if !stdoutEnv.Metadata.ContentOmitted {
		t.Error("over-threshold result should be written to file and marked omitted")
	}
	// 结果文件存在且为完整 JSON。
	if _, err := os.Stat(stdoutEnv.Metadata.OutputPath); err != nil {
		t.Errorf("result file missing: %v", err)
	}
	fileBytes, err := os.ReadFile(stdoutEnv.Metadata.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	var fileEnv result.Envelope
	if err := json.Unmarshal(fileBytes, &fileEnv); err != nil {
		t.Fatalf("result file is not valid JSON: %v", err)
	}
	if fileEnv.Metadata.OutputPath != stdoutEnv.Metadata.OutputPath {
		t.Fatalf("result file outputPath = %q, want its own path %q", fileEnv.Metadata.OutputPath, stdoutEnv.Metadata.OutputPath)
	}
	if fileEnv.Metadata.ContentOmitted {
		t.Fatal("result file must not mark contentOmitted")
	}
}

func TestWriteCompactsOversizedSummary(t *testing.T) {
	dir := t.TempDir()
	bigTitle := strings.Repeat("t", 2000)
	bigContent := strings.Repeat("c", 2000)
	longURL := "https://example.com/articles/contract-output-summary"
	env := result.NewSuccess(result.CommandPtr(result.CommandScrape), map[string]interface{}{
		"results": []interface{}{
			map[string]interface{}{
				"success":  true,
				"url":      longURL,
				"finalUrl": longURL,
				"title":    bigTitle,
				"format":   "markdown",
				"content":  bigContent,
				"metadata": map[string]interface{}{"raw": strings.Repeat("m", 2000)},
				"error":    nil,
			},
			map[string]interface{}{
				"success":  false,
				"url":      "https://example.com/fail",
				"finalUrl": "https://example.com/fail",
				"title":    bigTitle,
				"format":   "markdown",
				"content":  "",
				"metadata": map[string]interface{}{"raw": strings.Repeat("e", 2000)},
				"error": map[string]interface{}{
					"code":    result.CodeHTTPStatusError,
					"message": strings.Repeat("boom", 500),
				},
			},
		},
	})

	f := tmpFile(t)
	if err := Write(f, env, Options{
		AutoDir:        dir,
		ThresholdBytes: 1000,
		Summarize: func(full *result.Envelope) *result.Envelope {
			summary := *full
			data := full.Data.(map[string]interface{})
			results := data["results"].([]interface{})
			trimmed := make([]interface{}, len(results))
			for i, raw := range results {
				item := raw.(map[string]interface{})
				copyItem := make(map[string]interface{}, len(item))
				for k, v := range item {
					copyItem[k] = v
				}
				copyItem["content"] = ""
				trimmed[i] = copyItem
			}
			summary.Data = map[string]interface{}{"results": trimmed}
			return &summary
		},
	}); err != nil {
		t.Fatal(err)
	}

	stdoutBytes, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(stdoutBytes) > 1000 {
		t.Fatalf("stdout summary size = %d, want <= 1000\n%s", len(stdoutBytes), stdoutBytes)
	}
	stdoutEnv := readBack(t, f)
	if !stdoutEnv.Metadata.ContentOmitted || stdoutEnv.Metadata.OutputPath == "" {
		t.Fatal("compacted stdout must still point to the full result file")
	}
	data := stdoutEnv.Data.(map[string]interface{})
	results := data["results"].([]interface{})
	if len(results) != 2 {
		t.Fatalf("compacted summary lost results: got %d", len(results))
	}
	first := results[0].(map[string]interface{})
	if first["url"] != longURL || first["finalUrl"] != longURL {
		t.Fatalf("compacted summary must preserve URLs, got %+v", first)
	}
	for _, k := range []string{"title", "format", "content", "metadata"} {
		if _, ok := first[k]; ok {
			t.Fatalf("compacted summary should omit %s from result: %+v", k, first)
		}
	}
	second := results[1].(map[string]interface{})
	errObj := second["error"].(map[string]interface{})
	if errObj["code"] != result.CodeHTTPStatusError {
		t.Fatalf("compacted summary must preserve stable error code, got %+v", errObj)
	}
	if _, ok := errObj["message"]; ok {
		t.Fatalf("compacted summary should omit large error message, got %+v", errObj)
	}

	fileBytes, err := os.ReadFile(stdoutEnv.Metadata.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fileBytes), bigContent) || !strings.Contains(string(fileBytes), bigTitle) {
		t.Fatal("full result file should keep complete uncompact data")
	}
}

// TestWriteStageTwoTrimsMetadataBeforeCompacting 验证三阶段裁剪的第 2 阶段（方案 §5.5）：
// 当省略 content 后摘要仍超阈值、但超限来自超大的 per-item metadata 时，应先省略
// 非必要可变长元数据（metadata），而不是越级进入 stage 3 丢掉 title / format。
func TestWriteStageTwoTrimsMetadataBeforeCompacting(t *testing.T) {
	dir := t.TempDir()
	smallTitle := "Concise Title"
	url := "https://example.com/page"
	env := result.NewSuccess(result.CommandPtr(result.CommandScrape), map[string]interface{}{
		"results": []interface{}{
			map[string]interface{}{
				"success":  true,
				"url":      url,
				"finalUrl": url,
				"title":    smallTitle,
				"format":   "markdown",
				"content":  strings.Repeat("c", 2000),
				// 超大的可变长元数据：stage 1 省 content 后仍会超阈值。
				"metadata": map[string]interface{}{"raw": strings.Repeat("m", 1500)},
				"error":    nil,
			},
		},
	})

	f := tmpFile(t)
	if err := Write(f, env, Options{
		AutoDir:        dir,
		ThresholdBytes: 800,
		Summarize:      scrapeStageOne,
	}); err != nil {
		t.Fatal(err)
	}

	stdoutBytes, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(stdoutBytes) > 800 {
		t.Fatalf("stage-2 summary size = %d, want <= 800\n%s", len(stdoutBytes), stdoutBytes)
	}
	stdoutEnv := readBack(t, f)
	item := stdoutEnv.Data.(map[string]interface{})["results"].([]interface{})[0].(map[string]interface{})

	// stage 2 应省掉非必要可变长字段（metadata、title）...
	if _, ok := item["metadata"]; ok {
		t.Fatalf("stage 2 should omit metadata, got %+v", item)
	}
	if _, ok := item["title"]; ok {
		t.Fatalf("stage 2 should omit nonessential title, got %+v", item)
	}
	// ...但仍保留 format / content 字段（证明没有越级进入 stage 3，stage 3 会连这些一起丢）。
	if _, ok := item["format"]; !ok {
		t.Fatalf("stage 2 must keep format (stage 3 would drop it), got %+v", item)
	}
	if _, ok := item["content"]; !ok {
		t.Fatalf("stage 2 must keep content field (stage 3 would drop it), got %+v", item)
	}
	if item["url"] != url || item["finalUrl"] != url {
		t.Fatalf("stage 2 must keep URLs, got %+v", item)
	}
}

// scrapeStageOne 模拟 command-level summarizer 的第 1 阶段：仅省略大 content。
func scrapeStageOne(full *result.Envelope) *result.Envelope {
	summary := *full
	data := full.Data.(map[string]interface{})
	results := data["results"].([]interface{})
	trimmed := make([]interface{}, len(results))
	for i, raw := range results {
		item := raw.(map[string]interface{})
		copyItem := make(map[string]interface{}, len(item))
		for k, v := range item {
			copyItem[k] = v
		}
		copyItem["content"] = ""
		trimmed[i] = copyItem
	}
	summary.Data = map[string]interface{}{"results": trimmed}
	return &summary
}
