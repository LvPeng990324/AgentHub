package adapter

// 第 2 轮 QA 回归（D-02）：独立验证「会话恢复被拒 → 降级新会话」的**类型覆盖面**，
// 不复用工程师的用例。重点：
//  1) S 族 4 种是否真的共用同一判定（不是 claude 特判）—— 另挑 qwen 实跑降级；
//  2) J 族 openclaw 的拒绝判定路径（按描述符/文案分别核对）；
//  3) 已产出文本 / 停止生成 / 非拒绝类错误 三种「不得重跑」的负例；
//  4) opencode 零输出判 failed 的行为变更不留回归。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// TestQA2ResumeDescriptorCoverage 逐条核对 9 种描述符的恢复配置与拒绝判定路径，
// 固定「哪些类型靠文案匹配、哪些靠结构兜底」这一事实，防止后续静默漂移。
func TestQA2ResumeDescriptorCoverage(t *testing.T) {
	registry := NewRegistry()

	// S 族 4 种：ResumeArg + --resume，拒绝判定依赖文案正则。
	streamTypes := []string{
		protocol.AgentTypeClaude, protocol.AgentTypeCodeBuddy,
		protocol.AgentTypeQwen, protocol.AgentTypeCursor,
	}
	for _, agentType := range streamTypes {
		descriptor, ok := registry.Get(agentType)
		if !ok {
			t.Fatalf("缺少描述符 %s", agentType)
		}
		if descriptor.Family != FamilyStream {
			t.Errorf("%s 应属于 S 族，实际 %s", agentType, descriptor.Family)
		}
		if descriptor.Resume != ResumeArg || descriptor.ResumeArgFlag != "--resume" {
			t.Errorf("%s 应为 ResumeArg + --resume，实际 %v/%q",
				agentType, descriptor.Resume, descriptor.ResumeArgFlag)
		}
		plan := BuildResume(descriptor, "dead-session")
		if len(plan.Args) != 2 || plan.Args[0] != "--resume" || plan.Args[1] != "dead-session" {
			t.Errorf("%s 续接 argv 异常: %v", agentType, plan.Args)
		}
		if ResumeRejectionUndetectable(agentType) {
			t.Errorf("%s 不应属于「不可区分」类型（应靠文案匹配）", agentType)
		}
		if !ClassifyResumeFailure(errText("Invalid session identifier"), descriptor) {
			t.Errorf("%s 的拒绝文案应被 ClassifyResumeFailure 命中", agentType)
		}
		if ClassifyResumeFailure(errText("connection reset by peer"), descriptor) {
			t.Errorf("%s 的网络错误不应被误判为恢复拒绝", agentType)
		}
	}

	// J 族 opencode：结构化兜底（任何失败都视为可降级）。
	opencode, _ := registry.Get(protocol.AgentTypeOpencode)
	if opencode.Resume != ResumeSessionIDArg || opencode.ResumeArgFlag != "--session" {
		t.Errorf("opencode 应为 ResumeSessionIDArg + --session，实际 %v/%q",
			opencode.Resume, opencode.ResumeArgFlag)
	}
	if !ResumeRejectionUndetectable(opencode.Type) {
		t.Error("opencode 应属于「恢复拒绝不可区分」类型（U4）")
	}

	// J 族 openclaw：不在 undetectable 集合，但 ResumeSessionIDArg 分支已结构性兜底，
	// 即「不依赖文案」也能命中——这正是团队关心的风险点，必须有断言固定。
	openclaw, ok := registry.Get(protocol.AgentTypeOpenclaw)
	if !ok {
		t.Fatal("缺少 openclaw 描述符")
	}
	if openclaw.Resume != ResumeSessionIDArg || openclaw.ResumeArgFlag != "--session-id" {
		t.Errorf("openclaw 应为 ResumeSessionIDArg + --session-id，实际 %v/%q",
			openclaw.Resume, openclaw.ResumeArgFlag)
	}
	if ResumeRejectionUndetectable(openclaw.Type) {
		t.Log("openclaw 已加入 undetectable 集合（若工程师后续调整，此断言需同步）")
	}
	// 任意错误文案（包括完全不含 resume 关键词的中文/网络错误）都必须命中。
	for _, message := range []string{
		"Invalid session identifier", "session not found",
		"connection refused", "exit status 1", "未知错误",
	} {
		if !ClassifyResumeFailure(errText(message), openclaw) {
			t.Errorf("openclaw（ResumeSessionIDArg）对 %q 应判定为可降级，实际未命中", message)
		}
	}

	// ACP / AS：各自解析器内处理，属于「文案 + 结构」双路径。
	for _, agentType := range []string{protocol.AgentTypeHermes, protocol.AgentTypeKimi} {
		descriptor, _ := registry.Get(agentType)
		if descriptor.Resume != ResumeSessionRPC {
			t.Errorf("%s 应为 ResumeSessionRPC，实际 %v", agentType, descriptor.Resume)
		}
		if !ClassifyResumeFailure(errText("Invalid session identifier"), descriptor) {
			t.Errorf("%s 的 ACP 恢复拒绝应被命中", agentType)
		}
	}
	codex, _ := registry.Get(protocol.AgentTypeCodex)
	if codex.Resume != ResumeThreadRPC {
		t.Errorf("codex 应为 ResumeThreadRPC，实际 %v", codex.Resume)
	}
	if !ClassifyResumeFailure(errText("boom"), codex) {
		t.Error("codex（thread/resume）任何失败都应降级（U4）")
	}
}

// errText 是 errors.New 的短名字，避免测试文件里反复 import errors。
func errText(message string) error { return &simpleError{message} }

type simpleError struct{ message string }

func (e *simpleError) Error() string { return e.message }

// TestQA2QwenResumeDowngrade qwen（S 族，非 claude）实跑降级，证明不是 claude 特判。
func TestQA2QwenResumeDowngrade(t *testing.T) {
	for _, agentType := range []string{protocol.AgentTypeQwen, protocol.AgentTypeCursor} {
		agentType := agentType
		t.Run(agentType, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			descriptor, ok := NewRegistry().Get(agentType)
			if !ok {
				t.Fatalf("缺少 %s 描述符", agentType)
			}
			plan := BuildResume(descriptor, "dead-session")
			process := newHelperProcess(t, ctx, "stream-resume-reject", descriptor, nil, plan.Args)
			process.WritePrompt("继续上一轮")

			session, err := NewStreamParser().Parse(ctx, process, descriptor, plan)
			if err != nil {
				t.Fatalf("Parse 失败: %v", err)
			}
			_, result := collectSession(t, session, 15*time.Second)

			if !result.ResumeRejected {
				t.Errorf("%s 恢复被拒必须置 ResumeRejected=true（实际 status=%s err=%q）",
					agentType, result.Status, result.Error)
			}
			if result.Status != ResultCompleted {
				t.Errorf("%s 降级后应完成本轮，实际 %s (%s)", agentType, result.Status, result.Error)
			}
			if result.SessionID == "" || result.SessionID == "dead-session" {
				t.Errorf("%s 降级后应回带新会话 id，实际 %q", agentType, result.SessionID)
			}
		})
	}
}

// TestQA2OpenclawResumeDowngrade openclaw（J 族整块 JSON）实跑降级：
// 拒绝时零输出 → 判 failed → 降级新会话 → 输出整块 JSON 完成。
func TestQA2OpenclawResumeDowngrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	descriptor, ok := NewRegistry().Get(protocol.AgentTypeOpenclaw)
	if !ok {
		t.Fatal("缺少 openclaw 描述符")
	}
	plan := BuildResume(descriptor, "dead-session")
	if len(plan.Args) != 2 || plan.Args[0] != "--session-id" || plan.Args[1] != "dead-session" {
		t.Fatalf("openclaw 续接 argv 异常: %v", plan.Args)
	}

	process := newHelperProcess(t, ctx, "json-block-resume-reject", descriptor, nil, plan.Args)
	process.WritePrompt("继续上一轮")
	session, err := NewJSONParser().Parse(ctx, process, descriptor, plan)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	messages, result := collectSession(t, session, 20*time.Second)

	if !result.ResumeRejected {
		t.Errorf("openclaw 恢复被拒必须降级并置 ResumeRejected=true（实际 status=%s err=%q）",
			result.Status, result.Error)
	}
	if result.Status != ResultCompleted {
		t.Errorf("openclaw 降级后应完成本轮，实际 %s (%s)", result.Status, result.Error)
	}
	if result.SessionID != "ses-openclaw-fresh" {
		t.Errorf("openclaw 降级后应回带新会话 id，实际 %q", result.SessionID)
	}
	if !strings.Contains(joinMessages(messages), "openclaw 新会话回复") {
		t.Errorf("降级后的整块输出应投递给前端，实际 %q", joinMessages(messages))
	}
}

// TestQA2NonResumeFailureDoesNotRerun 非「恢复被拒」的失败不得触发重跑（避免任何失败都重开进程）。
func TestQA2NonResumeFailureDoesNotRerun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeClaude)
	plan := BuildResume(descriptor, "dead-session")

	process := newHelperProcess(t, ctx, "stream-resume-error-other", descriptor, nil, plan.Args)
	process.WritePrompt("继续上一轮")

	var restartCalls atomic.Int32
	original := process.restart
	if original == nil {
		t.Fatal("测试前提：Process.restart 应由 helper 注入")
	}
	process.restart = func(restartCtx context.Context, resumeID string) (*Process, error) {
		restartCalls.Add(1)
		return original(restartCtx, resumeID)
	}

	session, err := NewStreamParser().Parse(ctx, process, descriptor, plan)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	_, result := collectSession(t, session, 15*time.Second)

	if result.Status != ResultFailed {
		t.Errorf("网络类错误应保持 failed，实际 %s", result.Status)
	}
	if result.ResumeRejected {
		t.Error("非恢复拒绝类错误不得置 ResumeRejected")
	}
	if got := restartCalls.Load(); got != 0 {
		t.Errorf("非恢复拒绝类错误不得重开进程，实际 restart 调用 %d 次", got)
	}
}

// TestQA2OpencodeZeroOutputBehaviour opencode 行为变更复核：
// 零输出 → failed（不产空气泡）；有输出 → completed（不受影响）。
func TestQA2OpencodeZeroOutputBehaviour(t *testing.T) {
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeOpencode)

	t.Run("零输出判失败", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		plan := BuildResume(descriptor, "")
		process := newHelperProcess(t, ctx, "json-silent", descriptor, nil, nil)
		process.WritePrompt("问题")
		session, err := NewJSONParser().Parse(ctx, process, descriptor, plan)
		if err != nil {
			t.Fatalf("Parse 失败: %v", err)
		}
		messages, result := collectSession(t, session, 15*time.Second)
		if result.Status != ResultFailed {
			t.Errorf("opencode 零输出应判 failed，实际 %s", result.Status)
		}
		if len(messages) != 0 {
			t.Errorf("零输出不应产生任何气泡，实际 %d 条: %q", len(messages), joinMessages(messages))
		}
	})

	t.Run("有输出仍完成", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		plan := BuildResume(descriptor, "")
		process := newHelperProcess(t, ctx, "json-lines", descriptor, nil, nil)
		process.WritePrompt("问题")
		session, err := NewJSONParser().Parse(ctx, process, descriptor, plan)
		if err != nil {
			t.Fatalf("Parse 失败: %v", err)
		}
		messages, result := collectSession(t, session, 15*time.Second)
		if result.Status != ResultCompleted {
			t.Errorf("有输出的 opencode 应完成，实际 %s (%s)", result.Status, result.Error)
		}
		if !strings.Contains(joinMessages(messages), "行式A") {
			t.Errorf("输出应投递给前端，实际 %q", joinMessages(messages))
		}
	})
}

// TestQA2StopBeforeAnyOutputDoesNotDowngrade 恢复请求尚未产出任何文本时执行「停止生成」：
// 终态必须是 cancelled（→ chat.done{stopped}），且绝不能因为「零输出 + 有 resume」被误判成
// 恢复被拒而拉起新进程（否则「停止」会变成「停止后又偷偷重开一个会话」）。
func TestQA2StopBeforeAnyOutputDoesNotDowngrade(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	descriptor, _ := NewRegistry().Get(protocol.AgentTypeClaude)
	plan := BuildResume(descriptor, "ses-existing")
	process := newHelperProcess(t, ctx, "stream-hang-silent", descriptor, nil, plan.Args)
	process.WritePrompt("继续上一轮")

	var restartCalls atomic.Int32
	original := process.restart
	if original == nil {
		t.Fatal("测试前提：Process.restart 应由 helper 注入")
	}
	process.restart = func(restartCtx context.Context, resumeID string) (*Process, error) {
		restartCalls.Add(1)
		return original(restartCtx, resumeID)
	}

	session, err := NewStreamParser().Parse(ctx, process, descriptor, plan)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	// 给解析器一点时间进入「等输出」状态，再模拟 chat.stop。
	time.Sleep(300 * time.Millisecond)
	cancel()
	process.KillTree()

	messages, result := collectSession(t, session, 15*time.Second)
	if result.Status != ResultCancelled {
		t.Errorf("停止生成应映射为 cancelled（→ stopped），实际 %s", result.Status)
	}
	if result.ResumeRejected {
		t.Error("停止生成不得标记 ResumeRejected")
	}
	if got := restartCalls.Load(); got != 0 {
		t.Errorf("停止生成不得重开进程，实际 restart 调用 %d 次", got)
	}
	if len(messages) != 0 {
		t.Errorf("零输出场景不应产生气泡，实际 %d 条", len(messages))
	}
}
