// Package output 负责把统一 JSON 信封写到 stdout 或结果文件（方案 §5.5）。
//
// 行为契约：
//   - 小结果（序列化 <= 阈值）直接在 stdout 返回完整 JSON。
//   - 指定输出文件、或完整 JSON 超过阈值时，完整 JSON 写入文件，stdout 返回摘要 JSON，
//     并设置 metadata.contentOmitted=true 与绝对路径 metadata.outputPath。
//   - 显式输出路径已存在且为普通文件时原子替换；目标为目录 / 符号链接 / 无法安全替换
//     返回 OUTPUT_WRITE_ERROR。
//   - 自动落盘使用不与已有文件冲突的文件名，不覆盖已有文件。
//   - 写入失败不留下半写文件（先写临时文件并 fsync，再 rename）。
package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/fanlv/opensearch/internal/result"
)

// DefaultThresholdBytes 是 stdout 直接返回完整 JSON 的大小阈值（方案 §5.5：默认 256KB）。
const DefaultThresholdBytes = 256 * 1024

// ErrOutputWrite 表示结果文件无法安全写入（映射为 OUTPUT_WRITE_ERROR）。
var ErrOutputWrite = errors.New("output write error")

var errOutputExists = errors.New("output target exists")

func writeErr(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrOutputWrite, fmt.Sprintf(format, args...))
}

// Options 控制一次写出行为。
type Options struct {
	// ExplicitPath 是用户通过 -o 显式指定的输出路径；为空表示未指定。
	ExplicitPath string
	// AutoDir 是自动落盘目录（来自配置 OPENSEARCH_OUTPUT_DIR 或默认 .opensearch）。
	AutoDir string
	// ThresholdBytes 是 stdout 完整返回的字节阈值；<=0 时用 DefaultThresholdBytes。
	ThresholdBytes int
	// Summarize 在需要落盘时把完整信封裁剪为 stdout 摘要信封。
	// 为 nil 时退化为“原样输出但标记 contentOmitted”。本步骤先提供框架，
	// 逐字段省略（先省 snippet/content）由 search/scrape 在步骤 5/6/7 填充。
	Summarize func(full *result.Envelope) *result.Envelope
}

// Write 把信封按契约写到 stdout（w）并在需要时落盘。返回最终写到 stdout 的字节。
// 写文件失败时，会把传入信封改写为 OUTPUT_WRITE_ERROR 失败信封并仍输出到 stdout，
// 由调用方据其 ExitCode 决定退出码。
func Write(w io.Writer, env *result.Envelope, opts Options) error {
	threshold := opts.ThresholdBytes
	if threshold <= 0 {
		threshold = DefaultThresholdBytes
	}

	full, err := Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	// 命令级失败信封只向 stdout 输出单个 JSON：失败没有需要持久化的“完整结果”，
	// 不写文件、也不设 outputPath / contentOmitted（方案 §5.5：data 仅在成功时存在，
	// contentOmitted 表示“完整结果以文件为准”，对无 content 的失败无意义）。
	needFile := env.Success && (opts.ExplicitPath != "" || len(full) > threshold)
	if !needFile {
		return writeStdout(w, full)
	}

	path, ferr := writeResultFile(env, opts)
	if ferr != nil {
		return writeOutputFailure(w, env, ferr)
	}

	// 落盘成功：stdout 返回摘要信封并标记省略。
	summary := env
	if opts.Summarize != nil {
		summary = opts.Summarize(env)
	}
	summary.Metadata.OutputPath = path
	summary.Metadata.ContentOmitted = true
	// 同步回写给调用方，使其 metadata 反映落盘结果。
	env.Metadata.OutputPath = path
	env.Metadata.ContentOmitted = true

	sb, merr := Marshal(summary)
	if merr != nil {
		return merr
	}
	// stdout 摘要分阶段裁剪（方案 §5.5 / output-schema 三阶段优先级）：
	//   阶段 1：command-level summarizer 已省略大字段（snippet / content）。
	//   阶段 2：仍超阈值时，省略非必要的可变长元数据（如 per-item metadata、title）。
	//   阶段 3：极端情况只保留结果身份、状态、URL / finalUrl 与稳定错误码。
	if len(sb) > threshold {
		staged := trimNonessentialMetadata(summary)
		if sb2, err := Marshal(staged); err == nil {
			sb = sb2
			summary = staged
		}
	}
	if len(sb) > threshold {
		compact := compactSummary(summary)
		sb, merr = Marshal(compact)
		if merr != nil {
			return merr
		}
	}
	return writeStdout(w, sb)
}

func writeOutputFailure(w io.Writer, env *result.Envelope, ferr error) error {
	fail := result.NewFailure(env.Metadata.Command, result.CodeOutputWriteErr, ferr.Error())
	fail.Metadata.DurationMs = env.Metadata.DurationMs
	b, merr := Marshal(fail)
	if merr != nil {
		return merr
	}
	*env = *fail
	return writeStdout(w, b)
}

func writeResultFile(env *result.Envelope, opts Options) (string, error) {
	if opts.ExplicitPath != "" {
		path, err := prepareExplicit(opts.ExplicitPath)
		if err != nil {
			return "", err
		}
		data, err := marshalFileEnvelope(env, path)
		if err != nil {
			return "", err
		}
		if err := atomicWriteReplace(path, data); err != nil {
			return "", err
		}
		return path, nil
	}
	return writeAutoResultFile(env, opts.AutoDir)
}

func marshalFileEnvelope(env *result.Envelope, path string) ([]byte, error) {
	// 结果文件必须包含完整 JSON，并记录自身绝对路径。contentOmitted 只表示
	// stdout 摘要发生了省略，完整文件本身不应标记为省略。
	fileEnv := *env
	fileEnv.Metadata.OutputPath = path
	fileEnv.Metadata.ContentOmitted = false
	data, err := Marshal(&fileEnv)
	if err != nil {
		return nil, fmt.Errorf("marshal file envelope: %w", err)
	}
	return data, nil
}

func writeAutoResultFile(env *result.Envelope, dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", writeErr("create output dir: %v", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", writeErr("resolve output dir: %v", err)
	}
	for i := 0; i < 100000; i++ {
		path := filepath.Join(absDir, fmt.Sprintf("opensearch-%05d.json", i))
		data, err := marshalFileEnvelope(env, path)
		if err != nil {
			return "", err
		}
		err = atomicWriteNoReplace(path, data)
		if errors.Is(err, errOutputExists) {
			continue
		}
		if err != nil {
			return "", err
		}
		return path, nil
	}
	return "", writeErr("could not allocate a unique output filename in %s", absDir)
}

// compactSummary 是 stdout 摘要的第二阶段裁剪：当命令级 summarizer 已省略
// snippet/content 后仍超过阈值时，只保留每个结果的身份、状态和稳定错误码。
// 这样可以尽量不超 stdout 阈值，同时不丢结果项、不截断 URL。
func compactSummary(env *result.Envelope) *result.Envelope {
	// Only success envelopes reach this stage (see needFile in Write), so there
	// is no command-level failure branch to handle here.
	compact := *env
	data, ok := envelopeDataAsMap(env.Data)
	if !ok {
		return &compact
	}
	results, ok := data["results"].([]interface{})
	if !ok {
		return &compact
	}

	minResults := make([]interface{}, len(results))
	for i, raw := range results {
		item, ok := raw.(map[string]interface{})
		if !ok {
			minResults[i] = raw
			continue
		}
		minItem := make(map[string]interface{}, 4)
		if v, ok := item["success"]; ok {
			minItem["success"] = v
		}
		if v, ok := item["url"]; ok {
			minItem["url"] = v
		}
		if v, ok := item["finalUrl"]; ok {
			minItem["finalUrl"] = v
		}
		if errObj, ok := item["error"].(map[string]interface{}); ok {
			if code, ok := errObj["code"]; ok {
				minItem["error"] = map[string]interface{}{"code": code}
			}
		}
		minResults[i] = minItem
	}
	compact.Data = map[string]interface{}{"results": minResults}
	return &compact
}

// trimNonessentialMetadata 是 stdout 摘要的第二阶段裁剪（方案 §5.5）：当省略
// snippet / content 后仍超阈值时，去掉每个结果项中非必要的可变长元数据
// （per-item metadata 对象，以及 title / publishedDate 这类可变长非关键字段），
// 但保留结果身份、状态、URL / finalUrl、format 与稳定错误码，且不丢结果项、不截断 URL。
// 该阶段优先于极端的 compactSummary，使裁剪粒度与文档定义的三阶段一致。
func trimNonessentialMetadata(env *result.Envelope) *result.Envelope {
	trimmed := *env
	if !env.Success {
		return &trimmed
	}
	data, ok := envelopeDataAsMap(env.Data)
	if !ok {
		return &trimmed
	}
	results, ok := data["results"].([]interface{})
	if !ok {
		return &trimmed
	}

	out := make([]interface{}, len(results))
	for i, raw := range results {
		item, ok := raw.(map[string]interface{})
		if !ok {
			out[i] = raw
			continue
		}
		for _, k := range nonessentialItemKeys {
			delete(item, k)
		}
		out[i] = item
	}
	data["results"] = out
	trimmed.Data = data
	return &trimmed
}

// nonessentialItemKeys 是阶段 2 可省略的可变长 / 非关键结果字段。
// 不含 success / url / finalUrl / format / error 等身份与状态字段。
var nonessentialItemKeys = []string{
	"metadata",
	"title", "titleTruncated",
	"publishedDate", "publishedDateTruncated",
}

func envelopeDataAsMap(data interface{}) (map[string]interface{}, bool) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, false
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, false
	}
	return out, true
}

// prepareExplicit 校验用户指定路径：拒绝目录 / 符号链接，已存在普通文件允许后续原子替换。
func prepareExplicit(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", writeErr("resolve path: %v", err)
	}
	// Lstat 不跟随符号链接，用于拒绝目标本身是符号链接的情况。
	if info, err := os.Lstat(abs); err == nil {
		switch {
		case info.IsDir():
			return "", writeErr("target is a directory")
		case info.Mode()&os.ModeSymlink != 0:
			return "", writeErr("target is a symlink")
		case !info.Mode().IsRegular():
			return "", writeErr("target is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", writeErr("stat target: %v", err)
	}
	return abs, nil
}

// atomicWriteReplace 先写同目录临时文件并 fsync，再 rename 到目标。用于显式 -o，允许
// 原子替换已经校验过的普通文件。
func atomicWriteReplace(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".opensearch-*.tmp")
	if err != nil {
		return writeErr("create temp file: %v", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return writeErr("write temp file: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return writeErr("sync temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return writeErr("close temp file: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return writeErr("rename temp file: %v", err)
	}
	return nil
}

// atomicWriteNoReplace 先写同目录临时文件并 fsync，再用 hard link 原子创建最终路径。
// link 语义保证目标已存在时失败而不是覆盖，避免自动落盘的 Lstat/Rename TOCTOU。
func atomicWriteNoReplace(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".opensearch-*.tmp")
	if err != nil {
		return writeErr("create temp file: %v", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return writeErr("write temp file: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return writeErr("sync temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return writeErr("close temp file: %v", err)
	}
	if err := os.Link(tmpName, path); err != nil {
		cleanup()
		if errors.Is(err, os.ErrExist) {
			return errOutputExists
		}
		return writeErr("link temp file: %v", err)
	}
	cleanup()
	return nil
}

// Marshal 以两空格缩进序列化信封，末尾补换行。
func Marshal(env *result.Envelope) ([]byte, error) {
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func writeStdout(w io.Writer, data []byte) error {
	_, err := w.Write(data)
	return err
}
