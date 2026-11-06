package adapter

// 回归用例（D-11）：turn_timeout_s 必须真正生效（K8 / 5005 / §6.5）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/pkg/protocol"
)

// TestSessionManagerTurnTimeout CLI 挂死时单轮硬超时必须：
//   - 返回 ResultTimeout（不是 cancelled，也不是 failed）；
//   - 下发 chat.error{5005} + chat.done{interrupted}；
//   - 不被误判为「恢复被拒」（ResumeRejected=false）；
//   - 整树回收（父子进程都停写心跳文件，无孤儿）；
//   - 释放单活跃 turn 槽位，同一 agent 可再次发送。
func TestSessionManagerTurnTimeout(t *testing.T) {
	tickFile := filepath.Join(t.TempDir(), "tick.log")
	t.Setenv(helperEnvFlag, "1")
	t.Setenv(helperEnvMode, "hang-with-child")
	t.Setenv(helperEnvTickFile, tickFile)

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 最简描述符：把测试二进制本身当假 CLI，argv 里只带 -test.run 与分隔符。
	registry := NewRegistry()
	registry.descriptors[protocol.AgentTypeClaude] = Descriptor{
		Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
		Family: FamilyStream, DefaultCommand: "claude",
		LaunchArgs: []string{"-test.run=TestHelperProcess", "--"},
	}
	discoverer := &Discoverer{
		cfg:      &config.AdapterConfig{AgentPaths: map[string]string{}},
		registry: registry,
		probe:    NewVersionProbe(),
		cache:    newShellCache(),
		agents: map[string]*discoveredState{
			protocol.AgentTypeClaude: {agent: DiscoveredAgent{
				Type: protocol.AgentTypeClaude, ExecPath: os.Args[0],
				Status: protocol.AgentStatusAvailable,
			}},
		},
	}
	backend := NewBackend(registry, NewLauncher(registry))
	sender := &recordingSender{}
	// 600ms 硬超时：足够进程启动并输出 chunk，又能在测试内快速触发。
	manager := NewSessionManager(rootCtx, backend, sender, discoverer, registry,
		600*time.Millisecond, t.TempDir())

	payload := protocol.ChatSendPayload{
		AgentID: "dev_timeout:claude", Content: "会挂起的请求",
		SessionID: "ses_timeout", AssistantSeq: 2,
	}
	manager.OnChatSend(payload)

	// ① 超时后应收到 chat.done{interrupted}。
	waitFor(t, 20*time.Second, func() bool {
		_, ok := sender.find(protocol.FrameChatDone)
		return ok
	}, "超时后应收到 chat.done")

	doneFrame, _ := sender.find(protocol.FrameChatDone)
	var done protocol.ChatDonePayload
	if err := doneFrame.DecodePayload(&done); err != nil {
		t.Fatalf("解析 chat.done 失败: %v", err)
	}
	if done.Status != protocol.DoneStatusInterrupted {
		t.Errorf("超时应映射 chat.done{interrupted}（§7.3 无 timeout 枚举），实际 %s", done.Status)
	}
	if done.ResumeRejected {
		t.Error("超时不得被误判为「恢复被拒」")
	}

	// ② 必须先下发 chat.error{5005}。
	errorFrame, ok := sender.find(protocol.FrameChatError)
	if !ok {
		t.Fatal("超时应下发 chat.error{5005}")
	}
	var chatErr protocol.ChatErrorPayload
	if err := errorFrame.DecodePayload(&chatErr); err != nil {
		t.Fatalf("解析 chat.error 失败: %v", err)
	}
	if chatErr.Code != protocol.CodeGenerationTimeout {
		t.Errorf("超时错误码应为 5005，实际 %d", chatErr.Code)
	}

	// ③ 槽位释放：可再次发送（不出现 4003）。
	waitFor(t, 10*time.Second, func() bool { return manager.ActiveCount() == 0 }, "超时后槽位应释放")
	manager.OnChatSend(payload)
	waitFor(t, 5*time.Second, func() bool { return manager.ActiveCount() == 1 }, "同一 agent 超时后应可再次发送")

	// ④ 无孤儿进程：父假 CLI 与其子进程都停止写心跳文件。
	waitFor(t, 10*time.Second, func() bool {
		info, err := os.Stat(tickFile)
		return err == nil && info.Size() > 0
	}, "子进程应已开始写心跳文件")
	time.Sleep(600 * time.Millisecond)
	sizeAfterTimeout := fileSize(t, tickFile)
	time.Sleep(600 * time.Millisecond)
	sizeLater := fileSize(t, tickFile)
	if sizeLater != sizeAfterTimeout {
		t.Errorf("超时后仍有孤儿进程写心跳文件: %d -> %d 字节", sizeAfterTimeout, sizeLater)
	}

	// 收尾：取消 rootCtx（第二次 turn 仍在跑）并等待槽位清空。
	cancel()
	waitFor(t, 10*time.Second, func() bool { return manager.ActiveCount() == 0 }, "退出时活跃 turn 应清空")
}
