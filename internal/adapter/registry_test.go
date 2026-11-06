package adapter

import (
	"strings"
	"testing"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// TestDescriptorsComplete 校验 §8.3 的 9 行描述符表完整且与 /meta 的 agent_types 一致。
func TestDescriptorsComplete(t *testing.T) {
	if len(Descriptors) != 9 {
		t.Fatalf("描述符应有 9 行，实际 %d", len(Descriptors))
	}
	registry := NewRegistry()
	metaTypes := protocol.AgentTypes()
	if len(metaTypes) != 9 {
		t.Fatalf("/meta agent_types 应有 9 项，实际 %d", len(metaTypes))
	}
	for _, item := range metaTypes {
		descriptor, ok := registry.Get(item.Type)
		if !ok {
			t.Errorf("描述符表缺少 %s", item.Type)
			continue
		}
		if descriptor.DisplayName != item.DisplayName || string(descriptor.Family) != item.Family {
			t.Errorf("%s 与 /meta 不一致: %s/%s vs %s/%s",
				item.Type, descriptor.DisplayName, descriptor.Family, item.DisplayName, item.Family)
		}
		if descriptor.DefaultCommand == "" {
			t.Errorf("%s 缺少 default_command", item.Type)
		}
		wantEnvKey := "AGENTHUB_" + strings.ToUpper(item.Type) + "_PATH"
		if descriptor.PathEnvKey != wantEnvKey {
			t.Errorf("%s 的 path_env_key 期望 %s，实际 %s", item.Type, wantEnvKey, descriptor.PathEnvKey)
		}
		if descriptor.Resume == "" {
			t.Errorf("%s 缺少 resume 写法", item.Type)
		}
	}
}

// TestRegistryByFamily 校验 4 个协议族的分组与单一事实来源。
func TestRegistryByFamily(t *testing.T) {
	registry := NewRegistry()
	cases := map[ProtocolFamily]int{
		FamilyStream:    4, // claude / codebuddy / qwen / cursor
		FamilyACP:       2, // hermes / kimi
		FamilyAppServer: 1, // codex
		FamilyJSON:      2, // opencode / openclaw
	}
	total := 0
	for family, want := range cases {
		got := len(registry.ByFamily(family))
		if got != want {
			t.Errorf("协议族 %s 期望 %d 个，实际 %d", family, want, got)
		}
		total += got
	}
	if total != 9 {
		t.Errorf("4 族合计应为 9，实际 %d", total)
	}
	if len(registry.All()) != 9 || len(registry.Types()) != 9 {
		t.Error("All / Types 应返回 9 项")
	}
	if _, ok := registry.Get("not-an-agent"); ok {
		t.Error("未知类型不应命中")
	}
	// 返回值是副本，外部改写不得污染静态表。
	copied := registry.All()
	copied[0].DisplayName = "mutated"
	again, _ := registry.Get(copied[0].Type)
	if again.DisplayName == "mutated" {
		t.Error("Registry 必须返回描述符副本")
	}
}

// TestDescriptorCriticalValues 校验易踩坑的关键取值（§8.3 / 分派 3.2）。
func TestDescriptorCriticalValues(t *testing.T) {
	registry := NewRegistry()
	codex, _ := registry.Get(protocol.AgentTypeCodex)
	if codex.HandshakeMs != 60000 {
		t.Errorf("codex handshake_ms 应为 60000，实际 %d", codex.HandshakeMs)
	}
	for _, agentType := range []string{protocol.AgentTypeHermes, protocol.AgentTypeKimi} {
		descriptor, _ := registry.Get(agentType)
		if descriptor.HandshakeMs != 30000 {
			t.Errorf("%s handshake_ms 应为 30000，实际 %d", agentType, descriptor.HandshakeMs)
		}
		if descriptor.Resume != ResumeSessionRPC {
			t.Errorf("%s 应使用 session/resume", agentType)
		}
	}
	openclaw, _ := registry.Get(protocol.AgentTypeOpenclaw)
	if openclaw.IdleGraceMs != 2000 {
		t.Errorf("openclaw idle_grace_ms 应为 2000，实际 %d", openclaw.IdleGraceMs)
	}
	if !openclaw.WindowsSpecial {
		t.Error("openclaw 应标记 windows_special")
	}
	for _, agentType := range []string{
		protocol.AgentTypeClaude, protocol.AgentTypeCodeBuddy,
		protocol.AgentTypeQwen, protocol.AgentTypeCursor,
	} {
		descriptor, _ := registry.Get(agentType)
		if descriptor.Family != FamilyStream || descriptor.Resume != ResumeArg {
			t.Errorf("%s 应为 S 族 + --resume", agentType)
		}
	}
	codebuddy, _ := registry.Get(protocol.AgentTypeCodeBuddy)
	joined := strings.Join(codebuddy.LaunchArgs, " ")
	for _, tool := range []string{"AskUserQuestion", "EnterPlanMode", "ExitPlanMode"} {
		if !strings.Contains(joined, tool) {
			t.Errorf("codebuddy 应禁用 Plan 三件套，缺少 %s", tool)
		}
	}
}
