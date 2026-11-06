package adapter

// AS 族解析器（§8.8）：codex app-server 的 JSON-RPC over stdio。
// initialize → thread/start|thread/resume（失败回退 thread/start）→ turn/start；
// 通知 turn/started / item/* / turn/completed → MsgText/Result；
// HandshakeTimeout=60s 覆盖 WSL 冷启动 ~39s；thread/resume 单行巨大依赖 32MiB scanner。

import (
	"context"
	"strings"
	"time"
)

// AppServerParser 解析 codex app-server。
type AppServerParser struct{}

// NewAppServerParser 构造 AS 族解析器。
func NewAppServerParser() *AppServerParser {
	return &AppServerParser{}
}

// Parse 启动 app-server turn。
func (p *AppServerParser) Parse(
	ctx context.Context,
	process *Process,
	descriptor Descriptor,
	prompt string,
	plan ResumePlan,
	opts ExecOptions,
) (*Session, error) {
	return parseRPCTurn(ctx, process, descriptor, prompt, plan, opts, appServerTurnConfig())
}

func appServerTurnConfig() rpcTurnConfig {
	return rpcTurnConfig{
		newMethod:         rpcThreadStart,
		promptMethod:      rpcASTurnStart,
		initializedMethod: "", // app-server 不要求 initialized 通知
		handshakeTimeout:  60 * time.Second,
		promptParams: func(threadID string, prompt string) map[string]any {
			return map[string]any{
				"threadId": threadID,
				"input":    []any{map[string]any{"type": "text", "text": prompt}},
			}
		},
		extractText: func(frame rpcFrame) string {
			method := stringFromMap(frame, "method")
			if method != "turn/started" && !strings.HasPrefix(method, "item/") {
				return ""
			}
			params := nestedMap(frame, "params")
			if text := textFromAny(nestedMap(params, "item")); text != "" {
				return text
			}
			return textFromAny(nestedMap(params, "content"))
		},
		isDoneNotification: func(frame rpcFrame) bool {
			return stringFromMap(frame, "method") == "turn/completed"
		},
	}
}
