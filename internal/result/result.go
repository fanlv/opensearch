// Package result 定义 opensearch-cli 的统一输出契约（方案 §5.5）：
// 顶层 JSON 信封、稳定错误对象与错误码、以及命令级退出码。
// 这是所有子命令共享的单一事实源；后续 references/output-schema.md 以此为准。
package result

// 退出码（方案 §5.1 / §5.5）。
//
//	--help / --version 与命令级 success=true 均为 0；
//	参数 / 用法 / 配置类失败为 2；其余命令级失败为 1。
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Command 标识本次执行确定的子命令；未确定（含无法识别）时为 nil，
// 序列化为 metadata.command=null（方案 §5.1）。
type Command string

const (
	CommandSearch Command = "search"
	CommandScrape Command = "scrape"
)

// 命令级稳定错误码（方案 §5.5）。这些错误描述整条命令的成败。
const (
	CodeInvalidArgument = "INVALID_ARGUMENT"
	CodeConfigRequired  = "CONFIG_REQUIRED"
	CodeOutputWriteErr  = "OUTPUT_WRITE_ERROR"
	CodeCanceled        = "CANCELED"
	CodeInternalError   = "INTERNAL_ERROR"

	// Provider（Exa search）相关命令级错误，由 search 子命令触发。
	CodeProviderAuth        = "PROVIDER_AUTH_ERROR"
	CodeProviderRateLimited = "PROVIDER_RATE_LIMITED"
	CodeProviderUnavailable = "PROVIDER_UNAVAILABLE"
	CodeProviderError       = "PROVIDER_ERROR"
)

// 单 URL 抓取稳定错误码（方案 §5.5）。由 scrape 子命令在逐项结果的 error 中使用，
// 不影响命令级 success / 退出码。
const (
	CodeInvalidURL                 = "INVALID_URL"
	CodeSSRFBlocked                = "SSRF_BLOCKED"
	CodeHTTPStatusError            = "HTTP_STATUS_ERROR"
	CodeScrapeTimeout              = "SCRAPE_TIMEOUT"
	CodeTaskTimeout                = "TASK_TIMEOUT"
	CodeNetworkError               = "NETWORK_ERROR"
	CodeTooManyRedirects           = "TOO_MANY_REDIRECTS"
	CodeResponseTooLarge           = "RESPONSE_TOO_LARGE"
	CodeUnsupportedContentType     = "UNSUPPORTED_CONTENT_TYPE"
	CodeUnsupportedContentEncoding = "UNSUPPORTED_CONTENT_ENCODING"
	CodeUnsupportedCharset         = "UNSUPPORTED_CHARSET"
	CodeEmptyContent               = "EMPTY_CONTENT"
	CodeConversionError            = "CONVERSION_ERROR"
)

// usageCodes 是映射到退出码 2 的命令级错误码集合。
var usageCodes = map[string]struct{}{
	CodeInvalidArgument: {},
	CodeConfigRequired:  {},
}

// ExitCodeFor 返回某个命令级错误码对应的进程退出码。
func ExitCodeFor(code string) int {
	if _, ok := usageCodes[code]; ok {
		return ExitUsage
	}
	return ExitError
}

// Error 是稳定的命令级 / 单项错误对象。Message 面向模型，
// 必须不含 API Key，也不得回传 Provider 原始错误响应正文（方案 §5.5 / §6.2）。
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Metadata 是每次执行都返回的元数据。Command 用指针以区分
// “未确定子命令”（null）与已确定子命令（方案 §5.1）。
type Metadata struct {
	Command     *Command `json:"command"`
	DurationMs  int64    `json:"durationMs"`
	ResultCount *int     `json:"resultCount,omitempty"`
	// 文件输出相关：完整结果落盘时填充（方案 §5.5）。
	OutputPath     string `json:"outputPath,omitempty"`
	ContentOmitted bool   `json:"contentOmitted,omitempty"`
}

// Envelope 是写到 stdout / 结果文件的顶层 JSON 对象（方案 §5.5）。
// 四个字段固定存在；成功时 Error 为 nil、失败时 Data 为 nil。
type Envelope struct {
	Success  bool        `json:"success"`
	Data     interface{} `json:"data"`
	Error    *Error      `json:"error"`
	Metadata Metadata    `json:"metadata"`
}

// NewSuccess 构造一个成功信封。
func NewSuccess(cmd *Command, data interface{}) *Envelope {
	return &Envelope{
		Success:  true,
		Data:     data,
		Error:    nil,
		Metadata: Metadata{Command: cmd},
	}
}

// NewFailure 构造一个失败信封。
func NewFailure(cmd *Command, code, message string) *Envelope {
	return &Envelope{
		Success:  false,
		Data:     nil,
		Error:    &Error{Code: code, Message: message},
		Metadata: Metadata{Command: cmd},
	}
}

// ExitCode 返回该信封对应的进程退出码。
func (e *Envelope) ExitCode() int {
	if e.Success {
		return ExitOK
	}
	if e.Error != nil {
		return ExitCodeFor(e.Error.Code)
	}
	return ExitError
}

// CommandPtr 是构造 *Command 的便捷函数。
func CommandPtr(c Command) *Command { return &c }
