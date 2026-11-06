package protocol

import "sort"

// 统一错误码常量（§6.5 / K3）。REST 响应包络 code 与 WS chat.error.code 共用这一套编号，
// 任何其它包不得硬编码数字字面量。
const (
	CodeOK                      = 0    // OK
	CodeUnauthorized            = 1001 // UNAUTHORIZED
	CodeInvalidPassword         = 1002 // INVALID_PASSWORD
	CodeTokenExpired            = 1003 // TOKEN_EXPIRED
	CodeBadRequest              = 1004 // BAD_REQUEST
	CodeNotFound                = 1005 // NOT_FOUND（通用 404：未知 API 路径，保持 K3 统一包络）
	CodeDeviceNotFound          = 2001 // DEVICE_NOT_FOUND
	CodeAgentNotFound           = 3001 // AGENT_NOT_FOUND
	CodeAgentNotRegistered      = 3002 // AGENT_NOT_REGISTERED
	CodeAgentOffline            = 3003 // AGENT_OFFLINE
	CodeSessionNotFound         = 4001 // SESSION_NOT_FOUND
	CodeMessageTooLarge         = 4002 // MESSAGE_TOO_LARGE
	CodeGenerationInProgress    = 4003 // GENERATION_IN_PROGRESS
	CodeInternalError           = 5000 // INTERNAL_ERROR
	CodeAdapterUnauthorized     = 5001 // ADAPTER_UNAUTHORIZED
	CodeProtocolVersionMismatch = 5002 // PROTOCOL_VERSION_MISMATCH
	CodeCLIStartFailed          = 5003 // CLI_START_FAILED
	CodeResumeRejected          = 5004 // RESUME_REJECTED
	CodeGenerationTimeout       = 5005 // GENERATION_TIMEOUT
)

// codeInfo 汇总一个错误码在用户侧与传输层的呈现方式。
type codeInfo struct {
	message    string // 简体中文，可直接展示（R-18 / K10）
	httpStatus int    // 失败时使用的 HTTP 状态码
}

// codeRegistry 是错误码 → 中文文案 / HTTP 状态的唯一映射表。
var codeRegistry = map[int]codeInfo{
	CodeOK:                      {message: "ok", httpStatus: 200},
	CodeUnauthorized:            {message: "请先登录", httpStatus: 401},
	CodeInvalidPassword:         {message: "密码错误", httpStatus: 401},
	CodeTokenExpired:            {message: "登录已过期，请重新登录", httpStatus: 401},
	CodeBadRequest:              {message: "请求参数有误", httpStatus: 400},
	CodeNotFound:                {message: "请求的资源不存在", httpStatus: 404},
	CodeDeviceNotFound:          {message: "设备不存在", httpStatus: 404},
	CodeAgentNotFound:           {message: "Agent 不存在", httpStatus: 404},
	CodeAgentNotRegistered:      {message: "该 Agent 尚未注册", httpStatus: 409},
	CodeAgentOffline:            {message: "Agent 当前离线，暂时无法发送消息", httpStatus: 409},
	CodeSessionNotFound:         {message: "会话不存在", httpStatus: 404},
	CodeMessageTooLarge:         {message: "消息内容过长", httpStatus: 413},
	CodeGenerationInProgress:    {message: "上一条回复仍在生成中", httpStatus: 409},
	CodeInternalError:           {message: "服务内部错误，请稍后重试", httpStatus: 500},
	CodeAdapterUnauthorized:     {message: "设备令牌无效", httpStatus: 401},
	CodeProtocolVersionMismatch: {message: "适配器版本不兼容，请升级", httpStatus: 426},
	CodeCLIStartFailed:          {message: "Agent 启动失败，请检查本机 CLI", httpStatus: 502},
	CodeResumeRejected:          {message: "会话恢复失败，已开启新会话", httpStatus: 409},
	CodeGenerationTimeout:       {message: "回复超时，已停止", httpStatus: 504},
}

// ErrorMessage 返回错误码对应的简体中文文案；未知码回退为内部错误文案。
func ErrorMessage(code int) string {
	if info, ok := codeRegistry[code]; ok {
		return info.message
	}
	return codeRegistry[CodeInternalError].message
}

// HTTPStatus 返回错误码对应的 HTTP 状态码；未知码回退 500。
func HTTPStatus(code int) int {
	if info, ok := codeRegistry[code]; ok {
		return info.httpStatus
	}
	return codeRegistry[CodeInternalError].httpStatus
}

// IsKnownCode 判断错误码是否已登记。
func IsKnownCode(code int) bool {
	_, ok := codeRegistry[code]
	return ok
}

// AllCodes 返回全部已登记错误码（升序，含 0），供完整性单测使用。
func AllCodes() []int {
	out := make([]int, 0, len(codeRegistry))
	for code := range codeRegistry {
		out = append(out, code)
	}
	sort.Ints(out)
	return out
}
