package adapter

// 平台 WS 客户端（§7 / §8）：连接级 seq 由本文件统一维护，适配器绝不分配会话级 seq。
// 每次连接/重连都立即发 F01 全量快照；消费平台下发的 F05/F10/F12；上行 F07/F08/F09。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/pkg/protocol"
)

const (
	minReconnectBackoff = 1 * time.Second
	maxReconnectBackoff = 30 * time.Second
	jitterRatio         = 0.2
	stableConnectionFor = 10 * time.Second
	wsWriteWait         = 10 * time.Second
	wsPongWait          = 60 * time.Second
	wsPingPeriod        = (wsPongWait * 9) / 10 // 54s
	wsSendBuffer        = 256
	wsHandshakeTimeout  = 10 * time.Second
)

// FrameSender 是 SessionManager 发送下行帧的抽象（重连对上层透明）。
type FrameSender interface {
	Send(frameType string, payload any) bool
}

// wsLink 是一条 WS 连接的全部状态；重连会新建对象，故 seq / lastSeq 天然重置（L1）。
type wsLink struct {
	ws        *websocket.Conn
	send      chan []byte
	done      chan struct{}
	closeOnce sync.Once
	seq       atomic.Int64
	lastSeq   int64 // 仅 readPump 单 goroutine 访问
}

func (l *wsLink) close() {
	l.closeOnce.Do(func() {
		close(l.done)
		_ = l.ws.Close()
	})
}

// WSClient 管理与平台 /api/v1/ws/adapter 的唯一长连接。
type WSClient struct {
	cfg        *config.AdapterConfig
	version    string
	deviceID   string
	discoverer *Discoverer
	manager    *SessionManager

	mu              sync.RWMutex
	current         *wsLink
	heartbeatSecond int
}

// NewWSClient 构造 WS 客户端。
func NewWSClient(
	cfg *config.AdapterConfig,
	version string,
	deviceID string,
	discoverer *Discoverer,
) *WSClient {
	return &WSClient{
		cfg:             cfg,
		version:         version,
		deviceID:        deviceID,
		discoverer:      discoverer,
		heartbeatSecond: cfg.EffectiveHeartbeatSeconds(),
	}
}

// AttachManager 装配对话编排器（解决构造期循环引用）。
func (c *WSClient) AttachManager(manager *SessionManager) {
	c.manager = manager
}

// Send 非阻塞下发一帧；连接级 seq 由当前连接分配，重连重置为 1。
func (c *WSClient) Send(frameType string, payload any) bool {
	c.mu.RLock()
	link := c.current
	c.mu.RUnlock()
	if link == nil {
		return false
	}
	message, err := protocol.NewMessage(frameType, link.seq.Add(1), payload)
	if err != nil {
		slog.Error("构造上行帧失败", "type", frameType, "err", err)
		return false
	}
	data, err := message.Marshal()
	if err != nil {
		return false
	}
	select {
	case <-link.done:
		return false
	default:
	}
	select {
	case <-link.done:
		return false
	case link.send <- data:
		return true
	default:
		slog.Warn("WS 发送缓冲已满，丢弃上行帧", "type", frameType)
		return false
	}
}

// SendSnapshot 发送 F01 device.register 全量快照（每次连接成功与每次周期重探测都调用）。
func (c *WSClient) SendSnapshot(snapshot []DiscoveredAgent) {
	c.Send(protocol.FrameDeviceRegister, c.registerPayload(snapshot))
}

func (c *WSClient) registerPayload(snapshot []DiscoveredAgent) protocol.DeviceRegisterPayload {
	agents := make([]protocol.AgentSnapshot, 0, len(snapshot))
	for _, agent := range snapshot {
		agents = append(agents, protocol.AgentSnapshot{
			Type:        agent.Type,
			DisplayName: agent.DisplayName,
			Version:     agent.Version,
			ExecPath:    agent.ExecPath,
			Status:      agent.Status,
		})
	}
	return protocol.DeviceRegisterPayload{
		DeviceID:        c.deviceID,
		Name:            c.cfg.GetDeviceName(),
		OS:              runtime.GOOS, // WSL 内跑 Linux 版即报 linux（C-13 / Q11）
		Arch:            runtime.GOARCH,
		AdapterVersion:  c.version,
		ProtocolVersion: protocol.ProtocolVersion,
		Agents:          agents,
	}
}

// Run 阻塞运行重连循环，直到 ctx 取消。
func (c *WSClient) Run(ctx context.Context) error {
	backoff := minReconnectBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		startedAt := time.Now()
		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			err = errors.New("连接已关闭")
		}
		if time.Since(startedAt) >= stableConnectionFor {
			backoff = minReconnectBackoff
		} else {
			backoff = min(time.Duration(float64(backoff)*2), maxReconnectBackoff)
		}
		jitter := time.Duration(float64(backoff) * jitterRatio * (rand.Float64()*2 - 1))
		delay := backoff + jitter
		if delay < minReconnectBackoff {
			delay = minReconnectBackoff
		}
		if delay > maxReconnectBackoff {
			delay = maxReconnectBackoff
		}
		// K7：只打原因与退避时长，不打印完整 WS URL / token。
		slog.Warn("平台连接断开，按退避策略重连",
			"reason", err.Error(), "next_retry_ms", delay.Milliseconds())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// connectAndServe 建立一条连接并阻塞到该连接结束。
func (c *WSClient) connectAndServe(ctx context.Context) error {
	target, err := c.buildURL()
	if err != nil {
		return err
	}
	header := http.Header{}
	// 双保险：query 优先，服务端同时兼容 Authorization: Bearer（§7.1）。
	header.Set("Authorization", "Bearer "+c.cfg.DeviceToken)
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: wsHandshakeTimeout,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
	}
	ws, response, err := dialer.DialContext(ctx, target, header)
	if err != nil {
		if response != nil {
			return fmt.Errorf("WS 握手失败 (HTTP %d)", response.StatusCode)
		}
		return fmt.Errorf("WS 连接失败: %w", err)
	}

	link := &wsLink{ws: ws, send: make(chan []byte, wsSendBuffer), done: make(chan struct{})}
	c.mu.Lock()
	c.current = link
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.current == link {
			c.current = nil
		}
		c.mu.Unlock()
		link.close()
	}()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-connCtx.Done()
		link.close()
	}()

	// ★连接成功立即发 F01 全量快照；每次重连都重发，平台以快照幂等 upsert 对账（§7.5）。
	c.SendSnapshot(c.discoverer.Snapshot())
	slog.Info("已连接平台",
		"platform", config.RedactURL(c.cfg.PlatformWSURL),
		"device_id", c.deviceID)

	errCh := make(chan error, 1)
	go func() { errCh <- c.readPump(connCtx, link) }()
	go c.writePump(link)
	go c.heartbeatLoop(connCtx)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case pumpErr := <-errCh:
		return pumpErr
	}
}

// buildURL 在配置地址上追加握手参数；token 只放 query（浏览器/适配器一致）。
func (c *WSClient) buildURL() (string, error) {
	parsed, err := url.Parse(c.cfg.PlatformWSURL)
	if err != nil {
		return "", fmt.Errorf("解析平台 WS 地址失败: %w", err)
	}
	query := parsed.Query()
	query.Set("token", c.cfg.DeviceToken)
	query.Set("device_id", c.deviceID)
	query.Set("name", c.cfg.GetDeviceName())
	query.Set("os", runtime.GOOS)
	query.Set("arch", runtime.GOARCH)
	query.Set("adapter_version", c.version)
	query.Set("protocol_version", strconv.Itoa(protocol.ProtocolVersion))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// readPump 读入站帧 → 校验 → L1 去重 → 分派。
func (c *WSClient) readPump(ctx context.Context, link *wsLink) error {
	defer link.close()
	link.ws.SetReadLimit(protocol.WSReadLimitBytes)
	if err := link.ws.SetReadDeadline(time.Now().Add(wsPongWait)); err != nil {
		return err
	}
	link.ws.SetPongHandler(func(string) error {
		return link.ws.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	for {
		select {
		case <-link.done:
			return errors.New("连接已关闭")
		default:
		}
		_, data, err := link.ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				return fmt.Errorf("WS 连接异常关闭: %w", err)
			}
			return errors.New("WS 连接已关闭")
		}
		var message protocol.Message
		if err := message.Decode(data); err != nil {
			slog.Warn("丢弃非法下行帧", "err", err)
			continue
		}
		if !protocol.IsValidFrameType(message.Type) {
			continue
		}
		// L1：连接级 seq 去重（重连时 link 新建，lastSeq 归零）。
		if message.Seq > 0 {
			if message.Seq <= link.lastSeq {
				continue
			}
			link.lastSeq = message.Seq
		}
		c.dispatch(message)
	}
}

// writePump 是唯一底层写 goroutine，并周期发 Ping 保活。
func (c *WSClient) writePump(link *wsLink) {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		link.close()
	}()
	for {
		select {
		case <-link.done:
			return
		case data := <-link.send:
			if err := link.ws.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return
			}
			if err := link.ws.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			if err := link.ws.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
				return
			}
			if err := link.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// heartbeatLoop 每 15s 发 F03 device.heartbeat。
// 每轮重新读取间隔，使 F02 register_ack 下发的心跳参数对当前连接立即生效。
func (c *WSClient) heartbeatLoop(ctx context.Context) {
	for {
		c.mu.RLock()
		seconds := c.heartbeatSecond
		c.mu.RUnlock()
		if seconds <= 0 {
			seconds = protocol.HeartbeatIntervalSeconds
		}
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		snapshot := c.discoverer.Snapshot()
		agents := make([]protocol.HeartbeatAgent, 0, len(snapshot))
		for _, agent := range snapshot {
			agents = append(agents, protocol.HeartbeatAgent{Type: agent.Type, Status: agent.Status})
		}
		c.Send(protocol.FrameDeviceHeartbeat, protocol.HeartbeatPayload{
			DeviceID: c.deviceID,
			Agents:   agents,
		})
	}
}

// dispatch 处理平台下发的帧；适配器不消费浏览器通道帧。
func (c *WSClient) dispatch(message protocol.Message) {
	switch message.Type {
	case protocol.FrameDeviceRegisterAck:
		var ack protocol.DeviceRegisterAckPayload
		if err := message.DecodePayload(&ack); err != nil {
			slog.Warn("解析 register_ack 失败", "err", err)
			return
		}
		if ack.HeartbeatIntervalS > 0 {
			c.mu.Lock()
			c.heartbeatSecond = ack.HeartbeatIntervalS
			c.mu.Unlock()
		}
		slog.Info("平台已接受设备注册", "accepted", ack.Accepted,
			"registered_agents", len(ack.RegisteredAgentIDs),
			"heartbeat_interval_s", ack.HeartbeatIntervalS)
	case protocol.FrameHeartbeatAck:
		// 心跳回执无需业务处理。
	case protocol.FrameChatSend:
		var payload protocol.ChatSendPayload
		if err := message.DecodePayload(&payload); err != nil {
			slog.Warn("解析 chat.send 失败", "err", err)
			return
		}
		if c.manager != nil {
			c.manager.OnChatSend(payload)
		}
	case protocol.FrameChatStop:
		var payload protocol.ChatStopPayload
		if err := message.DecodePayload(&payload); err != nil {
			slog.Warn("解析 chat.stop 失败", "err", err)
			return
		}
		if c.manager != nil {
			c.manager.OnChatStop(payload)
		}
	case protocol.FrameAgentUnregistered:
		var payload protocol.AgentUnregisteredPayload
		if err := message.DecodePayload(&payload); err != nil {
			slog.Warn("解析 agent.unregistered 失败", "err", err)
			return
		}
		if c.manager != nil {
			c.manager.OnUnregistered(payload.AgentID)
		}
	case protocol.FrameAgentRegistered:
		// 可选 P1 预热帧，首版不处理。
	default:
		slog.Debug("忽略不下发到 adapter 通道的帧", "type", message.Type)
	}
}
