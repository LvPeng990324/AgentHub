package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// newTestStore 在空目录（含不存在的父目录）打开一个全新 SQLite 库。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "agenthub.db")
	store, err := OpenStore(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenStore 失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// countRows 执行 COUNT 查询并返回结果，供各断言复用。
func countRows(t *testing.T, store *Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.DB.QueryRowContext(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("countRows 失败 (%s): %v", query, err)
	}
	return count
}

// insertDeviceOnly 只插入 devices 一行（UpsertSnapshot 测试需要设备先存在）。
func insertDeviceOnly(t *testing.T, store *Store, deviceID string) {
	t.Helper()
	now := nowUTC()
	if _, err := store.DB.ExecContext(context.Background(), `
INSERT INTO devices(id, name, os, arch, adapter_version, protocol_version, remark, online, last_seen_at, created_at, updated_at)
VALUES(?, ?, 'windows', 'amd64', '0.1.0', 1, '', 1, ?, ?, ?)`,
		deviceID, "测试设备", now, now, now); err != nil {
		t.Fatalf("insertDeviceOnly 失败: %v", err)
	}
}

// insertFixture 造一条完整的 device → agent → session → messages 数据链，返回 session_id。
func insertFixture(t *testing.T, store *Store, deviceID string, agentID string, agentType string, messageCount int) string {
	t.Helper()
	ctx := context.Background()
	now := nowUTC()
	if _, err := store.DB.ExecContext(ctx, `
INSERT INTO devices(id, name, os, arch, adapter_version, protocol_version, remark, online, last_seen_at, created_at, updated_at)
VALUES(?, '测试设备', 'windows', 'amd64', '0.1.0', 1, '设备备注', 1, ?, ?, ?)`,
		deviceID, now, now, now); err != nil {
		t.Fatalf("插入 device 失败: %v", err)
	}
	if _, err := store.DB.ExecContext(ctx, `
INSERT INTO agents(id, device_id, type, display_name, name, version, exec_path, remark, registered, online, last_seen_at, created_at, updated_at)
VALUES(?, ?, ?, 'Claude Code', ?, '1.0.0', '/bin/claude', '主力', 1, 1, ?, ?, ?)`,
		agentID, deviceID, agentType, agentType, now, now, now); err != nil {
		t.Fatalf("插入 agent 失败: %v", err)
	}
	sessionID := "ses_" + deviceID
	if _, err := store.DB.ExecContext(ctx, `
INSERT INTO sessions(id, agent_id, resume_id, created_at, updated_at) VALUES(?, ?, 'resume-1', ?, ?)`,
		sessionID, agentID, now, now); err != nil {
		t.Fatalf("插入 session 失败: %v", err)
	}
	for seq := 1; seq <= messageCount; seq++ {
		if _, err := store.DB.ExecContext(ctx, `
INSERT INTO messages(session_id, seq, role, content, status, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
			sessionID, seq, protocol.RoleUser, "测试消息", protocol.MessageStatusComplete, now); err != nil {
			t.Fatalf("插入 message 失败: %v", err)
		}
	}
	return sessionID
}

// TestOpenStoreCreatesFiveTables 校验「空目录启动自动建 5 表 + 索引 + user_version=1」。
func TestOpenStoreCreatesFiveTables(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	rows, err := store.DB.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("查询表清单失败: %v", err)
	}
	defer rows.Close()
	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("扫描表名失败: %v", err)
		}
		tables[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历表名失败: %v", err)
	}
	for _, name := range []string{"devices", "agents", "sessions", "messages", "auth_tokens"} {
		if !tables[name] {
			t.Errorf("缺少表 %s", name)
		}
	}
	for _, index := range []string{
		"idx_devices_online", "idx_agents_device", "idx_agents_registered",
		"idx_agents_type", "idx_messages_session_seq",
	} {
		var count int
		if err := store.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&count); err != nil {
			t.Fatalf("查询索引失败: %v", err)
		}
		if count != 1 {
			t.Errorf("缺少索引 %s", index)
		}
	}
	var version int
	if err := store.DB.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("读取 user_version 失败: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version 期望 %d，实际 %d", schemaVersion, version)
	}

	// 迁移必须幂等：重复执行不报错且表数量不变。
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("重复迁移失败: %v", err)
	}
}

// TestForeignKeysEnabled 校验 PRAGMA foreign_keys=ON 生效（级联清空的开关）。
func TestForeignKeysEnabled(t *testing.T) {
	store := newTestStore(t)
	enabled, err := store.ForeignKeysEnabled(context.Background())
	if err != nil {
		t.Fatalf("读取 foreign_keys 失败: %v", err)
	}
	if !enabled {
		t.Fatal("PRAGMA foreign_keys 必须为 ON，否则取消注册无法级联清空 messages")
	}
}

// TestCascadeDeleteClearsMessages 校验 devices → agents → sessions → messages 级联链。
func TestCascadeDeleteClearsMessages(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	insertFixture(t, store, "dev_cascade", "dev_cascade:claude", protocol.AgentTypeClaude, 3)
	if got := countRows(t, store, `SELECT COUNT(*) FROM messages`); got != 3 {
		t.Fatalf("前置条件：应有 3 条消息，实际 %d", got)
	}
	if _, err := store.DB.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, "dev_cascade"); err != nil {
		t.Fatalf("删除设备失败: %v", err)
	}
	for table, query := range map[string]string{
		"agents":   `SELECT COUNT(*) FROM agents`,
		"sessions": `SELECT COUNT(*) FROM sessions`,
		"messages": `SELECT COUNT(*) FROM messages`,
	} {
		if got := countRows(t, store, query); got != 0 {
			t.Errorf("删除设备后 %s 应被级联清空，实际 %d 行", table, got)
		}
	}
}

// TestResetOnlineStates 校验进程启动时 online 统一重置为 0（K11 / 三态独立）。
func TestResetOnlineStates(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	insertFixture(t, store, "dev_online", "dev_online:claude", protocol.AgentTypeClaude, 0)
	if got := countRows(t, store, `SELECT COUNT(*) FROM devices WHERE online = 1`); got != 1 {
		t.Fatalf("前置条件：应有 1 台在线设备，实际 %d", got)
	}
	if err := store.ResetOnlineStates(ctx); err != nil {
		t.Fatalf("重置在线态失败: %v", err)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM devices WHERE online <> 0`); got != 0 {
		t.Errorf("设备 online 未重置，仍有 %d 行在线", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM agents WHERE online <> 0`); got != 0 {
		t.Errorf("agent online 未重置，仍有 %d 行在线", got)
	}
	// registered 不受影响（三态独立）。
	if got := countRows(t, store, `SELECT COUNT(*) FROM agents WHERE registered = 1`); got != 1 {
		t.Errorf("重置 online 不应改变 registered，实际已注册 %d 行", got)
	}
}
