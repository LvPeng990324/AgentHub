package server

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（transpile），CGO_ENABLED=0 下可编译
)

// migrations 逐条幂等执行（§5.2 DDL + §5.3 迁移方案）。
// 顺序即外键依赖顺序：devices → agents → sessions → messages → auth_tokens。
// 严禁把取消注册实现为删除 devices / agents 行；会话清空一律走 sessions 的级联。
var migrations = []string{
	// ① 设备：一台适配器一行
	`CREATE TABLE IF NOT EXISTS devices (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL,
  os               TEXT NOT NULL,
  arch             TEXT NOT NULL,
  adapter_version  TEXT NOT NULL DEFAULT '',
  protocol_version INTEGER NOT NULL DEFAULT 1,
  remark           TEXT NOT NULL DEFAULT '',
  online           INTEGER NOT NULL DEFAULT 0,
  last_seen_at     TEXT,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
)`,
	`CREATE INDEX IF NOT EXISTS idx_devices_online ON devices(online)`,

	// ② Agent：稳定键 id = '<device_id>:<type>'
	`CREATE TABLE IF NOT EXISTS agents (
  id            TEXT PRIMARY KEY,
  device_id     TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  type          TEXT NOT NULL,
  display_name  TEXT NOT NULL,
  name          TEXT NOT NULL,
  version       TEXT NOT NULL DEFAULT '',
  exec_path     TEXT NOT NULL DEFAULT '',
  remark        TEXT NOT NULL DEFAULT '',
  registered    INTEGER NOT NULL DEFAULT 0,
  online        INTEGER NOT NULL DEFAULT 0,
  last_seen_at  TEXT,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  UNIQUE(device_id, type)
)`,
	`CREATE INDEX IF NOT EXISTS idx_agents_device ON agents(device_id)`,
	`CREATE INDEX IF NOT EXISTS idx_agents_registered ON agents(registered)`,
	`CREATE INDEX IF NOT EXISTS idx_agents_type ON agents(type)`,

	// ③ 会话：agent_id UNIQUE 是「一个 agent 一条会话」的库级保证
	`CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  agent_id   TEXT NOT NULL UNIQUE REFERENCES agents(id) ON DELETE CASCADE,
  resume_id  TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
)`,

	// ④ 消息：seq 为会话内单调递增，由平台分配，不随 WS 重连重置
	`CREATE TABLE IF NOT EXISTS messages (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq        INTEGER NOT NULL,
  role       TEXT NOT NULL,
  content    TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'complete',
  created_at TEXT NOT NULL,
  UNIQUE(session_id, seq)
)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_session_seq ON messages(session_id, seq)`,

	// ⑤ 登录 token：只存 SHA-256 哈希 + 过期时间，登出即时删哈希
	`CREATE TABLE IF NOT EXISTS auth_tokens (
  token_hash TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
)`,
}

// schemaVersion 是首版 schema 版本号，写入 PRAGMA user_version。
const schemaVersion = 1

// Store 持有单连接 SQLite 句柄与迁移逻辑。
type Store struct {
	DB   *sql.DB
	Path string
}

// OpenStore 打开（必要时自动创建）SQLite 单文件、应用 PRAGMA，并执行幂等迁移。
//
// 关键约束（§5.3 / K11）：
//   - SetMaxOpenConns(1)：SQLite 单写者，且保证连接级 PRAGMA foreign_keys=ON 对全部语句生效；
//   - WAL + busy_timeout=5000：规避 database is locked；
//   - 启动时把所有 online 重置为 0（online 不持久化，由 WS + 心跳重新推导）。
func OpenStore(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		path = "./agenthub.db"
	}
	parent := filepath.Dir(path)
	if parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据库父目录失败: %w", err)
		}
	}

	// modernc.org/sqlite 通过 _pragma 查询参数在每条新连接上应用 PRAGMA；
	// 下面还会显式再执行一次，双保险且便于单测断言。
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	store := &Store{DB: db, Path: path}
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA busy_timeout = 5000;",
	}
	for _, statement := range pragmas {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("应用 SQLite PRAGMA 失败: %w", err)
		}
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 SQLite 失败: %w", err)
	}
	if err := store.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Migrate 逐条执行幂等 DDL，写入 user_version，并把全部 online 重置为 0。
func (s *Store) Migrate(ctx context.Context) error {
	for index, statement := range migrations {
		if _, err := s.DB.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("执行迁移 #%d 失败: %w", index+1, err)
		}
	}
	if _, err := s.DB.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d;", schemaVersion)); err != nil {
		return fmt.Errorf("写入 schema 版本失败: %w", err)
	}
	return s.ResetOnlineStates(ctx)
}

// ResetOnlineStates 进程启动时把所有设备与 agent 的 online 置 0（K11）。
// online 是连接 + 心跳推导的实时态，绝不与 discovered / registered 互相推断。
func (s *Store) ResetOnlineStates(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, `UPDATE devices SET online = 0 WHERE online <> 0`); err != nil {
		return fmt.Errorf("重置设备在线态失败: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE agents SET online = 0 WHERE online <> 0`); err != nil {
		return fmt.Errorf("重置 Agent 在线态失败: %w", err)
	}
	return nil
}

// BeginTx 开启事务（取消注册等跨表写路径必须使用）。
func (s *Store) BeginTx(ctx context.Context) (*sql.Tx, error) {
	return s.DB.BeginTx(ctx, nil)
}

// Ping 供 /api/v1/health 探测数据库可用性。
func (s *Store) Ping(ctx context.Context) error {
	return s.DB.PingContext(ctx)
}

// ForeignKeysEnabled 返回当前连接的 foreign_keys 开关状态（单测断言用）。
func (s *Store) ForeignKeysEnabled(ctx context.Context) (bool, error) {
	var enabled int
	if err := s.DB.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		return false, fmt.Errorf("读取 foreign_keys 状态失败: %w", err)
	}
	return enabled == 1, nil
}

// Close 关闭数据库句柄。
func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.Close()
}
