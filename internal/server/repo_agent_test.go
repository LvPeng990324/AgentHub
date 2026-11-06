package server

import (
	"context"
	"testing"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// TestUpsertSnapshotPreservesRegisteredAndRemark 校验全量快照幂等且绝不覆盖用户态（K11 / §7.5）。
func TestUpsertSnapshotPreservesRegisteredAndRemark(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	insertDeviceOnly(t, store, "dev_snap")
	repo := NewAgentRepo(store)

	first := []protocol.AgentSnapshot{{
		Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
		Version: "1.0.0", ExecPath: "/bin/claude", Status: protocol.AgentStatusAvailable,
	}}
	if err := repo.UpsertSnapshot(ctx, "dev_snap", first); err != nil {
		t.Fatalf("首次快照失败: %v", err)
	}
	agent, err := repo.GetByID(ctx, "dev_snap:claude")
	if err != nil {
		t.Fatalf("读取 agent 失败: %v", err)
	}
	if agent.ID != "dev_snap:claude" {
		t.Errorf("稳定键应为 <device_id>:<type>，实际 %s", agent.ID)
	}
	if agent.Registered {
		t.Error("新发现的 agent 默认不应已注册")
	}

	// 模拟用户注册 + 填写备注。
	if _, err := store.DB.ExecContext(ctx,
		`UPDATE agents SET registered = 1, remark = '主力' WHERE id = ?`, "dev_snap:claude"); err != nil {
		t.Fatalf("设置用户态失败: %v", err)
	}

	// 第二次快照：同类型展示信息变化，并新增一个 codex。
	second := []protocol.AgentSnapshot{
		{Type: protocol.AgentTypeClaude, DisplayName: "Claude Code 2", Version: "2.0.0", ExecPath: "/usr/bin/claude", Status: protocol.AgentStatusAvailable},
		{Type: protocol.AgentTypeCodex, DisplayName: "Codex", Version: "0.5.0", ExecPath: "/usr/bin/codex", Status: protocol.AgentStatusAvailable},
	}
	if err := repo.UpsertSnapshot(ctx, "dev_snap", second); err != nil {
		t.Fatalf("第二次快照失败: %v", err)
	}
	agent, err = repo.GetByID(ctx, "dev_snap:claude")
	if err != nil {
		t.Fatalf("读取 agent 失败: %v", err)
	}
	if !agent.Registered {
		t.Error("UpsertSnapshot 覆盖了 registered，违反三态独立")
	}
	if agent.Remark != "主力" {
		t.Errorf("UpsertSnapshot 覆盖了 remark，实际 %q", agent.Remark)
	}
	if agent.Name != protocol.AgentTypeClaude {
		t.Errorf("UpsertSnapshot 不应改写 name，实际 %q", agent.Name)
	}
	if agent.Version != "2.0.0" || agent.ExecPath != "/usr/bin/claude" {
		t.Errorf("快照的只读展示字段应更新: version=%s exec_path=%s", agent.Version, agent.ExecPath)
	}
	if agent.DisplayName != "Claude Code 2" {
		t.Errorf("display_name 应更新，实际 %q", agent.DisplayName)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM agents WHERE device_id = ?`, "dev_snap"); got != 2 {
		t.Fatalf("快照后应有 2 个 agent，实际 %d", got)
	}
	codex, err := repo.GetByID(ctx, "dev_snap:codex")
	if err != nil {
		t.Fatalf("新增的 codex 未落库: %v", err)
	}
	if codex.Registered {
		t.Error("新发现的 codex 不应已注册")
	}

	// 第三次快照缺失 claude：只置离线、不删行，registered / remark 保持不动。
	third := []protocol.AgentSnapshot{{
		Type: protocol.AgentTypeCodex, DisplayName: "Codex", Version: "0.5.0",
		ExecPath: "/usr/bin/codex", Status: protocol.AgentStatusAvailable,
	}}
	if err := repo.UpsertSnapshot(ctx, "dev_snap", third); err != nil {
		t.Fatalf("第三次快照失败: %v", err)
	}
	agent, err = repo.GetByID(ctx, "dev_snap:claude")
	if err != nil {
		t.Fatalf("快照缺失的 agent 不应被删行: %v", err)
	}
	if agent.Online {
		t.Error("快照缺失的 agent 应仅置 offline")
	}
	if !agent.Registered || agent.Remark != "主力" {
		t.Error("快照缺失时不得改动 registered / remark")
	}
}

// TestPurgeByAgentCascades 校验 PurgeByAgent 靠外键级联清空 messages（E11 第二步）。
func TestPurgeByAgentCascades(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	insertFixture(t, store, "dev_purge", "dev_purge:claude", protocol.AgentTypeClaude, 3)
	repo := NewMessageRepo(store)

	affected, err := repo.PurgeByAgent(ctx, "dev_purge:claude")
	if err != nil {
		t.Fatalf("PurgeByAgent 失败: %v", err)
	}
	if affected != 1 {
		t.Errorf("应删除 1 条会话，实际 %d", affected)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM sessions`); got != 0 {
		t.Errorf("sessions 应清空，实际 %d", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM messages`); got != 0 {
		t.Errorf("messages 应被级联清空，实际 %d", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM agents`); got != 1 {
		t.Errorf("PurgeByAgent 不得删除 agents 行，实际 %d", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM devices`); got != 1 {
		t.Errorf("PurgeByAgent 不得删除 devices 行，实际 %d", got)
	}
	var remark string
	if err := store.DB.QueryRowContext(ctx,
		`SELECT remark FROM agents WHERE id = ?`, "dev_purge:claude").Scan(&remark); err != nil {
		t.Fatalf("读取备注失败: %v", err)
	}
	if remark != "主力" {
		t.Errorf("PurgeByAgent 不得清空 remark，实际 %q", remark)
	}
}

// TestUnregisterE11 是 PRD R-06 / D-6 铁律的库级验证：
// 单事务内 registered=0 + DELETE sessions（级联 messages），
// 不删设备/agent 行、不清备注、不动 online，且之后可重新注册并新建会话。
func TestUnregisterE11(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	oldSessionID := insertFixture(t, store, "dev_e11", "dev_e11:claude", protocol.AgentTypeClaude, 4)
	repo := NewAgentRepo(store)

	deviceID, err := repo.Unregister(ctx, "dev_e11:claude")
	if err != nil {
		t.Fatalf("Unregister 失败: %v", err)
	}
	if deviceID != "dev_e11" {
		t.Errorf("应返回 device_id=dev_e11，实际 %s", deviceID)
	}

	var registered, online int
	var remark string
	if err := store.DB.QueryRowContext(ctx,
		`SELECT registered, online, remark FROM agents WHERE id = ?`, "dev_e11:claude").
		Scan(&registered, &online, &remark); err != nil {
		t.Fatalf("读取 agent 失败: %v", err)
	}
	if registered != 0 {
		t.Error("registered 应置 0")
	}
	if online != 1 {
		t.Error("取消注册不得推断/改动 online（三态独立）")
	}
	if remark != "主力" {
		t.Errorf("取消注册不得清空 remark，实际 %q", remark)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM devices`); got != 1 {
		t.Errorf("不得删除 devices 行，实际 %d", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM agents`); got != 1 {
		t.Errorf("不得删除 agents 行，实际 %d", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM sessions`); got != 0 {
		t.Errorf("sessions 应清空，实际 %d", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM messages`); got != 0 {
		t.Errorf("messages 应被级联清空，实际 %d", got)
	}

	// 之后可重新注册，EnsureSession 必须新建一条会话（历史为空）。
	agent, _, err := repo.RegisterOne(ctx, "dev_e11:claude")
	if err != nil {
		t.Fatalf("重新注册失败: %v", err)
	}
	if !agent.Registered {
		t.Error("重新注册后 registered 应为 true")
	}
	if agent.SessionID == "" || agent.SessionID == oldSessionID {
		t.Errorf("重新注册应新建会话，实际 session_id=%q（旧值 %q）", agent.SessionID, oldSessionID)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM messages`); got != 0 {
		t.Errorf("重新注册后历史消息应为空，实际 %d", got)
	}
}

// TestUnregisterMissingAgent 校验不存在的 agent 返回 ErrAgentNotFound（映射 3001）。
func TestUnregisterMissingAgent(t *testing.T) {
	store := newTestStore(t)
	_, err := NewAgentRepo(store).Unregister(context.Background(), "missing:claude")
	if err != ErrAgentNotFound {
		t.Fatalf("期望 ErrAgentNotFound，实际 %v", err)
	}
}
