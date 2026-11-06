package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/pkg/protocol"
)

// ---------------- VersionProbe ----------------

func TestVersionProbe(t *testing.T) {
	ok := &VersionProbe{
		Timeout:     time.Second,
		OutputLimit: 200,
		Runner: func(ctx context.Context, execPath string) ([]byte, error) {
			return []byte("v1.2.3 fake-cli\nsecond line\n"), nil
		},
	}
	if got := ok.Run(context.Background(), "stub"); got != "v1.2.3 fake-cli" {
		t.Errorf("应取首行版本，实际 %q", got)
	}
	failed := &VersionProbe{Runner: func(context.Context, string) ([]byte, error) {
		return nil, errors.New("exec failed")
	}}
	if got := failed.Run(context.Background(), "stub"); got != "unknown" {
		t.Errorf("失败应填 unknown，实际 %q", got)
	}
	timedOut := &VersionProbe{
		Timeout: 40 * time.Millisecond,
		Runner: func(ctx context.Context, _ string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	if got := timedOut.Run(context.Background(), "stub"); got != "unknown" {
		t.Errorf("超时应填 unknown，实际 %q", got)
	}
	limited := &VersionProbe{
		Timeout:     time.Second,
		OutputLimit: 10,
		Runner: func(context.Context, string) ([]byte, error) {
			return []byte(strings.Repeat("a", 100)), nil
		},
	}
	if got := limited.Run(context.Background(), "stub"); len(got) != 10 {
		t.Errorf("输出应截断到 200B(本例 10B)，实际长度 %d", len(got))
	}
}

// ---------------- Discoverer 四级探测 ----------------

func TestDiscovererFourLevelProbe(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "path-override-cli")
	if err := os.WriteFile(stub, []byte("stub"), 0o755); err != nil {
		t.Fatalf("写入假 CLI 失败: %v", err)
	}
	cfg := &config.AdapterConfig{AgentPaths: map[string]string{}, DeviceIDPath: filepath.Join(dir, "device_id")}
	discoverer := NewDiscoverer(cfg, NewRegistry())
	ctx := context.Background()
	descriptor := Descriptor{
		Type: "claude", DefaultCommand: "agenthub-no-such-cli-xyz",
		PathEnvKey: "AGENTHUB_TEST_DISCOVER_PATH", Family: FamilyStream,
	}

	// ① 配置覆盖
	cfg.AgentPaths[descriptor.Type] = stub
	if path, ok := discoverer.ProbeOne(ctx, descriptor); !ok || path != stub {
		t.Errorf("配置覆盖未命中: %q %v", path, ok)
	}
	delete(cfg.AgentPaths, descriptor.Type)

	// ① 环境变量覆盖
	t.Setenv(descriptor.PathEnvKey, stub)
	if path, ok := discoverer.ProbeOne(ctx, descriptor); !ok || path != stub {
		t.Errorf("环境变量覆盖未命中: %q %v", path, ok)
	}
	t.Setenv(descriptor.PathEnvKey, "")

	// ② exec.LookPath（Windows 靠 PATHEXT，Unix 靠可执行位）
	command := "agenthubfakecli"
	fileName := command
	if runtime.GOOS == "windows" {
		fileName += ".exe"
	}
	onPath := filepath.Join(dir, fileName)
	if err := os.WriteFile(onPath, []byte("stub"), 0o755); err != nil {
		t.Fatalf("写入 PATH 内假 CLI 失败: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	descriptor.DefaultCommand = command
	if path, ok := discoverer.ProbeOne(ctx, descriptor); !ok || path != onPath {
		t.Errorf("LookPath 未命中: %q %v", path, ok)
	}

	// ④ ExtraProbePaths
	descriptor.DefaultCommand = "another-agenthub-no-such-cli"
	descriptor.ExtraProbePaths = []string{stub}
	t.Setenv("PATH", dir)
	if path, ok := discoverer.ProbeOne(ctx, descriptor); !ok || path != stub {
		t.Errorf("ExtraProbePaths 未命中: %q %v", path, ok)
	}
}

// TestDiscovererMonotonic 校验单向只增不减：单次/双次缺失保留，连续 3 次才从快照移除。
func TestDiscovererMonotonic(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("SHELL", "")
	discoverer := &Discoverer{
		cfg:      &config.AdapterConfig{AgentPaths: map[string]string{}},
		registry: NewRegistry(),
		probe:    NewVersionProbe(),
		cache:    newShellCache(),
		agents: map[string]*discoveredState{
			protocol.AgentTypeClaude: {
				agent: DiscoveredAgent{
					Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
					Version: "1.0.0", ExecPath: "stub", Status: protocol.AgentStatusAvailable,
				},
			},
		},
	}
	for round := 1; round <= 2; round++ {
		snapshot := discoverer.Probe(context.Background())
		if len(snapshot) != 1 {
			t.Fatalf("第 %d 次缺失不应删除已上报 agent，实际 %d 条", round, len(snapshot))
		}
		if snapshot[0].Status != protocol.AgentStatusUnavailable {
			t.Errorf("缺失周期应标记 unavailable，实际 %s", snapshot[0].Status)
		}
	}
	snapshot := discoverer.Probe(context.Background())
	if len(snapshot) != 0 {
		t.Errorf("连续 3 个周期缺失后应从快照移除（平台据此置 online=0），实际 %d 条", len(snapshot))
	}
}

// TestShellCacheFingerprint 校验登录 shell 缓存 TTL 与 PATH|SHELL|HOME 指纹失效。
func TestShellCacheFingerprint(t *testing.T) {
	cache := newShellCache()
	fingerprint := shellFingerprint()
	cache.put(fingerprint, "claude", "/usr/bin/claude")
	if got, ok := cache.get(fingerprint, "claude"); !ok || got != "/usr/bin/claude" {
		t.Errorf("同指纹应命中缓存: %q %v", got, ok)
	}
	if _, ok := cache.get(fingerprint+"|changed", "claude"); ok {
		t.Error("指纹变化应整体失效")
	}
	cache.put(fingerprint, "claude", "/usr/bin/claude")
	cache.mu.Lock()
	entry := cache.entries["claude"]
	entry.cachedAt = time.Now().Add(-shellCacheTTL - time.Minute)
	cache.entries["claude"] = entry
	cache.mu.Unlock()
	if _, ok := cache.get(fingerprint, "claude"); ok {
		t.Error("超过 30min TTL 应失效")
	}
}

// ---------------- SessionManager ----------------

type recordingSender struct {
	mu     sync.Mutex
	frames []protocol.Message
}

func (s *recordingSender) Send(frameType string, payload any) bool {
	message, err := protocol.NewMessage(frameType, 1, payload)
	if err != nil {
		return false
	}
	s.mu.Lock()
	s.frames = append(s.frames, message)
	s.mu.Unlock()
	return true
}

func (s *recordingSender) snapshot() []protocol.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.Message, len(s.frames))
	copy(out, s.frames)
	return out
}

func (s *recordingSender) find(frameType string) (protocol.Message, bool) {
	for _, frame := range s.snapshot() {
		if frame.Type == frameType {
			return frame, true
		}
	}
	return protocol.Message{}, false
}

type blockingBackend struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (b *blockingBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	messages := make(chan Message)
	close(messages)
	results := make(chan Result, 1)
	go func() {
		select {
		case b.started <- struct{}{}:
		case <-ctx.Done():
		}
		select {
		case <-b.release:
			results <- Result{Status: ResultCompleted, SessionID: "ses-new"}
		case <-ctx.Done():
			results <- Result{Status: ResultCancelled}
		}
		close(results)
	}()
	return &Session{Messages: messages, Result: results}, nil
}

func (b *blockingBackend) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func newTestSessionManager(rootCtx context.Context, backend Backend, sender FrameSender) *SessionManager {
	return newTestSessionManagerWithTimeout(rootCtx, backend, sender, 0)
}

func newTestSessionManagerWithTimeout(
	rootCtx context.Context,
	backend Backend,
	sender FrameSender,
	turnTimeout time.Duration,
) *SessionManager {
	registry := NewRegistry()
	discoverer := &Discoverer{
		cfg:      &config.AdapterConfig{AgentPaths: map[string]string{}},
		registry: registry,
		probe:    NewVersionProbe(),
		cache:    newShellCache(),
		agents: map[string]*discoveredState{
			protocol.AgentTypeClaude: {
				agent: DiscoveredAgent{
					Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
					ExecPath: "stub", Status: protocol.AgentStatusAvailable,
				},
			},
		},
	}
	workingDir, err := os.Getwd()
	if err != nil {
		workingDir = ""
	}
	return NewSessionManager(rootCtx, backend, sender, discoverer, registry, turnTimeout, workingDir)
}

func testSendPayload() protocol.ChatSendPayload {
	return protocol.ChatSendPayload{
		AgentID: "dev_test:claude", Content: "你好",
		SessionID: "ses_test", UserSeq: 1, AssistantSeq: 2, ResumeID: "ses-old",
	}
}

// TestSessionManagerSingleActiveTurn 校验同一 agent 同时仅 1 个活跃 turn（K9 / U1）。
func TestSessionManagerSingleActiveTurn(t *testing.T) {
	backend := &blockingBackend{started: make(chan struct{}, 4), release: make(chan struct{})}
	sender := &recordingSender{}
	manager := newTestSessionManager(context.Background(), backend, sender)
	payload := testSendPayload()

	manager.OnChatSend(payload)
	select {
	case <-backend.started:
	case <-time.After(3 * time.Second):
		t.Fatal("首个 turn 未启动")
	}
	if manager.ActiveCount() != 1 {
		t.Fatalf("应有 1 个活跃 turn，实际 %d", manager.ActiveCount())
	}
	manager.OnChatSend(payload) // 并发第二次：不排队、不插话
	errorFrame, ok := sender.find(protocol.FrameChatError)
	if !ok {
		t.Fatal("并发发送应回 chat.error")
	}
	var chatError protocol.ChatErrorPayload
	if err := errorFrame.DecodePayload(&chatError); err != nil {
		t.Fatalf("解析 chat.error 失败: %v", err)
	}
	if chatError.Code != protocol.CodeGenerationInProgress {
		t.Errorf("应返回 4003，实际 %d", chatError.Code)
	}
	if backend.callCount() != 1 {
		t.Errorf("第二次发送不应启动新 CLI，实际调用 %d 次", backend.callCount())
	}

	close(backend.release)
	waitFor(t, 5*time.Second, func() bool { return manager.ActiveCount() == 0 }, "活跃 turn 未释放")
	doneFrame, ok := sender.find(protocol.FrameChatDone)
	if !ok {
		t.Fatal("turn 结束应发 chat.done")
	}
	var done protocol.ChatDonePayload
	if err := doneFrame.DecodePayload(&done); err != nil {
		t.Fatalf("解析 chat.done 失败: %v", err)
	}
	if done.Status != protocol.DoneStatusCompleted || done.ResumeID != "ses-new" {
		t.Errorf("chat.done 内容异常: %+v", done)
	}
	if done.AssistantSeq != payload.AssistantSeq || done.SessionID != payload.SessionID {
		t.Error("chat.done 必须回带平台下发的 session_id / assistant_seq")
	}
}

// TestSessionManagerStopAndUnregistered 校验 stop 保留内容、unregistered 只清内存不回 done。
func TestSessionManagerStopAndUnregistered(t *testing.T) {
	backend := &blockingBackend{started: make(chan struct{}, 4), release: make(chan struct{})}
	sender := &recordingSender{}
	manager := newTestSessionManager(context.Background(), backend, sender)
	payload := testSendPayload()
	manager.OnChatSend(payload)
	<-backend.started
	manager.OnChatStop(protocol.ChatStopPayload{AgentID: payload.AgentID, SessionID: payload.SessionID})
	waitFor(t, 5*time.Second, func() bool {
		_, ok := sender.find(protocol.FrameChatDone)
		return ok
	}, "停止后应收到 chat.done")
	doneFrame, _ := sender.find(protocol.FrameChatDone)
	var done protocol.ChatDonePayload
	if err := json.Unmarshal(doneFrame.Payload, &done); err != nil {
		t.Fatalf("解析 done 失败: %v", err)
	}
	if done.Status != protocol.DoneStatusStopped {
		t.Errorf("停止应映射 stopped，实际 %s", done.Status)
	}

	// F12 agent.unregistered：只 cancel + 清内存，不回 chat.done（由平台 interrupted 兜底）。
	backend2 := &blockingBackend{started: make(chan struct{}, 4), release: make(chan struct{})}
	sender2 := &recordingSender{}
	manager2 := newTestSessionManager(context.Background(), backend2, sender2)
	manager2.OnChatSend(payload)
	<-backend2.started
	manager2.OnUnregistered(payload.AgentID)
	waitFor(t, 5*time.Second, func() bool { return manager2.ActiveCount() == 0 }, "unregistered 应清掉活跃 turn")
	if _, ok := sender2.find(protocol.FrameChatDone); ok {
		t.Error("F12 不应由适配器回 chat.done（平台侧 interrupted 兜底）")
	}
}

// failingBackend 总是返回失败终态，用于验证 chat.done 是否回带 error 详情。
type failingBackend struct {
	errText string
}

func (b *failingBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	messages := make(chan Message)
	close(messages)
	results := make(chan Result, 1)
	results <- Result{Status: ResultFailed, Error: b.errText}
	close(results)
	return &Session{Messages: messages, Result: results}, nil
}

// TestChatDoneCarriesErrorDetail 失败时 chat.done 必须回带 CLI 原始错误文本（前端折叠展示），
// 且超长文本要被截断（保护 WS 帧），同时 chat.error 仍发固定中文主文案。
func TestChatDoneCarriesErrorDetail(t *testing.T) {
	longError := strings.Repeat("Invalid params: mcpServers Required value is missing; ", 80)
	backend := &failingBackend{errText: longError}
	sender := &recordingSender{}
	manager := newTestSessionManager(context.Background(), backend, sender)

	manager.OnChatSend(testSendPayload())
	waitFor(t, 5*time.Second, func() bool {
		_, ok := sender.find(protocol.FrameChatDone)
		return ok
	}, "失败后应收到 chat.done")

	doneFrame, _ := sender.find(protocol.FrameChatDone)
	var done protocol.ChatDonePayload
	if err := doneFrame.DecodePayload(&done); err != nil {
		t.Fatalf("解析 chat.done 失败: %v", err)
	}
	if done.Status != protocol.DoneStatusFailed {
		t.Errorf("应为 failed，实际 %s", done.Status)
	}
	if strings.TrimSpace(done.Error) == "" {
		t.Fatal("失败必须回带 error 详情（否则前端只能显示固定文案，无法自助排查）")
	}
	if runes := []rune(done.Error); len(runes) > maxErrorDetailRunes+1 {
		t.Errorf("error 详情应截断到 %d 个字符，实际 %d", maxErrorDetailRunes, len(runes))
	}
	if !strings.HasSuffix(done.Error, "…") {
		t.Error("截断后应以 … 结尾")
	}

	// 主文案仍是固定中文（前端 Banner），error 只作为详情补充。
	errorFrame, ok := sender.find(protocol.FrameChatError)
	if !ok {
		t.Fatal("失败时应同时下发 chat.error")
	}
	var chatErr protocol.ChatErrorPayload
	_ = errorFrame.DecodePayload(&chatErr)
	if chatErr.Code != protocol.CodeCLIStartFailed {
		t.Errorf("主文案错误码应为 5003，实际 %d", chatErr.Code)
	}
}

// TestAgentTypeFromID 校验 agent_id 尾段解析（含冒号的稳定键）。
func TestAgentTypeFromID(t *testing.T) {
	if got := agentTypeFromID("dev_9f2a1c:claude"); got != "claude" {
		t.Errorf("期望 claude，实际 %q", got)
	}
	if got := agentTypeFromID("dev_x:sub:type"); got != "type" {
		t.Errorf("应取最后一段，实际 %q", got)
	}
	if got := agentTypeFromID("no-colon"); got != "" {
		t.Errorf("无冒号应返回空，实际 %q", got)
	}
}
