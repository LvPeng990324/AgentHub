package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// WS 时序常量（§7.5 照搬 Multica daemonws/hub.go）。
const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10 // 54s
	sendBufferSize = 256
)

// ConnKind 区分两条独立 WS 通道。
type ConnKind string

const (
	ConnKindAdapter ConnKind = "adapter" // /api/v1/ws/adapter，设备令牌鉴权
	ConnKindClient  ConnKind = "client"  // /api/v1/ws/client，登录 token 鉴权
)

// FrameHandler 是入站帧分派回调；实现方只做帧处理，不得持有 Hub 锁。
type FrameHandler func(ctx context.Context, conn *WSConn, message protocol.Message)

// WSConn 是 adapter 与 client 共用的 WebSocket 连接抽象（K9）。
//
// 并发模型：每连接恰好 2 个 goroutine（readPump / writePump），send 为缓冲 256 的字节通道。
// trySend 永不阻塞；send 通道**永不 close**，因此不存在「向已关闭 chan 写」的 panic：
// Close() 通过 sync.Once 只关闭 done 通道与底层 websocket.Conn，writePump 感知 done 后退出。
type WSConn struct {
	ID       string
	DeviceID string // 仅 adapter 使用，作为 Hub.devices 的路由键
	Kind     ConnKind

	conn    *websocket.Conn
	hub     *Hub
	handler FrameHandler
	send    chan []byte
	done    chan struct{}

	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once

	// seq 是本连接「发送方」的连接级序号（L1），从 1 单调 +1，重连重置。
	seq atomic.Int64
	// lastSeq 是本连接「接收方」用于去重/排序的已处理最大入站序号。
	lastSeq atomic.Int64
}

// NewWSConn 构造连接；parentCtx 必须是 main 透传下来的根 ctx（K8）。
func NewWSConn(
	id string,
	deviceID string,
	kind ConnKind,
	ws *websocket.Conn,
	hub *Hub,
	handler FrameHandler,
	parentCtx context.Context,
) *WSConn {
	ctx, cancel := context.WithCancel(parentCtx)
	return &WSConn{
		ID:       id,
		DeviceID: deviceID,
		Kind:     kind,
		conn:     ws,
		hub:      hub,
		handler:  handler,
		send:     make(chan []byte, sendBufferSize),
		done:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// NextSeq 分配下一个连接级发送序号（L1），从 1 开始。
func (c *WSConn) NextSeq() int64 {
	return c.seq.Add(1)
}

// acceptSeq 对入站帧做连接级去重：丢弃 seq <= lastSeq 的重复/乱序旧帧。
// seq <= 0（老客户端未带序号）宽容放行，不参与去重。
func (c *WSConn) acceptSeq(seq int64) bool {
	if seq <= 0 {
		return true
	}
	for {
		last := c.lastSeq.Load()
		if seq <= last {
			return false
		}
		if c.lastSeq.CompareAndSwap(last, seq) {
			return true
		}
	}
}

// TrySend 非阻塞下发一帧：缓冲满则丢弃并记日志，永不阻塞、永不向已关闭 chan 写（K9）。
func (c *WSConn) TrySend(message protocol.Message) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	data, err := message.Marshal()
	if err != nil {
		slog.Warn("序列化下行帧失败", "conn_id", c.ID, "type", message.Type, "err", err)
		return false
	}
	select {
	case <-c.done:
		return false
	case c.send <- data:
		return true
	default:
		slog.Warn("WS 发送缓冲已满，丢弃下行帧",
			"conn_id", c.ID, "kind", string(c.Kind), "type", message.Type)
		return false
	}
}

// Start 启动读写双泵；调用顺序为 Hub.Register 之后（§7.1 握手顺序）。
func (c *WSConn) Start() {
	go c.writePump()
	go c.readPump()
}

// readPump 读帧 → 校验 → 去重 → 分派；异常或 ctx 结束时关闭连接。
func (c *WSConn) readPump() {
	defer c.Close()
	c.conn.SetReadLimit(protocol.WSReadLimitBytes)
	if err := c.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		return
	}
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		select {
		case <-c.done:
			return
		case <-c.ctx.Done():
			return
		default:
		}
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Warn("WS 连接异常关闭", "conn_id", c.ID, "kind", string(c.Kind), "err", err)
			}
			return
		}
		var message protocol.Message
		if err := message.Decode(data); err != nil {
			slog.Warn("丢弃非法 WS 帧", "conn_id", c.ID, "err", err)
			continue
		}
		if !protocol.IsValidFrameType(message.Type) {
			slog.Warn("丢弃未知 WS 帧类型", "conn_id", c.ID, "type", message.Type)
			continue
		}
		if !c.acceptSeq(message.Seq) {
			slog.Debug("丢弃重复/乱序 WS 帧", "conn_id", c.ID, "type", message.Type, "seq", message.Seq)
			continue
		}
		if c.handler != nil {
			c.handler(c.ctx, c, message)
		}
	}
}

// writePump 是唯一的底层写 goroutine：从 send 取帧写出，并周期性发 Ping 保活。
func (c *WSConn) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Close()
	}()
	for {
		select {
		case <-c.done:
			return
		case <-c.ctx.Done():
			return
		case data := <-c.send:
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Close 幂等关闭连接：只关 done + 底层 conn，不关 send（避免写端 panic），并从 Hub 注销。
func (c *WSConn) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.cancel()
		_ = c.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
			time.Now().Add(writeWait),
		)
		_ = c.conn.Close()
		if c.hub != nil {
			c.hub.Unregister(c)
		}
	})
}

// Context 返回该连接生命周期内的 ctx（供上层在必要时派生子 ctx）。
func (c *WSConn) Context() context.Context {
	return c.ctx
}
