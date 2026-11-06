package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/pkg/protocol"
)

// Hub 管理两条 WS 通道的连接表、按 device_id 路由、浏览器广播与离线扫描（K9）。
//
// 锁纪律：sync.RWMutex 只保护 devices / clients / lastSeen 三个 map；
// 仅在注册、注销、路由、广播取快照时加锁，**帧处理全程不加锁**。
type Hub struct {
	mu       sync.RWMutex
	devices  map[string]*WSConn   // device_id → adapter 连接
	clients  map[string]*WSConn   // conn_id → browser 连接
	lastSeen map[string]time.Time // device_id → 最近一次合法 register / heartbeat

	cfg         *config.ServerConfig
	store       *Store
	deviceRepo  *DeviceRepo
	agentRepo   *AgentRepo
	messageRepo *MessageRepo
	chat        *ChatOrchestrator
}

// NewHub 构造 Hub。根 ctx 通过 Run(ctx) 逐层透传给离线扫描器与 WS 连接（K8）。
func NewHub(
	cfg *config.ServerConfig,
	store *Store,
	deviceRepo *DeviceRepo,
	agentRepo *AgentRepo,
	messageRepo *MessageRepo,
) *Hub {
	return &Hub{
		devices:     make(map[string]*WSConn),
		clients:     make(map[string]*WSConn),
		lastSeen:    make(map[string]time.Time),
		cfg:         cfg,
		store:       store,
		deviceRepo:  deviceRepo,
		agentRepo:   agentRepo,
		messageRepo: messageRepo,
	}
}

// AttachChat 装配对话编排器（解决 Hub ↔ Chat 的构造期循环引用）。
func (h *Hub) AttachChat(chat *ChatOrchestrator) {
	h.chat = chat
}

// Register 把连接登记进连接表。U3：同一 device_id 的新连接踢掉旧连接，避免双进程重复上报快照。
func (h *Hub) Register(conn *WSConn) {
	if conn == nil {
		return
	}
	if conn.Kind == ConnKindAdapter {
		h.mu.Lock()
		old := h.devices[conn.DeviceID]
		h.mu.Unlock()
		if old != nil && old != conn {
			slog.Warn("同一 device_id 重复连接，踢掉旧连接",
				"device_id", conn.DeviceID, "old_conn_id", old.ID, "new_conn_id", conn.ID)
			old.Close()
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch conn.Kind {
	case ConnKindAdapter:
		h.devices[conn.DeviceID] = conn
		h.lastSeen[conn.DeviceID] = time.Now()
	case ConnKindClient:
		h.clients[conn.ID] = conn
	}
}

// Unregister 注销连接。设备断开时**不立即置离线**：保留 lastSeen，
// 由 45s 离线扫描统一判定（§7.5 宽限避免网络抖动误判）。
func (h *Hub) Unregister(conn *WSConn) {
	if conn == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch conn.Kind {
	case ConnKindAdapter:
		if current, ok := h.devices[conn.DeviceID]; ok && current == conn {
			delete(h.devices, conn.DeviceID)
		}
	case ConnKindClient:
		if current, ok := h.clients[conn.ID]; ok && current == conn {
			delete(h.clients, conn.ID)
		}
	}
}

// SendToConn 给单条连接下发一帧，连接级 seq 由该连接自己分配（L1）。
func (h *Hub) SendToConn(conn *WSConn, frameType string, payload any) bool {
	if conn == nil {
		return false
	}
	message, err := protocol.NewMessage(frameType, conn.NextSeq(), payload)
	if err != nil {
		slog.Error("构造下行帧失败", "type", frameType, "err", err)
		return false
	}
	return conn.TrySend(message)
}

// SendToDevice 按 device_id 把帧路由到该设备的 adapter 连接（F12 等）。
func (h *Hub) SendToDevice(deviceID string, frameType string, payload any) bool {
	h.mu.RLock()
	conn := h.devices[deviceID]
	h.mu.RUnlock()
	if conn == nil {
		return false
	}
	return h.SendToConn(conn, frameType, payload)
}

// BroadcastClients 向全部浏览器连接广播一帧（每条连接独立分配 seq）。
func (h *Hub) BroadcastClients(frameType string, payload any) {
	h.mu.RLock()
	connections := make([]*WSConn, 0, len(h.clients))
	for _, conn := range h.clients {
		connections = append(connections, conn)
	}
	h.mu.RUnlock()
	for _, conn := range connections {
		h.SendToConn(conn, frameType, payload)
	}
}

// HasDeviceConnection 判断设备当前是否有存活的 adapter WS 连接。
func (h *Hub) HasDeviceConnection(deviceID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.devices[deviceID]
	return ok
}

// ClientCount 返回当前浏览器连接数（供观测与测试）。
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// NoteActive 记录一次合法 register / heartbeat 时间，供离线扫描判定。
func (h *Hub) NoteActive(deviceID string) {
	h.mu.Lock()
	h.lastSeen[deviceID] = time.Now()
	h.mu.Unlock()
}

// OnDeviceRegister 处理 F01 全量快照：先 upsert 设备，再以快照收敛 agent（幂等、单向只增不减），
// 最后广播在线态；返回该设备已注册 agent id 列表供 F02 ack 回带。
func (h *Hub) OnDeviceRegister(ctx context.Context, payload protocol.DeviceRegisterPayload) ([]string, error) {
	if err := h.deviceRepo.Upsert(ctx, DeviceUpsert{
		ID:              payload.DeviceID,
		Name:            payload.Name,
		OS:              payload.OS,
		Arch:            payload.Arch,
		AdapterVersion:  payload.AdapterVersion,
		ProtocolVersion: payload.ProtocolVersion,
	}); err != nil {
		return nil, err
	}
	if err := h.agentRepo.UpsertSnapshot(ctx, payload.DeviceID, payload.Agents); err != nil {
		return nil, err
	}
	h.NoteActive(payload.DeviceID)
	registeredIDs, err := h.agentRepo.RegisteredIDsByDevice(ctx, payload.DeviceID)
	if err != nil {
		return nil, err
	}
	h.broadcastDeviceState(ctx, payload.DeviceID, true, protocol.ReasonDeviceOnline)
	return registeredIDs, nil
}

// OnDeviceHeartbeat 处理 F03：刷新设备心跳与快照中出现的 agent 在线态并回状态（仅状态翻转时广播）。
func (h *Hub) OnDeviceHeartbeat(ctx context.Context, deviceID string, agentTypes []string) error {
	before, err := h.deviceRepo.GetByID(ctx, deviceID)
	if err != nil {
		return err
	}
	if err := h.deviceRepo.Touch(ctx, deviceID); err != nil {
		return err
	}
	if len(agentTypes) > 0 {
		if err := h.agentRepo.SetAgentsOnlineByTypes(ctx, deviceID, agentTypes); err != nil {
			return err
		}
	}
	h.NoteActive(deviceID)
	if !before.Online {
		h.broadcastDeviceState(ctx, deviceID, true, protocol.ReasonDeviceOnline)
	}
	return nil
}

// broadcastDeviceState 广播 F11 agent.status（全量）与 F15 device.status。
func (h *Hub) broadcastDeviceState(ctx context.Context, deviceID string, online bool, reason string) {
	items, err := h.agentRepo.StatusItemsForDevice(ctx, deviceID)
	if err != nil {
		slog.Warn("广播设备状态前读取 agent 状态失败", "device_id", deviceID, "err", err)
		return
	}
	device, err := h.deviceRepo.GetByID(ctx, deviceID)
	if err != nil {
		slog.Warn("广播设备状态前读取设备失败", "device_id", deviceID, "err", err)
		return
	}
	h.BroadcastClients(protocol.FrameAgentStatus, protocol.AgentStatusPayload{
		Agents: items,
		Reason: reason,
	})
	h.BroadcastClients(protocol.FrameDeviceStatus, protocol.DeviceStatusPayload{
		DeviceID:   deviceID,
		Online:     online,
		LastSeenAt: device.LastSeenAt,
	})
}

// Run 启动单 goroutine 的 5s 离线扫描器（K9 ③）。
func (h *Hub) Run(ctx context.Context) {
	go h.scanLoop(ctx)
}

// scanLoop 每 5s 批量判定一次离线，批量算完再一次性广播。
func (h *Hub) scanLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.scanOffline(ctx)
		}
	}
}

// scanOffline 找出「连续 45s（3×15s）无心跳」且仍在线的设备并统一置离线。
func (h *Hub) scanOffline(ctx context.Context) {
	timeout := h.cfg.HeartbeatTimeout()
	now := time.Now()
	h.mu.RLock()
	expired := make([]string, 0)
	for deviceID, last := range h.lastSeen {
		if now.Sub(last) > timeout {
			expired = append(expired, deviceID)
		}
	}
	h.mu.RUnlock()

	for _, deviceID := range expired {
		if err := h.markOffline(ctx, deviceID); err != nil {
			slog.Warn("设备离线判定失败", "device_id", deviceID, "err", err)
		}
	}
}

// markOffline 置设备与其下全部 agent 为离线、广播状态，并兜底残留 streaming 消息。
// last_seen_at 保持不变（§7.5 判定动作）。
func (h *Hub) markOffline(ctx context.Context, deviceID string) error {
	device, err := h.deviceRepo.GetByID(ctx, deviceID)
	if err != nil {
		return err
	}
	if !device.Online {
		return nil // 已离线，避免重复广播
	}
	if err := h.deviceRepo.SetOnline(ctx, deviceID, false); err != nil {
		return err
	}
	if err := h.agentRepo.SetDeviceAgentsOnline(ctx, deviceID, false); err != nil {
		return err
	}
	h.broadcastDeviceState(ctx, deviceID, false, protocol.ReasonDeviceOffline)
	if h.chat != nil {
		if _, err := h.chat.InterruptStaleStreaming(ctx, deviceID); err != nil {
			slog.Warn("离线时兜底流式消息失败", "device_id", deviceID, "err", err)
		}
	}
	return nil
}

// CloseAll 关闭全部存活连接（优雅退出时调用）。
func (h *Hub) CloseAll() {
	h.mu.RLock()
	connections := make([]*WSConn, 0, len(h.devices)+len(h.clients))
	for _, conn := range h.devices {
		connections = append(connections, conn)
	}
	for _, conn := range h.clients {
		connections = append(connections, conn)
	}
	h.mu.RUnlock()
	for _, conn := range connections {
		conn.Close()
	}
}
