package adapter

// 单一进程构造点（§8.5）：exec.CommandContext + 三管道 + 平台钩子 + 日志脱敏。
// ★prompt 永远走 stdin，绝不进 argv（Windows .cmd 重分词 / Linux E2BIG / CreateProcess 32767 上限）。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// Process 是一个正在运行的 CLI turn 的全部资源。
type Process struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser

	cmd        *exec.Cmd
	handle     *procHandle
	descriptor Descriptor
	startedAt  time.Time

	killOnce    sync.Once
	waitOnce    sync.Once
	stderrIssue atomic.Value // string，kimi stderr 4xx/5xx 嗅探结果

	// restart 用同一描述符/选项重开一个进程（仅替换 resume argv），
	// 供 S/J 族解析器做「会话恢复被拒 → 降级新会话」；由 Launcher.Start 注入。
	// 为 nil 表示不支持重启（例如测试里手工构造的 Process）。
	restart func(ctx context.Context, resumeID string) (*Process, error)

	// prompt 记录本轮 stdin 内容，供降级重跑时重写到新进程（prompt 只走 stdin）。
	promptMu sync.Mutex
	prompt   string
}

// savedPrompt 返回本轮 prompt（供 restart 重写）。
func (p *Process) savedPrompt() string {
	p.promptMu.Lock()
	defer p.promptMu.Unlock()
	return p.prompt
}

// StartedAt 返回进程启动时间（用于 duration_ms）。
func (p *Process) StartedAt() time.Time {
	return p.startedAt
}

// StderrIssue 返回 stderr 嗅探到的中文错误文案（无则空串）。
func (p *Process) StderrIssue() string {
	if value, ok := p.stderrIssue.Load().(string); ok {
		return value
	}
	return ""
}

// WritePrompt 把 prompt 写入 stdin 后关闭；绝不把 prompt 放进 argv。
// 同时记录下来，降级重跑时由 restart 自动重写到新进程。
func (p *Process) WritePrompt(prompt string) {
	p.promptMu.Lock()
	p.prompt = prompt
	p.promptMu.Unlock()
	go func() {
		defer func() { _ = p.Stdin.Close() }()
		if _, err := io.WriteString(p.Stdin, prompt); err != nil {
			// K7：不打印 prompt 全文，只记录类型与错误。
			slog.Warn("写入 prompt 到 agent stdin 失败", "type", p.descriptor.Type, "err", err)
		}
	}()
}

// KillTree 整树回收子进程（Unix 进程组 / Windows Job Object），杜绝孤儿 CLI。
func (p *Process) KillTree() {
	p.killOnce.Do(func() {
		p.handle.killTree()
	})
}

// Finish 收尾：先 kill 整树再 Wait，关闭句柄；可安全重复调用。
func (p *Process) Finish() {
	p.KillTree()
	p.waitOnce.Do(func() {
		_ = p.cmd.Wait()
		p.handle.close()
		_ = p.Stdin.Close()
	})
}

// Launcher 是所有 CLI 子进程的唯一构造点。
type Launcher struct {
	registry *Registry
}

// NewLauncher 构造 Launcher。
func NewLauncher(registry *Registry) *Launcher {
	return &Launcher{registry: registry}
}

// Start 构造并启动一个 CLI 进程。
// argv = rewriteCommand 前缀 + Subcommand + LaunchArgs + ResumePlan.Args + CustomArgs。
func (l *Launcher) Start(
	ctx context.Context,
	descriptor Descriptor,
	opts ExecOptions,
	plan ResumePlan,
) (*Process, error) {
	command, prefixArgs, cleanup, err := rewriteCommand(descriptor, opts.ExecPath)
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, len(prefixArgs)+len(descriptor.Subcommand)+len(descriptor.LaunchArgs)+len(plan.Args))
	args = append(args, prefixArgs...)
	args = append(args, descriptor.Subcommand...)
	args = append(args, expandLaunchArgs(descriptor.LaunchArgs, opts.Cwd)...)
	args = append(args, plan.Args...)
	args = append(args, opts.CustomArgs...)

	cmd := exec.CommandContext(ctx, command, args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	handle, err := prepareProc(cmd, descriptor, opts.ExecPath, cleanup)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}
	// ctx 取消 / 单轮超时：直接回收整棵进程树（exec 默认实现只 Kill 主进程，
	// Windows 下 CLI 派生的子进程会残留），保证超时后不留孤儿。
	cmd.Cancel = func() error {
		handle.killTree()
		return os.ErrProcessDone
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		handle.close()
		return nil, fmt.Errorf("创建 stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		handle.close()
		return nil, fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		handle.close()
		return nil, fmt.Errorf("创建 stderr 管道失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		handle.close()
		return nil, fmt.Errorf("启动 CLI 进程失败: %w", err)
	}
	if err := handle.attach(cmd); err != nil {
		handle.killTree()
		_ = cmd.Wait()
		handle.close()
		return nil, err
	}

	process := &Process{
		Stdin:      stdin,
		Stdout:     stdout,
		Stderr:     stderr,
		cmd:        cmd,
		handle:     handle,
		descriptor: descriptor,
		startedAt:  time.Now(),
	}
	// 注入「降级重跑」能力：解析器检测到恢复被拒时可换新会话重开，无需知道 Launcher 细节。
	process.restart = func(restartCtx context.Context, resumeID string) (*Process, error) {
		nextOptions := opts
		nextOptions.ResumeSessionID = resumeID
		restarted, restartErr := l.Start(restartCtx, descriptor, nextOptions, BuildResume(descriptor, resumeID))
		if restartErr != nil {
			return nil, restartErr
		}
		// 重跑必须重写同一 prompt（prompt 永远走 stdin，不进 argv）。
		if prompt := process.savedPrompt(); prompt != "" {
			restarted.WritePrompt(prompt)
		}
		return restarted, nil
	}

	go process.logStderr()
	// K7：不打印完整 argv / prompt，只记录类型与参数个数。
	slog.Debug("已启动 agent CLI", "type", descriptor.Type, "args_count", len(args))
	return process, nil
}

// expandLaunchArgs 把 cursor 的 <cwd> 占位替换为真实工作目录。
// cwd 无法解析时**整对跳过**（如 `--workspace <cwd>`），绝不产出 `--workspace ""` 这种非法参数。
func expandLaunchArgs(args []string, cwd string) []string {
	if cwd == "" {
		if workingDir, err := os.Getwd(); err == nil {
			cwd = workingDir
		}
	}
	expanded := make([]string, 0, len(args))
	for _, arg := range args {
		if arg != "<cwd>" {
			expanded = append(expanded, arg)
			continue
		}
		if cwd != "" {
			expanded = append(expanded, cwd)
			continue
		}
		// cwd 不可用：连同前一个 flag 一起丢弃（`--workspace` 没有值会被 CLI 当非法）。
		if len(expanded) > 0 && strings.HasPrefix(expanded[len(expanded)-1], "-") {
			expanded = expanded[:len(expanded)-1]
		}
	}
	return expanded
}

var httpStatusPattern = regexp.MustCompile(`(?i)\b(4\d{2}|5\d{2})\b`)

// logStderr 结构化记录 stderr；kimi 需嗅探 4xx/5xx 转 chat.error（§8.8）。
// 日志不打印 stderr 原文（可能含敏感信息），只记录长度。
func (p *Process) logStderr() {
	scanner := bufio.NewScanner(p.Stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(trimTrailingCR(scanner.Text()))
		if line == "" {
			continue
		}
		if p.descriptor.Type == protocol.AgentTypeKimi && httpStatusPattern.MatchString(line) {
			p.stderrIssue.Store(protocol.ErrorMessage(protocol.CodeCLIStartFailed))
			slog.Warn("kimi stderr 嗅探到 HTTP 错误状态", "bytes", len(line))
		} else {
			slog.Debug("agent stderr", "type", p.descriptor.Type, "bytes", len(line))
		}
	}
}
