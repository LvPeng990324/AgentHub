package adapter

// 假 CLI 测试助手：用测试二进制自身作为子进程（GO_WANT_HELPER 模式），
// 覆盖 4 个协议族的真实 stdin/stdout 交互、流式输出、恢复续接、停止生成与进程树清理。
// 这样无需依赖任何真实 agent CLI，也不需要 Node/Python 运行时。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const helperEnvFlag = "AGENTHUB_HELPER"
const helperEnvMode = "AGENTHUB_HELPER_MODE"
const helperEnvTickFile = "AGENTHUB_TICK_FILE"
const helperEnvReject = "AGENTHUB_HELPER_REJECT"

// TestHelperProcess 在子进程中被 `-test.run=TestHelperProcess` 调用。
// 它按 AGENTHUB_HELPER_MODE 扮演某一种假 CLI，然后 os.Exit 结束。
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvFlag) != "1" {
		return
	}
	mode := os.Getenv(helperEnvMode)
	write := func(text string) { _, _ = fmt.Fprintln(os.Stdout, text) }
	writeJSON := func(value any) {
		encoded, err := json.Marshal(value)
		if err == nil {
			write(string(encoded))
		}
	}
	resumeRequested := func() bool {
		for _, arg := range os.Args {
			if arg == "--resume" || arg == "--session" || arg == "--session-id" {
				return true
			}
		}
		return false
	}

	switch mode {
	case "stream-ok":
		_, _ = io.ReadAll(os.Stdin) // prompt 走 stdin，等待写端关闭
		if resumeRequested() {
			write(`{"type":"content_block_delta","delta":{"text":"[续接]"}}`)
		} else {
			write(`{"type":"assistant","message":{"content":[{"type":"text","text":"首轮"}]},"session_id":"ses-helper"}`)
			write(`{"type":"content_block_delta","delta":{"text":",流式"}}`)
			write(`{"type":"tool_use","name":"ignored"}`)
		}
		write(`{"type":"result","subtype":"success","session_id":"ses-helper"}`)
	case "stream-no-result":
		_, _ = io.ReadAll(os.Stdin)
		write(`{"type":"content_block_delta","delta":{"text":"半截"}}`)
	case "stream-resume-reject":
		// QA 用例：模拟 CLI 明确拒绝一个已失效的会话恢复 id（claude --resume <dead-id>）。
		_, _ = io.ReadAll(os.Stdin)
		if resumeRequested() {
			write(`{"type":"result","subtype":"error_during_execution","is_error":true,` +
				`"error":"Invalid session identifier","session_id":"dead-session"}`)
			os.Exit(1)
		}
		write(`{"type":"result","subtype":"success","session_id":"ses-fresh"}`)
	case "json-resume-reject":
		// 回归用例：opencode 恢复被拒时 CLI 无输出直接退出（行式 JSON 的 EOF 属正常边界，
		// 但零输出必须判失败，才能触发「降级新会话」）。
		_, _ = io.ReadAll(os.Stdin)
		if resumeRequested() {
			os.Exit(1)
		}
		write(`{"type":"text","text":"新会话回复"}`)
		write(`{"type":"result","session_id":"ses-json-fresh"}`)
	case "stream-partial-then-reject":
		// 回归用例：已产出文本后才报恢复被拒 → 不得重跑（否则前端看到重复内容）。
		_, _ = io.ReadAll(os.Stdin)
		write(`{"type":"content_block_delta","delta":{"text":"半截输出"}}`)
		write(`{"type":"result","subtype":"error_during_execution","is_error":true,` +
			`"error":"Invalid session identifier","session_id":"dead-session"}`)
		os.Exit(1)
	case "stream-hang-silent":
		// QA 用例：续接请求且「零输出就挂住」，用于验证 stop 不会被误判成恢复被拒而重跑。
		_, _ = io.ReadAll(os.Stdin)
		time.Sleep(10 * time.Minute)
	case "json-block-resume-reject":
		// QA 用例：openclaw —— 续接请求被拒时无任何输出直接退出；
		// 不带 resume 时输出一整块合法 JSON（走 idle grace 收尾路径）。
		_, _ = io.ReadAll(os.Stdin)
		if resumeRequested() {
			os.Exit(1)
		}
		write(`{"text":"openclaw 新会话回复","session_id":"ses-openclaw-fresh"}`)
	case "json-silent":
		// QA 用例：opencode 零输出（无论是否续接）→ 必须判 failed，不得产出空气泡。
		_, _ = io.ReadAll(os.Stdin)
		os.Exit(1)
	case "stream-resume-error-other":
		// QA 用例：非「恢复被拒」的失败文案（如网络错误）不得触发降级重跑。
		_, _ = io.ReadAll(os.Stdin)
		if resumeRequested() {
			write(`{"type":"result","subtype":"error_during_execution","is_error":true,"error":"connection reset by peer"}`)
			os.Exit(1)
		}
		write(`{"type":"result","subtype":"success","session_id":"ses-fresh"}`)
	case "hang-with-child":
		// 超时用例：输出一条 chunk 后派生一个持续写心跳文件的子进程，然后挂起。
		// 超时必须整树回收，否则子进程会继续写文件（孤儿证据）。
		_, _ = io.ReadAll(os.Stdin)
		write(`{"type":"content_block_delta","delta":{"text":"挂起中"}}`)
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		child.Env = append(os.Environ(), helperEnvFlag+"=1", helperEnvMode+"=child-tick")
		if err := child.Start(); err == nil {
			write(fmt.Sprintf("child_pid=%d", child.Process.Pid))
		}
		time.Sleep(10 * time.Minute)
	case "argv-dump":
		// QA 用例：把 argv 落盘，供断言「prompt 绝不进 argv」。
		if path := os.Getenv("AGENTHUB_ARGV_FILE"); path != "" {
			_ = os.WriteFile(path, []byte(fmt.Sprintf("%q", os.Args)), 0o600)
		}
		os.Exit(0)
	case "stream-hang":
		_, _ = io.ReadAll(os.Stdin)
		write(`{"type":"content_block_delta","delta":{"text":"已产出"}}`)
		time.Sleep(10 * time.Minute) // 等 stop / KillTree
	case "json-lines":
		_, _ = io.ReadAll(os.Stdin)
		write(`{"type":"text","text":"行式A"}`)
		write(`{"type":"text","text":"行式B"}`)
		write(`{"type":"result","session_id":"ses-json-lines"}`)
	case "json-block":
		_, _ = io.ReadAll(os.Stdin)
		// 整块 JSON 输出完进程仍不退出，模拟 openclaw 实测的 8 分钟挂起。
		write(`{"text":"整块结果","session_id":"ses-json-block"}`)
		time.Sleep(10 * time.Minute)
	case "acp", "appserver":
		runRPCHelper(mode, writeJSON)
	case "version-ok":
		write("v1.2.3 fake-cli")
	case "version-fail":
		os.Exit(3)
	case "child-tick":
		tickLoop(os.Getenv(helperEnvTickFile))
		return
	case "parent-tick":
		// 给 Windows Job Object attach 留窗口，随后派生一个同样写心跳文件的子进程。
		time.Sleep(200 * time.Millisecond)
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		child.Env = append(os.Environ(), helperEnvFlag+"=1", helperEnvMode+"=child-tick")
		if err := child.Start(); err == nil {
			write(fmt.Sprintf("child_pid=%d", child.Process.Pid))
		}
		tickLoop(os.Getenv(helperEnvTickFile))
		return
	default:
		write("unknown helper mode")
	}
	os.Exit(0)
}

// runRPCHelper 扮演 ACP / app-server 的 JSON-RPC 端点。
func runRPCHelper(mode string, writeJSON func(any)) {
	result := func(id any, payload any) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "result": payload}
	}
	notification := func(method string, params any) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	}
	chunk := func(method string, text string) map[string]any {
		if mode == "acp" {
			return notification(method, map[string]any{
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": text},
				},
			})
		}
		return notification(method, map[string]any{
			"item": map[string]any{"type": "agentMessage", "text": text},
		})
	}
	// 把 session/new | session/resume 的 params 落盘，供「cwd 必须非空且有效」的回归用例断言。
	dumpParams := func(method string, request map[string]any) {
		base := os.Getenv("AGENTHUB_ACP_PARAMS_FILE")
		if base == "" {
			return
		}
		file := base + "-" + strings.ReplaceAll(method, "/", "_") + ".json"
		payload := map[string]any{"method": method, "params": request["params"]}
		if encoded, err := json.Marshal(payload); err == nil {
			_ = os.WriteFile(file, encoded, 0o600)
		}
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request map[string]any
		if err := json.Unmarshal([]byte(scanner.Text()), &request); err != nil {
			continue
		}
		method, _ := request["method"].(string)
		id := request["id"]
		if mode == "acp" {
			switch method {
			case "initialize":
				writeJSON(result(id, map[string]any{"protocolVersion": 1}))
			case "session/new":
				dumpParams(method, request)
				writeJSON(result(id, map[string]any{"sessionId": "ses-acp"}))
			case "session/resume":
				dumpParams(method, request)
				if os.Getenv(helperEnvReject) == "1" {
					writeJSON(map[string]any{"jsonrpc": "2.0", "id": id,
						"error": map[string]any{"message": "Invalid session identifier"}})
				} else {
					writeJSON(result(id, map[string]any{"sessionId": "ses-acp"}))
				}
			case "session/prompt":
				// 服务端主动请求权限，客户端必须自动应答，否则这里会一直阻塞。
				writeJSON(map[string]any{"jsonrpc": "2.0", "id": 777,
					"method": "session/request_permission", "params": map[string]any{}})
				if !scanner.Scan() {
					return
				}
				writeJSON(chunk("session/update", "ACP-A"))
				writeJSON(chunk("session/update", "ACP-B"))
				writeJSON(result(id, map[string]any{}))
			}
		} else {
			switch method {
			case "initialize":
				writeJSON(result(id, map[string]any{"protocolVersion": 1}))
			case "thread/start":
				writeJSON(result(id, map[string]any{"threadId": "thr-appserver"}))
			case "thread/resume":
				if os.Getenv(helperEnvReject) == "1" {
					writeJSON(map[string]any{"jsonrpc": "2.0", "id": id,
						"error": map[string]any{"message": "invalid thread id"}})
				} else {
					writeJSON(result(id, map[string]any{"threadId": "thr-appserver"}))
				}
			case "turn/start":
				writeJSON(chunk("item/updated", "AS-A"))
				writeJSON(chunk("item/updated", "AS-B"))
				writeJSON(result(id, map[string]any{"turnId": "turn-1"}))
				writeJSON(notification("turn/completed", map[string]any{"turnId": "turn-1"}))
			}
		}
	}
}

func tickLoop(path string) {
	if path == "" {
		return
	}
	for {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = file.WriteString("tick\n")
			_ = file.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startHelperProcess 启动一个假 CLI 子进程（不依赖 *testing.T，可在 restart 闭包中被调用）。
// 注入 restart 能力：用 BuildResume 生成新的恢复 argv（--resume / --session / --session-id），
// 供解析器层做「恢复被拒 → 降级新会话」重跑。
func startHelperProcess(
	ctx context.Context,
	mode string,
	descriptor Descriptor,
	extraEnv []string,
	extraArgs []string,
) (*Process, error) {
	args := []string{"-test.run=TestHelperProcess", "--"}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	env := append(os.Environ(), helperEnvFlag+"=1", helperEnvMode+"="+mode)
	env = append(env, extraEnv...)
	cmd.Env = env

	handle, err := prepareProc(cmd, descriptor, os.Args[0], func() {})
	if err != nil {
		return nil, fmt.Errorf("prepareProc 失败: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		handle.close()
		return nil, fmt.Errorf("StdinPipe 失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		handle.close()
		return nil, fmt.Errorf("StdoutPipe 失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		handle.close()
		return nil, fmt.Errorf("StderrPipe 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		handle.close()
		return nil, fmt.Errorf("启动假 CLI 失败: %w", err)
	}
	if err := handle.attach(cmd); err != nil {
		handle.killTree()
		_ = cmd.Wait()
		handle.close()
		return nil, fmt.Errorf("attach 进程句柄失败: %w", err)
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
	process.restart = func(restartCtx context.Context, resumeID string) (*Process, error) {
		restarted, restartErr := startHelperProcess(restartCtx, mode, descriptor, extraEnv,
			BuildResume(descriptor, resumeID).Args)
		if restartErr != nil {
			return nil, restartErr
		}
		if prompt := process.savedPrompt(); prompt != "" {
			restarted.WritePrompt(prompt)
		}
		return restarted, nil
	}
	return process, nil
}

// newHelperProcess 是 startHelperProcess 的测试便捷包装（失败即 Fatalf）。
func newHelperProcess(
	t *testing.T,
	ctx context.Context,
	mode string,
	descriptor Descriptor,
	extraEnv []string,
	extraArgs []string,
) *Process {
	t.Helper()
	process, err := startHelperProcess(ctx, mode, descriptor, extraEnv, extraArgs)
	if err != nil {
		t.Fatalf("启动假 CLI 失败: %v", err)
	}
	return process
}

// collectSession 读取一个 Session 的全部消息与终态（带超时防挂）。
func collectSession(t *testing.T, session *Session, timeout time.Duration) ([]Message, Result) {
	t.Helper()
	messages := make([]Message, 0)
	deadline := time.After(timeout)
	for {
		select {
		case message, ok := <-session.Messages:
			if !ok {
				select {
				case result := <-session.Result:
					return messages, result
				case <-deadline:
					t.Fatal("等待 Result 超时")
				}
			}
			messages = append(messages, message)
		case <-deadline:
			t.Fatal("等待 Session 结束超时")
		}
	}
}
