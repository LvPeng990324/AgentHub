package adapter

// 回归用例（D-02）：会话恢复被拒 → 降级新会话的两条补充断言。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// TestJSONResumeRejectedDowngradesForOpencode opencode 属于「恢复拒绝不可区分」类型（U4）：
// CLI 无输出直接退出时必须判失败并降级新会话，否则会一直带着死 resume_id 重试。
func TestJSONResumeRejectedDowngradesForOpencode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, ok := NewRegistry().Get(protocol.AgentTypeOpencode)
	if !ok {
		t.Fatal("缺少 opencode 描述符")
	}
	if !ResumeRejectionUndetectable(descriptor.Type) {
		t.Fatal("opencode 应属于恢复拒绝不可区分类型")
	}
	plan := BuildResume(descriptor, "dead-session")
	if len(plan.Args) != 2 || plan.Args[1] != "dead-session" {
		t.Fatalf("opencode 续接计划异常: %v", plan.Args)
	}

	process := newHelperProcess(t, ctx, "json-resume-reject", descriptor, nil, plan.Args)
	process.WritePrompt("继续上一轮")
	session, err := NewJSONParser().Parse(ctx, process, descriptor, plan)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	messages, result := collectSession(t, session, 15*time.Second)

	if !result.ResumeRejected {
		t.Errorf("opencode 恢复失败必须降级并置 ResumeRejected=true（实际 status=%s err=%q）",
			result.Status, result.Error)
	}
	if result.Status != ResultCompleted {
		t.Errorf("降级后应完成本轮，实际 %s (%s)", result.Status, result.Error)
	}
	if result.SessionID != "ses-json-fresh" {
		t.Errorf("降级后应回带新会话 id，实际 %q", result.SessionID)
	}
	if !strings.Contains(joinMessages(messages), "新会话回复") {
		t.Errorf("降级后应把新会话输出投递给前端，实际 %q", joinMessages(messages))
	}
}

// TestStreamPartialOutputThenRejectDoesNotRerun 已产出文本后才失败时绝不重跑：
// 否则前端会看到同一段内容出现两次。
func TestStreamPartialOutputThenRejectDoesNotRerun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeClaude)
	plan := BuildResume(descriptor, "dead-session")

	process := newHelperProcess(t, ctx, "stream-partial-then-reject", descriptor, nil, plan.Args)
	process.WritePrompt("继续上一轮")
	session, err := NewStreamParser().Parse(ctx, process, descriptor, plan)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	messages, result := collectSession(t, session, 15*time.Second)

	if result.Status != ResultFailed {
		t.Errorf("已产出后失败必须判 failed，实际 %s", result.Status)
	}
	if result.ResumeRejected {
		t.Error("已产出文本时不得重跑（ResumeRejected 应为 false）")
	}
	if got := strings.Count(joinMessages(messages), "半截输出"); got != 1 {
		t.Errorf("已产出内容只应投递一次（防重复输出），实际出现 %d 次: %q",
			got, joinMessages(messages))
	}
}

// TestStreamStopCancelsWithoutDowngrade 「停止生成」绝不能触发降级重跑：
// 防止未来有人调整判定顺序，把 stop 变成「停止后又悄悄重开一个新会话」。
// 断言三件事：cancelled 终态、ResumeRejected=false、restart 从未被调用（restart 是唯一
// 会拉起新 CLI 进程的路径，调用数为 0 即等价于「没有新进程被拉起」）。
func TestStreamStopCancelsWithoutDowngrade(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	descriptor, _ := NewRegistry().Get(protocol.AgentTypeClaude)
	plan := BuildResume(descriptor, "ses-existing")

	// stream-hang：先输出一条 chunk，然后挂起 10 分钟，模拟「生成中」。
	process := newHelperProcess(t, ctx, "stream-hang", descriptor, nil, plan.Args)
	process.WritePrompt("继续上一轮")

	var restartCalls atomic.Int32
	originalRestart := process.restart
	if originalRestart == nil {
		t.Fatal("测试前提：Process.restart 应已由 Launcher 注入")
	}
	process.restart = func(restartCtx context.Context, resumeID string) (*Process, error) {
		restartCalls.Add(1)
		return originalRestart(restartCtx, resumeID)
	}

	session, err := NewStreamParser().Parse(ctx, process, descriptor, plan)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}

	// 等「半截输出」已投递，再模拟 chat.stop（cancel + KillTree）。
	produced := false
	firstChunkDeadline := time.After(10 * time.Second)
	for !produced {
		select {
		case message, ok := <-session.Messages:
			if !ok {
				t.Fatal("半截输出到达前 Session 已结束")
			}
			if strings.Contains(message.Content, "已产出") {
				produced = true
			}
		case <-firstChunkDeadline:
			t.Fatal("等待首个 chunk 超时")
		}
	}
	cancel()
	process.KillTree()

	// 排空剩余消息与终态。
	var result Result
	drainDeadline := time.After(10 * time.Second)
drain:
	for {
		select {
		case _, ok := <-session.Messages:
			if !ok {
				result = <-session.Result
				break drain
			}
		case <-drainDeadline:
			t.Fatal("停止后 Session 未收尾")
		}
	}

	if result.Status != ResultCancelled {
		t.Errorf("停止生成应映射 cancelled，实际 %s (%s)", result.Status, result.Error)
	}
	if result.ResumeRejected {
		t.Error("停止生成不得降级新会话（ResumeRejected 必须为 false）")
	}
	if calls := restartCalls.Load(); calls != 0 {
		t.Errorf("停止生成后不得重跑 CLI（restart 被调用 %d 次，说明悄悄重开了新进程）", calls)
	}
}
