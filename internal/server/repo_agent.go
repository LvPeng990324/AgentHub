package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/agenthub/agenthub/pkg/protocol"
)

const agentFrom = ` FROM agents a
  JOIN devices d ON d.id = a.device_id
  LEFT JOIN sessions s ON s.agent_id = a.id `

// scanAgent 把 JOIN 后的一行扫描为 Agent 实体（含 device_name 与 session_id）。
func scanAgent(row rowScanner) (Agent, error) {
	var agent Agent
	var registered int
	var online int
	var lastSeen sql.NullString
	err := row.Scan(
		&agent.ID, &agent.DeviceID, &agent.Type, &agent.DisplayName,
		&agent.Name, &agent.Version, &agent.ExecPath, &agent.Remark,
		&registered, &online, &lastSeen,
		&agent.CreatedAt, &agent.UpdatedAt,
		&agent.DeviceName, &agent.SessionID,
	)
	if err != nil {
		return Agent{}, err
	}
	agent.Registered = registered != 0
	agent.Online = online != 0
	if lastSeen.Valid {
		agent.LastSeenAt = lastSeen.String
	}
	return agent, nil
}

// AgentListFilter 汇总 E08 / E09 的筛选条件。
type AgentListFilter struct {
	Query          ListQuery
	RegisteredOnly bool // E09 仅返回 registered=1
}

// AgentRepo 封装 agents 表读写与注册态变更。
type AgentRepo struct {
	store *Store
}

// NewAgentRepo 构造 Agent Repo。
func NewAgentRepo(store *Store) *AgentRepo {
	return &AgentRepo{store: store}
}

// UpsertSnapshot 以适配器全量快照为准收敛 agent 清单（§7.5 重连对账 / K11）。
//
// 铁律：
//   - 稳定键 agent_id = "<device_id>:<type>"，快照 upsert 幂等；
//   - 绝不覆盖 registered（用户注册态）与 remark（用户备注），也不覆盖 name / created_at；
//   - 快照中出现的 agent 置 online=1；快照缺失的 agent **不删行**，仅 online=0（单向只增不减，保护正在对话的 agent）；
//   - 整个收敛过程在单事务内完成。
func (r *AgentRepo) UpsertSnapshot(ctx context.Context, deviceID string, snapshots []protocol.AgentSnapshot) error {
	tx, err := r.store.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("开启快照事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := nowUTC()
	ids := make([]string, 0, len(snapshots))
	for _, item := range snapshots {
		if strings.TrimSpace(item.Type) == "" {
			continue
		}
		agentID := deviceID + ":" + item.Type
		ids = append(ids, agentID)
		displayName := item.DisplayName
		if displayName == "" {
			displayName = item.Type
		}
		_, execErr := tx.ExecContext(ctx, `
INSERT INTO agents(id, device_id, type, display_name, name, version, exec_path, remark, registered, online, last_seen_at, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, '', 0, 1, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  display_name = excluded.display_name,
  version      = excluded.version,
  exec_path    = excluded.exec_path,
  online       = 1,
  last_seen_at = excluded.last_seen_at,
  updated_at   = excluded.updated_at`,
			agentID, deviceID, item.Type, displayName, item.Type,
			item.Version, item.ExecPath, now, now, now,
		)
		if execErr != nil {
			return fmt.Errorf("写入 agent 快照 %s 失败: %w", agentID, execErr)
		}
	}

	// 快照缺失的 agent 只置离线，不删行；registered 与 remark 保持不动。
	if len(ids) == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE agents SET online = 0 WHERE device_id = ? AND online <> 0`, deviceID); err != nil {
			return fmt.Errorf("收敛快照离线态失败: %w", err)
		}
	} else {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, 0, len(ids)+1)
		args = append(args, deviceID)
		for _, id := range ids {
			args = append(args, id)
		}
		statement := fmt.Sprintf(
			`UPDATE agents SET online = 0 WHERE device_id = ? AND id NOT IN (%s) AND online <> 0`,
			placeholders)
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			return fmt.Errorf("收敛快照离线态失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交快照事务失败: %w", err)
	}
	return nil
}

// List 返回 agent 清单，支持 q / type / online / device_id / registered 筛选与分页。
func (r *AgentRepo) List(ctx context.Context, filter AgentListFilter) ([]Agent, int, error) {
	where := " WHERE 1=1"
	args := make([]any, 0, 8)
	if filter.RegisteredOnly {
		where += " AND a.registered = 1"
	}
	if filter.Query.Registered != nil {
		where += " AND a.registered = ?"
		args = append(args, boolToInt(*filter.Query.Registered))
	}
	if filter.Query.Type != "" {
		where += " AND a.type = ?"
		args = append(args, filter.Query.Type)
	}
	if filter.Query.DeviceID != "" {
		where += " AND a.device_id = ?"
		args = append(args, filter.Query.DeviceID)
	}
	if filter.Query.Online != nil {
		where += " AND a.online = ?"
		args = append(args, boolToInt(*filter.Query.Online))
	}
	if filter.Query.Q != "" {
		where += " AND (a.display_name LIKE ? OR a.name LIKE ? OR a.remark LIKE ? OR a.type LIKE ? OR d.name LIKE ?)"
		pattern := "%" + filter.Query.Q + "%"
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}

	var total int
	if err := r.store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*)`+agentFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计 agent 数量失败: %w", err)
	}

	rows, err := r.store.DB.QueryContext(ctx,
		`SELECT `+agentColumns+agentFrom+where+
			` ORDER BY a.created_at ASC, a.id ASC LIMIT ? OFFSET ?`,
		append(args, filter.Query.Limit, filter.Query.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询 agent 清单失败: %w", err)
	}
	defer rows.Close()

	agents := make([]Agent, 0)
	for rows.Next() {
		agent, scanErr := scanAgent(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("扫描 agent 行失败: %w", scanErr)
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("遍历 agent 行失败: %w", err)
	}
	return agents, total, nil
}

// GetByID 读取单个 agent（含 device_name / session_id）；不存在返回 ErrAgentNotFound。
func (r *AgentRepo) GetByID(ctx context.Context, id string) (Agent, error) {
	agent, err := scanAgent(r.store.DB.QueryRowContext(ctx,
		`SELECT `+agentColumns+agentFrom+` WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrAgentNotFound
	}
	if err != nil {
		return Agent{}, fmt.Errorf("读取 agent 失败: %w", err)
	}
	return agent, nil
}

// DeviceIDOf 返回 agent 所属设备 id（E11 第一步 / 帧路由需要）。
func (r *AgentRepo) DeviceIDOf(ctx context.Context, agentID string) (string, error) {
	var deviceID string
	err := r.store.DB.QueryRowContext(ctx,
		`SELECT device_id FROM agents WHERE id = ?`, agentID).Scan(&deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrAgentNotFound
	}
	if err != nil {
		return "", fmt.Errorf("查询 agent 所属设备失败: %w", err)
	}
	return deviceID, nil
}

// setRegisteredWith 在给定的 DB/Tx 上置注册位，绝不动 online / remark（K11）。
func setRegisteredWith(ctx context.Context, runner sqlRunner, agentID string, registered bool) error {
	_, err := runner.ExecContext(ctx,
		`UPDATE agents SET registered = ?, updated_at = ? WHERE id = ?`,
		boolToInt(registered), nowUTC(), agentID)
	if err != nil {
		return fmt.Errorf("更新 agent 注册态失败: %w", err)
	}
	return nil
}

// RegisterOne 注册单个 agent（E10，幂等）：单事务内置 registered=1 并 EnsureSession。
// 返回更新后的完整 Agent（含 session_id）与其 device_id。
func (r *AgentRepo) RegisterOne(ctx context.Context, agentID string) (Agent, string, error) {
	tx, err := r.store.BeginTx(ctx)
	if err != nil {
		return Agent{}, "", fmt.Errorf("开启注册事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var deviceID string
	if err := tx.QueryRowContext(ctx,
		`SELECT device_id FROM agents WHERE id = ?`, agentID).Scan(&deviceID); errors.Is(err, sql.ErrNoRows) {
		return Agent{}, "", ErrAgentNotFound
	} else if err != nil {
		return Agent{}, "", fmt.Errorf("查询 agent 失败: %w", err)
	}
	if err := setRegisteredWith(ctx, tx, agentID, true); err != nil {
		return Agent{}, "", err
	}
	if _, err := ensureSessionWith(ctx, tx, agentID); err != nil {
		return Agent{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Agent{}, "", fmt.Errorf("提交注册事务失败: %w", err)
	}
	agent, err := r.GetByID(ctx, agentID)
	if err != nil {
		return Agent{}, "", err
	}
	return agent, deviceID, nil
}

// RegisterBatch 批量注册（E12，幂等）：单事务内逐个置位 + EnsureSession，
// 不存在的 id 进入 skipped。返回 registered / skipped / 与 registered 一一对应的 device_id 列表。
func (r *AgentRepo) RegisterBatch(ctx context.Context, agentIDs []string) ([]string, []string, []string, error) {
	registered := make([]string, 0, len(agentIDs))
	skipped := make([]string, 0)
	deviceIDs := make([]string, 0, len(agentIDs))

	tx, err := r.store.BeginTx(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("开启批量注册事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, agentID := range agentIDs {
		var deviceID string
		err := tx.QueryRowContext(ctx,
			`SELECT device_id FROM agents WHERE id = ?`, agentID).Scan(&deviceID)
		if errors.Is(err, sql.ErrNoRows) {
			skipped = append(skipped, agentID)
			continue
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("查询待注册 agent 失败: %w", err)
		}
		if err := setRegisteredWith(ctx, tx, agentID, true); err != nil {
			return nil, nil, nil, err
		}
		if _, err := ensureSessionWith(ctx, tx, agentID); err != nil {
			return nil, nil, nil, err
		}
		registered = append(registered, agentID)
		deviceIDs = append(deviceIDs, deviceID)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, nil, fmt.Errorf("提交批量注册事务失败: %w", err)
	}
	return registered, skipped, deviceIDs, nil
}

// Unregister 实现 E11 取消注册（D-6 铁律 / §6.3），**单个 db.BeginTx 事务内三步**：
//  1. SELECT device_id（不存在 → ErrAgentNotFound）；
//  2. UPDATE agents SET registered=0（不删行、不动 remark、不动 online）；
//  3. DELETE FROM sessions WHERE agent_id=?（靠 FK 级联清空该 agent 的全部 messages）。
//
// 明确禁止：删 devices 行、删 agents 行、清 remark、断 WS。提交后由 Handler 推 F12 / F11。
func (r *AgentRepo) Unregister(ctx context.Context, agentID string) (string, error) {
	tx, err := r.store.BeginTx(ctx)
	if err != nil {
		return "", fmt.Errorf("开启取消注册事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var deviceID string
	if err := tx.QueryRowContext(ctx,
		`SELECT device_id FROM agents WHERE id = ?`, agentID).Scan(&deviceID); errors.Is(err, sql.ErrNoRows) {
		return "", ErrAgentNotFound
	} else if err != nil {
		return "", fmt.Errorf("查询待取消注册 agent 失败: %w", err)
	}

	if err := setRegisteredWith(ctx, tx, agentID, false); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sessions WHERE agent_id = ?`, agentID); err != nil {
		return "", fmt.Errorf("级联清空会话失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("提交取消注册事务失败: %w", err)
	}
	return deviceID, nil
}

// UpdateRemark 更新 agent 备注（≤500 字由 Handler 校验）。
func (r *AgentRepo) UpdateRemark(ctx context.Context, agentID string, remark string) (Agent, error) {
	result, err := r.store.DB.ExecContext(ctx,
		`UPDATE agents SET remark = ?, updated_at = ? WHERE id = ?`, remark, nowUTC(), agentID)
	if err != nil {
		return Agent{}, fmt.Errorf("更新 agent 备注失败: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Agent{}, fmt.Errorf("读取 agent 备注更新结果失败: %w", err)
	}
	if affected == 0 {
		return Agent{}, ErrAgentNotFound
	}
	return r.GetByID(ctx, agentID)
}

// RegisteredIDsByDevice 返回某设备上已注册 agent 的 id 列表（F02 register_ack 用）。
func (r *AgentRepo) RegisteredIDsByDevice(ctx context.Context, deviceID string) ([]string, error) {
	rows, err := r.store.DB.QueryContext(ctx,
		`SELECT id FROM agents WHERE device_id = ? AND registered = 1 ORDER BY id`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("查询设备已注册 agent 失败: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("扫描已注册 agent id 失败: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历已注册 agent id 失败: %w", err)
	}
	return ids, nil
}

// StatusItemsForDevice 返回某设备下全部 agent 的实时状态，供 F11 广播。
func (r *AgentRepo) StatusItemsForDevice(ctx context.Context, deviceID string) ([]protocol.AgentStatusItem, error) {
	rows, err := r.store.DB.QueryContext(ctx,
		`SELECT id, online, registered FROM agents WHERE device_id = ? ORDER BY id`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("查询设备 agent 状态失败: %w", err)
	}
	defer rows.Close()
	items := make([]protocol.AgentStatusItem, 0)
	for rows.Next() {
		var item protocol.AgentStatusItem
		var online int
		var registered int
		if err := rows.Scan(&item.AgentID, &online, &registered); err != nil {
			return nil, fmt.Errorf("扫描 agent 状态失败: %w", err)
		}
		item.Online = online != 0
		item.Registered = registered != 0
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 agent 状态失败: %w", err)
	}
	return items, nil
}

// SetDeviceAgentsOnline 批量设置某设备下全部 agent 的在线态。
func (r *AgentRepo) SetDeviceAgentsOnline(ctx context.Context, deviceID string, online bool) error {
	_, err := r.store.DB.ExecContext(ctx,
		`UPDATE agents SET online = ? WHERE device_id = ?`, boolToInt(online), deviceID)
	if err != nil {
		return fmt.Errorf("批量设置 agent 在线态失败: %w", err)
	}
	return nil
}

// SetAgentsOnlineByTypes 把心跳负载中出现的 agent 置在线（按 device_id:type 稳定键）。
// 心跳里缺失的 agent 不动，由全量快照对账与离线扫描负责（K11 三态独立）。
func (r *AgentRepo) SetAgentsOnlineByTypes(ctx context.Context, deviceID string, types []string) error {
	unique := make([]string, 0, len(types))
	seen := make(map[string]struct{}, len(types))
	for _, agentType := range types {
		if agentType == "" {
			continue
		}
		if _, exists := seen[agentType]; exists {
			continue
		}
		seen[agentType] = struct{}{}
		unique = append(unique, deviceID+":"+agentType)
	}
	if len(unique) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")
	args := make([]any, 0, len(unique)+1)
	for _, id := range unique {
		args = append(args, id)
	}
	args = append(args, deviceID)
	statement := fmt.Sprintf(
		`UPDATE agents SET online = 1, last_seen_at = ? WHERE id IN (%s) AND device_id = ?`,
		placeholders)
	// last_seen_at 单独作为第一个占位参数。
	allArgs := make([]any, 0, len(args)+1)
	allArgs = append(allArgs, nowUTC())
	allArgs = append(allArgs, args...)
	if _, err := r.store.DB.ExecContext(ctx, statement, allArgs...); err != nil {
		return fmt.Errorf("按心跳刷新 agent 在线态失败: %w", err)
	}
	return nil
}
