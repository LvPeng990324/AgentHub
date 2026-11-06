package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrDeviceNotFound / ErrAgentNotFound / ErrSessionNotFound 是 Repo 层的哨兵错误，
// Handler 通过 errors.Is 统一映射为 §6.5 业务错误码（K10）。
var (
	ErrDeviceNotFound  = errors.New("设备不存在")
	ErrAgentNotFound   = errors.New("agent 不存在")
	ErrSessionNotFound = errors.New("会话不存在")
)

// nowUTC 返回 RFC3339 / ISO 8601 UTC 文本（K5：所有时间戳统一格式，DB 存 TEXT）。
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// boolToInt 把布尔三态值转为 SQLite INTEGER 0/1。
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// rowScanner 抽象 *sql.Row 与 *sql.Rows 的 Scan 方法，便于共用扫描函数。
type rowScanner interface {
	Scan(dest ...any) error
}

// deviceColumns / agentColumns 集中列顺序，避免各查询手写不一致。
const deviceColumns = `id, name, os, arch, adapter_version, protocol_version, remark, online, last_seen_at, created_at, updated_at`

const agentColumns = `a.id, a.device_id, a.type, a.display_name, a.name, a.version, a.exec_path, a.remark,
       a.registered, a.online, a.last_seen_at, a.created_at, a.updated_at,
       d.name, COALESCE(s.id, '')`

// scanDevice 把一行 devices（或 JOIN 后的 devices 列）扫描为 Device 实体。
func scanDevice(row rowScanner) (Device, error) {
	var device Device
	var online int
	var lastSeen sql.NullString
	err := row.Scan(
		&device.ID, &device.Name, &device.OS, &device.Arch,
		&device.AdapterVersion, &device.ProtocolVersion, &device.Remark,
		&online, &lastSeen, &device.CreatedAt, &device.UpdatedAt,
	)
	if err != nil {
		return Device{}, err
	}
	device.Online = online != 0
	if lastSeen.Valid {
		device.LastSeenAt = lastSeen.String
	}
	return device, nil
}

// DeviceUpsert 是适配器 device.register 快照中的设备信息。
// 不包含 remark / registered：备注与注册态由用户维护，快照一律不覆盖。
type DeviceUpsert struct {
	ID              string
	Name            string
	OS              string
	Arch            string
	AdapterVersion  string
	ProtocolVersion int
}

// DeviceRepo 封装 devices 表读写。
type DeviceRepo struct {
	store *Store
}

// NewDeviceRepo 构造设备 Repo。
func NewDeviceRepo(store *Store) *DeviceRepo {
	return &DeviceRepo{store: store}
}

// Upsert 幂等写入设备快照并建立在线态。
// ON CONFLICT 分支刻意不更新 remark（用户备注）与 created_at（K11）。
func (r *DeviceRepo) Upsert(ctx context.Context, device DeviceUpsert) error {
	now := nowUTC()
	_, err := r.store.DB.ExecContext(ctx, `
INSERT INTO devices(id, name, os, arch, adapter_version, protocol_version, remark, online, last_seen_at, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, '', 1, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name = excluded.name,
  os = excluded.os,
  arch = excluded.arch,
  adapter_version = excluded.adapter_version,
  protocol_version = excluded.protocol_version,
  online = 1,
  last_seen_at = excluded.last_seen_at,
  updated_at = excluded.updated_at`,
		device.ID, device.Name, device.OS, device.Arch,
		device.AdapterVersion, device.ProtocolVersion,
		now, now, now,
	)
	if err != nil {
		return fmt.Errorf("写入设备快照失败: %w", err)
	}
	return nil
}

// List 返回设备清单（支持名称/备注模糊搜索与分页）。
func (r *DeviceRepo) List(ctx context.Context, query ListQuery) ([]Device, int, error) {
	where := " WHERE 1=1"
	args := make([]any, 0, 4)
	if query.Q != "" {
		where += " AND (name LIKE ? OR remark LIKE ?)"
		pattern := "%" + query.Q + "%"
		args = append(args, pattern, pattern)
	}
	var total int
	if err := r.store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM devices`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计设备数量失败: %w", err)
	}

	rows, err := r.store.DB.QueryContext(ctx,
		`SELECT `+deviceColumns+` FROM devices`+where+
			` ORDER BY created_at ASC, id ASC LIMIT ? OFFSET ?`,
		append(args, query.Limit, query.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("查询设备清单失败: %w", err)
	}
	defer rows.Close()

	devices := make([]Device, 0)
	for rows.Next() {
		device, scanErr := scanDevice(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("扫描设备行失败: %w", scanErr)
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("遍历设备行失败: %w", err)
	}
	return devices, total, nil
}

// GetByID 按主键读取设备；不存在时返回 ErrDeviceNotFound。
func (r *DeviceRepo) GetByID(ctx context.Context, id string) (Device, error) {
	device, err := scanDevice(r.store.DB.QueryRowContext(ctx,
		`SELECT `+deviceColumns+` FROM devices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrDeviceNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("读取设备失败: %w", err)
	}
	return device, nil
}

// UpdateRemark 更新设备备注（≤500 字由 Handler 校验）。
func (r *DeviceRepo) UpdateRemark(ctx context.Context, id string, remark string) (Device, error) {
	result, err := r.store.DB.ExecContext(ctx,
		`UPDATE devices SET remark = ?, updated_at = ? WHERE id = ?`, remark, nowUTC(), id)
	if err != nil {
		return Device{}, fmt.Errorf("更新设备备注失败: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Device{}, fmt.Errorf("读取设备备注更新结果失败: %w", err)
	}
	if affected == 0 {
		return Device{}, ErrDeviceNotFound
	}
	return r.GetByID(ctx, id)
}

// Touch 记录一次心跳（或全量快照）：刷新 last_seen_at 并置在线。
func (r *DeviceRepo) Touch(ctx context.Context, id string) error {
	now := nowUTC()
	_, err := r.store.DB.ExecContext(ctx,
		`UPDATE devices SET online = 1, last_seen_at = ?, updated_at = ? WHERE id = ?`, now, now, id)
	if err != nil {
		return fmt.Errorf("刷新设备心跳失败: %w", err)
	}
	return nil
}

// SetOnline 单独设置设备在线态（不改变 last_seen_at）。
func (r *DeviceRepo) SetOnline(ctx context.Context, id string, online bool) error {
	_, err := r.store.DB.ExecContext(ctx,
		`UPDATE devices SET online = ?, updated_at = ? WHERE id = ?`, boolToInt(online), nowUTC(), id)
	if err != nil {
		return fmt.Errorf("设置设备在线态失败: %w", err)
	}
	return nil
}
