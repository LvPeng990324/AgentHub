package adapter

// QA 补充测试：聚焦 §4.3 高风险区中的适配器部分——
// NDJSON scanner 边界、UTF-8 切分、prompt 绝不进 argv、会话恢复被拒的降级契约、
// 版本探测失败不影响发现、PATH 覆盖失效后的回退。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/pkg/protocol"
)

// TestQAScannerEdgeCases 覆盖 NDJSON scanner 的行尾边界：
// 无换行结尾、CRLF、单独 \r、空行、以及「一行内混入 \r」时只去尾部 CR。
func TestQAScannerEdgeCases(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"无换行结尾", "line1\nline2", []string{"line1", "line2"}},
		{"CRLF", "line1\r\nline2\r\n", []string{"line1", "line2"}},
		{"单独CR只去尾部", "a\rb\r\nc\r", []string{"a\rb", "c"}},
		{"空行保留为空 Token", "a\n\nb\n", []string{"a", "", "b"}},
		{"末尾空行不产生额外 Token", "a\nb\n", []string{"a", "b"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scanner := newAgentStreamScanner(strings.NewReader(testCase.input))
			got := make([]string, 0, len(testCase.want))
			for scanner.Scan() {
				got = append(got, trimTrailingCR(scanner.Text()))
			}
			if err := scanner.Err(); err != nil {
				t.Fatalf("扫描出错: %v", err)
			}
			if len(got) != len(testCase.want) {
				t.Fatalf("期望 %d 个 Token %q，实际 %d 个 %q", len(testCase.want), testCase.want, len(got), got)
			}
			for index := range testCase.want {
				if got[index] != testCase.want[index] {
					t.Errorf("第 %d 个 Token 期望 %q，实际 %q", index, testCase.want[index], got[index])
				}
			}
		})
	}

	// 单行上限 32MiB：33MiB 必须报错且不 panic（已有用例覆盖 20/33MiB，这里补"恰好边界"语义）。
	oversized := newAgentStreamScanner(strings.NewReader(strings.Repeat("z", scannerMaxTokenSize+1)))
	if oversized.Scan() {
		t.Error("超过 32MiB 上限的行必须被拒绝")
	}
	if oversized.Err() == nil {
		t.Error("超限时必须返回 scanner 错误")
	}
}

// TestQASplitTextUTF8Boundary 大段文本切片必须按 rune 切，不能切坏 UTF-8（否则前端乱码）。
func TestQASplitTextUTF8Boundary(t *testing.T) {
	text := strings.Repeat("中", 2500) + "😀tail"
	parts := splitText(text, maxChunkRunes)
	if len(parts) != 3 {
		t.Fatalf("2504 个字符按 1024 切片应得 3 片，实际 %d 片", len(parts))
	}
	if got := strings.Join(parts, ""); got != text {
		t.Error("切片后拼接结果与原文不一致（切坏了字符）")
	}
	for index, part := range parts {
		if !utf8.ValidString(part) {
			t.Errorf("第 %d 片不是合法 UTF-8: %q", index, part)
		}
		if runeCount := len([]rune(part)); runeCount > maxChunkRunes {
			t.Errorf("第 %d 片超过 %d 个字符（实际 %d）", index, maxChunkRunes, runeCount)
		}
	}
	// 空文本不产生任何片段（避免下发空 chat.chunk）。
	if parts := splitText("", maxChunkRunes); len(parts) != 0 {
		t.Errorf("空文本不应产生片段，实际 %d 片", len(parts))
	}
	// 恰好等于上限时只有一片。
	if parts := splitText(strings.Repeat("a", maxChunkRunes), maxChunkRunes); len(parts) != 1 {
		t.Errorf("恰好等于上限时应只有 1 片，实际 %d 片", len(parts))
	}
}

// TestQAPromptNeverInArgv prompt 必须走 stdin，绝不进 argv（Windows .cmd 重分词 / Linux E2BIG）。
func TestQAPromptNeverInArgv(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv.txt")
	prompt := "QA-PROMPT-SHOULD-NOT-APPEAR-IN-ARGV 帮我看下构建报错"
	t.Setenv(helperEnvFlag, "1")
	t.Setenv(helperEnvMode, "argv-dump")
	t.Setenv("AGENTHUB_ARGV_FILE", argvFile)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 用最简描述符，让 argv 只包含 CustomArgs，避免真实 CLI 参数干扰测试二进制的 flag 解析。
	descriptor := Descriptor{
		Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
		Family: FamilyStream, DefaultCommand: "claude",
	}
	launcher := NewLauncher(NewRegistry())
	process, err := launcher.Start(ctx, descriptor, ExecOptions{
		ExecPath:   os.Args[0],
		CustomArgs: []string{"-test.run=TestHelperProcess", "--"},
	}, BuildResume(descriptor, ""))
	if err != nil {
		t.Fatalf("Launcher.Start 失败: %v", err)
	}
	process.WritePrompt(prompt)
	// 注意：必须等子进程把 argv 落盘后再 Finish（Finish 会立即回收整个进程树）。
	waitForFile(t, argvFile, 10*time.Second)
	process.Finish()

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("读取 argv 落盘文件失败: %v", err)
	}
	argv := string(raw)
	if argv == "" {
		t.Fatal("argv 落盘为空，测试自身失效")
	}
	if strings.Contains(argv, "QA-PROMPT-SHOULD-NOT-APPEAR-IN-ARGV") {
		t.Errorf("prompt 出现在 argv 中（必须只走 stdin）: %s", argv)
	}
	if !strings.Contains(argv, "-test.run=TestHelperProcess") {
		t.Errorf("argv 落盘内容异常: %s", argv)
	}
}

// waitForFile 等待文件出现且非空。
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("等待文件 %s 超时", path)
}

// TestQAStreamResumeRejectedMustDowngrade 会话恢复被拒的降级契约（§8.6 / K12 / U4 / R-18）：
// CLI 明确拒绝已失效的 resume_id 时，必须降级为新会话并置 ResumeRejected=true，
// 否则平台会把死的 resume_id 一直带下去、每轮都失败（无限重试死 id）。
func TestQAStreamResumeRejectedMustDowngrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	descriptor, _ := NewRegistry().Get(protocol.AgentTypeClaude)
	plan := BuildResume(descriptor, "dead-session")
	if len(plan.Args) != 2 || plan.Args[1] != "dead-session" {
		t.Fatalf("续接计划异常: %v", plan.Args)
	}

	process := newHelperProcess(t, ctx, "stream-resume-reject", descriptor, nil, plan.Args)
	process.WritePrompt("继续上一轮")
	session, err := NewStreamParser().Parse(ctx, process, descriptor, plan)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	_, result := collectSession(t, session, 15*time.Second)

	if !result.ResumeRejected {
		t.Errorf("会话恢复被拒时必须置 ResumeRejected=true（前端据此提示「已开启新会话」），实际 false（status=%s err=%q）",
			result.Status, result.Error)
	}
	if result.Status != ResultCompleted {
		t.Errorf("恢复被拒应降级为新会话并完成本轮，实际 status=%s err=%q", result.Status, result.Error)
	}
	if result.SessionID == "dead-session" {
		t.Error("不得把已失效的 resume_id 回带上行")
	}
}

// TestQAVersionProbeFailureStillDiscovered 版本探测失败必须填 unknown，且不影响该 agent 被发现（U8）。
func TestQAVersionProbeFailureStillDiscovered(t *testing.T) {
	dir := t.TempDir()
	// 造一个"存在但不是可执行 CLI"的文件：exec --version 必然失败。
	stub := filepath.Join(dir, "not-a-real-cli")
	if err := os.WriteFile(stub, []byte("这不是可执行文件"), 0o644); err != nil {
		t.Fatalf("写入假文件失败: %v", err)
	}
	cfg := &config.AdapterConfig{
		AgentPaths:   map[string]string{protocol.AgentTypeClaude: stub},
		DeviceIDPath: filepath.Join(dir, "device_id"),
	}
	discoverer := NewDiscoverer(cfg, NewRegistry())
	snapshot := discoverer.Probe(context.Background())

	var found *DiscoveredAgent
	for index := range snapshot {
		if snapshot[index].Type == protocol.AgentTypeClaude {
			found = &snapshot[index]
		}
	}
	if found == nil {
		t.Fatal("版本探测失败不应影响 agent 被发现（U8）")
	}
	if found.Version != unknownVersion {
		t.Errorf("版本探测失败应填 %q，实际 %q", unknownVersion, found.Version)
	}
	if found.ExecPath != stub {
		t.Errorf("exec_path 应为配置覆盖路径 %q，实际 %q", stub, found.ExecPath)
	}
	if found.Status != protocol.AgentStatusAvailable {
		t.Errorf("探测成功时 status 应为 available，实际 %q", found.Status)
	}
}

// TestQAPathOverrideMissingFallsBack 配置的 agent_paths 指向不存在的文件时必须回退到后续策略，
// 而不是把不存在的路径当作已发现（否则会注册一个永远启动失败的 agent）。
func TestQAPathOverrideMissingFallsBack(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.AdapterConfig{
		AgentPaths:   map[string]string{protocol.AgentTypeClaude: filepath.Join(dir, "ghost-cli")},
		DeviceIDPath: filepath.Join(dir, "device_id"),
	}
	discoverer := NewDiscoverer(cfg, NewRegistry())
	descriptor := Descriptor{
		Type: protocol.AgentTypeClaude, DefaultCommand: "agenthub-qa-definitely-not-installed",
		Family: FamilyStream,
	}
	path, ok := discoverer.ProbeOne(context.Background(), descriptor)
	if ok {
		t.Errorf("不存在的 PATH 覆盖不得被当作命中，实际返回 %q", path)
	}
}
