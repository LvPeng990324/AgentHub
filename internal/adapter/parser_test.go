package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

func joinMessages(messages []Message) string {
	var builder strings.Builder
	for _, message := range messages {
		builder.WriteString(message.Content)
	}
	return builder.String()
}

// TestScannerLongLine 校验 32MiB 单行上限：20MiB 不截断、去尾部 \r；33MiB 明确报错。
func TestScannerLongLine(t *testing.T) {
	long := strings.Repeat("x", 20<<20)
	scanner := newAgentStreamScanner(strings.NewReader(long + "\r\n"))
	if !scanner.Scan() {
		t.Fatalf("20MiB 长行不应被截断: %v", scanner.Err())
	}
	if got := trimTrailingCR(scanner.Text()); got != long {
		t.Errorf("长行内容不一致：期望 %d 字节，实际 %d", len(long), len(got))
	}
	oversized := newAgentStreamScanner(strings.NewReader(strings.Repeat("y", 33<<20) + "\n"))
	if oversized.Scan() {
		t.Error("超过 32MiB 上限的行应被拒绝")
	}
	if oversized.Err() == nil {
		t.Error("超限时应返回 scanner 错误")
	}
}

// TestStreamFamilyEndToEnd：S 族真进程假 CLI → 流式 chunk → result → done(session_id)。
func TestStreamFamilyEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeClaude)
	process := newHelperProcess(t, ctx, "stream-ok", descriptor, nil, nil)
	process.WritePrompt("测试问题")
	session, err := NewStreamParser().Parse(ctx, process, descriptor, BuildResume(descriptor, ""))
	if err != nil {
		t.Fatalf("S 族 Parse 失败: %v", err)
	}
	messages, result := collectSession(t, session, 15*time.Second)
	if result.Status != ResultCompleted {
		t.Fatalf("期望 completed，实际 %s (%s)", result.Status, result.Error)
	}
	if result.SessionID != "ses-helper" {
		t.Errorf("应回带 session_id=ses-helper，实际 %q", result.SessionID)
	}
	text := joinMessages(messages)
	if !strings.Contains(text, "首轮") || !strings.Contains(text, ",流式") {
		t.Errorf("S 族文本增量缺失: %q", text)
	}
	if strings.Contains(text, "ignored") {
		t.Error("tool_use 事件不得进消息流（Q8）")
	}
}

// TestStreamFamilyResumeArg：带 resume_id 续接时 argv 追加 --resume，假 CLI 输出续接内容。
func TestStreamFamilyResumeArg(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeClaude)
	plan := BuildResume(descriptor, "ses-helper")
	if len(plan.Args) != 2 || plan.Args[0] != "--resume" || plan.Args[1] != "ses-helper" {
		t.Fatalf("续接 argv 错误: %v", plan.Args)
	}
	process := newHelperProcess(t, ctx, "stream-ok", descriptor, nil, plan.Args)
	process.WritePrompt("继续")
	session, err := NewStreamParser().Parse(ctx, process, descriptor, plan)
	if err != nil {
		t.Fatalf("续接 Parse 失败: %v", err)
	}
	messages, result := collectSession(t, session, 15*time.Second)
	if result.Status != ResultCompleted {
		t.Fatalf("续接应完成，实际 %s", result.Status)
	}
	if !strings.Contains(joinMessages(messages), "[续接]") {
		t.Error("假 CLI 未收到 --resume，续接链路未生效")
	}
}

// TestStreamFamilyFailClosed：没有 result 事件必须判 failed，避免半截气泡冒充完成。
func TestStreamFamilyFailClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeClaude)
	process := newHelperProcess(t, ctx, "stream-no-result", descriptor, nil, nil)
	process.WritePrompt("测试问题")
	session, err := NewStreamParser().Parse(ctx, process, descriptor, BuildResume(descriptor, ""))
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	messages, result := collectSession(t, session, 15*time.Second)
	if result.Status != ResultFailed {
		t.Fatalf("无 result 事件应判 failed，实际 %s", result.Status)
	}
	if !strings.Contains(result.Error, "result 事件") {
		t.Errorf("失败原因应说明缺 result 事件: %q", result.Error)
	}
	if !strings.Contains(joinMessages(messages), "半截") {
		t.Error("已产出的半截内容仍应投递给前端")
	}
}

// TestStreamFamilyStopKeepsOutput：stop（cancel + KillTree）→ cancelled，已产出内容保留。
func TestStreamFamilyStopKeepsOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeClaude)
	process := newHelperProcess(t, ctx, "stream-hang", descriptor, nil, nil)
	process.WritePrompt("测试问题")
	session, err := NewStreamParser().Parse(ctx, process, descriptor, BuildResume(descriptor, ""))
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	var produced strings.Builder
	select {
	case message, ok := <-session.Messages:
		if ok {
			produced.WriteString(message.Content)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("等待首个 chunk 超时")
	}
	cancel()
	process.KillTree() // 模拟 Launcher.KillTree 回收整树
	deadline := time.After(10 * time.Second)
	for {
		select {
		case message, ok := <-session.Messages:
			if !ok {
				result := <-session.Result
				if result.Status != ResultCancelled {
					t.Errorf("停止应映射 cancelled，实际 %s", result.Status)
				}
				if mapResultStatus(result.Status) != protocol.DoneStatusStopped {
					t.Error("cancelled 应映射为 chat.done{stopped}")
				}
				if !strings.Contains(produced.String(), "已产出") {
					t.Errorf("停止后应保留已产出内容: %q", produced.String())
				}
				return
			}
			produced.WriteString(message.Content)
		case <-deadline:
			t.Fatal("停止后 Session 未收尾")
		}
	}
}

// TestJSONOpencodeLines：J 族行式 JSON 逐行解析，EOF 为正常边界。
func TestJSONOpencodeLines(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeOpencode)
	process := newHelperProcess(t, ctx, "json-lines", descriptor, nil, nil)
	process.WritePrompt("测试问题")
	session, err := NewJSONParser().Parse(ctx, process, descriptor, BuildResume(descriptor, ""))
	if err != nil {
		t.Fatalf("J 族 Parse 失败: %v", err)
	}
	messages, result := collectSession(t, session, 15*time.Second)
	if result.Status != ResultCompleted {
		t.Fatalf("行式 JSON 应完成，实际 %s (%s)", result.Status, result.Error)
	}
	text := joinMessages(messages)
	if !strings.Contains(text, "行式A") || !strings.Contains(text, "行式B") {
		t.Errorf("行式 JSON 文本缺失: %q", text)
	}
}

// TestJSONOpenclawIdleGrace：整块 JSON 输出后进程挂起，必须在 idle grace 内收尾（不等 EOF）。
func TestJSONOpenclawIdleGrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeOpenclaw)
	descriptor.IdleGraceMs = 150 // 加速测试；生产为 2000
	process := newHelperProcess(t, ctx, "json-block", descriptor, nil, nil)
	process.WritePrompt("测试问题")
	startedAt := time.Now()
	session, err := NewJSONParser().Parse(ctx, process, descriptor, BuildResume(descriptor, ""))
	if err != nil {
		t.Fatalf("openclaw Parse 失败: %v", err)
	}
	messages, result := collectSession(t, session, 8*time.Second)
	elapsed := time.Since(startedAt)
	if elapsed > 5*time.Second {
		t.Errorf("不得等待进程 EOF（假 CLI 挂起 10 分钟），实际耗时 %v", elapsed)
	}
	if result.Status != ResultCompleted {
		t.Fatalf("整块 JSON 应完成，实际 %s (%s)", result.Status, result.Error)
	}
	if !strings.Contains(joinMessages(messages), "整块结果") {
		t.Errorf("整块 JSON 文本缺失: %q", joinMessages(messages))
	}
	if result.SessionID != "ses-json-block" {
		t.Errorf("应回带 session_id，实际 %q", result.SessionID)
	}
}

// TestACPFamilyEndToEnd：A 族 JSON-RPC 握手 → session/new → prompt（含权限自动应答）→ 增量 → done。
func TestACPFamilyEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeHermes)
	process := newHelperProcess(t, ctx, "acp", descriptor, nil, nil)
	session, err := NewACPParser().Parse(ctx, process, descriptor, "你好",
		BuildResume(descriptor, ""), ExecOptions{HandshakeTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("A 族 Parse 失败: %v", err)
	}
	messages, result := collectSession(t, session, 15*time.Second)
	if result.Status != ResultCompleted {
		t.Fatalf("ACP 应完成，实际 %s (%s)", result.Status, result.Error)
	}
	if result.SessionID != "ses-acp" {
		t.Errorf("应回带 sessionId，实际 %q", result.SessionID)
	}
	text := joinMessages(messages)
	if !strings.Contains(text, "ACP-A") || !strings.Contains(text, "ACP-B") {
		t.Errorf("ACP session/update 增量缺失或权限自动应答失败: %q", text)
	}
}

// TestACPResumeRejectedDowngrade：恢复被拒 → 降级 session/new + ResumeRejected=true。
func TestACPResumeRejectedDowngrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeKimi)
	process := newHelperProcess(t, ctx, "acp", descriptor,
		[]string{helperEnvReject + "=1"}, nil)
	plan := BuildResume(descriptor, "dead-session-id")
	if plan.Method != protocol.ResumeKindSessionResume {
		t.Fatalf("应生成 session/resume 计划，实际 %s", plan.Method)
	}
	session, err := NewACPParser().Parse(ctx, process, descriptor, "你好",
		plan, ExecOptions{HandshakeTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("A 族 Parse 失败: %v", err)
	}
	_, result := collectSession(t, session, 15*time.Second)
	if !result.ResumeRejected {
		t.Error("恢复被拒应置 ResumeRejected=true")
	}
	if result.Status != ResultCompleted {
		t.Errorf("降级后应正常完成，实际 %s (%s)", result.Status, result.Error)
	}
}

// TestAppServerFamilyEndToEnd：AS 族 initialize → thread/start → turn/start → turn/completed。
func TestAppServerFamilyEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeCodex)
	process := newHelperProcess(t, ctx, "appserver", descriptor, nil, nil)
	session, err := NewAppServerParser().Parse(ctx, process, descriptor, "你好",
		BuildResume(descriptor, ""), ExecOptions{HandshakeTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("AS 族 Parse 失败: %v", err)
	}
	messages, result := collectSession(t, session, 15*time.Second)
	if result.Status != ResultCompleted {
		t.Fatalf("app-server 应完成，实际 %s (%s)", result.Status, result.Error)
	}
	if result.SessionID != "thr-appserver" {
		t.Errorf("应回带 threadId，实际 %q", result.SessionID)
	}
	if !strings.Contains(joinMessages(messages), "AS-A") {
		t.Errorf("app-server item/* 增量缺失: %q", joinMessages(messages))
	}
}

// TestProcessTreeKill 是进程树级清理的运行时证据：
// 父假 CLI 派生一个子假 CLI，两者持续写同一个心跳文件；KillTree 后文件必须停止增长。
func TestProcessTreeKill(t *testing.T) {
	tickFile := filepath.Join(t.TempDir(), "tick.log")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	process := newHelperProcess(t, ctx, "parent-tick", Descriptor{},
		[]string{helperEnvTickFile + "=" + tickFile}, nil)

	waitFor(t, 8*time.Second, func() bool {
		info, err := os.Stat(tickFile)
		return err == nil && info.Size() > 0
	}, "父子进程应持续写心跳文件")
	process.KillTree()
	time.Sleep(600 * time.Millisecond)
	sizeAfterKill := fileSize(t, tickFile)
	time.Sleep(600 * time.Millisecond)
	sizeLater := fileSize(t, tickFile)
	if sizeLater != sizeAfterKill {
		t.Errorf("KillTree 后仍有存活（孤儿）进程写心跳文件: %d -> %d 字节", sizeAfterKill, sizeLater)
	}
	process.Finish()
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return info.Size()
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(message)
}
