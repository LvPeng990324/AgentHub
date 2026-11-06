package adapter

// `<cmd> --version` 探测（§8.4）：3s 超时、输出截断 200B、失败填 "unknown"。
// 版本只是 Q14 只读展示，探测失败不得影响该 agent 被发现与注册（U8）。

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultVersionTimeout     = 3 * time.Second
	defaultVersionOutputLimit = 200
	unknownVersion            = "unknown"
)

// VersionProbe 可配置的版本探测器（便于单测覆盖超时与失败路径）。
// Runner 允许注入子进程执行逻辑；nil 时使用默认的 `<cmd> --version`。
type VersionProbe struct {
	Timeout     time.Duration
	OutputLimit int
	Runner      func(ctx context.Context, execPath string) ([]byte, error)
}

// NewVersionProbe 构造默认探测器。
func NewVersionProbe() *VersionProbe {
	return &VersionProbe{Timeout: defaultVersionTimeout, OutputLimit: defaultVersionOutputLimit}
}

// Run 返回版本字符串；任何失败、超时、空输出一律返回 "unknown"。
// 日志中不打印完整输出（可能含环境信息），只由调用方记录结果。
func (p *VersionProbe) Run(ctx context.Context, execPath string) string {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultVersionTimeout
	}
	limit := p.OutputLimit
	if limit <= 0 {
		limit = defaultVersionOutputLimit
	}
	if strings.TrimSpace(execPath) == "" {
		return unknownVersion
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var raw []byte
	if p.Runner != nil {
		output, err := p.Runner(probeCtx, execPath)
		if err != nil {
			return unknownVersion
		}
		raw = output
	} else {
		cmd := exec.CommandContext(probeCtx, execPath, "--version")
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output // 部分 CLI 把版本打到 stderr
		if err := cmd.Run(); err != nil {
			return unknownVersion
		}
		raw = output.Bytes()
	}
	if len(raw) > limit {
		raw = raw[:limit]
	}
	text := strings.TrimSpace(string(raw))
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		text = text[:index]
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return unknownVersion
	}
	return text
}

// probeVersion 是默认探测器的快捷入口。
func probeVersion(ctx context.Context, execPath string) string {
	return NewVersionProbe().Run(ctx, execPath)
}
