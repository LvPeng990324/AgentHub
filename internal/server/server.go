package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/pkg/protocol"
	"github.com/agenthub/agenthub/web"
)

// Server 是平台服务的顶层装配对象：持有配置、存储、三个 Repo、鉴权、Hub、对话编排与 HTTP 路由。
// 所有业务层组件共享 main 传入的根 ctx（K8），不使用 context.Background()。
type Server struct {
	cfg       *config.ServerConfig
	version   string
	startedAt time.Time
	rootCtx   context.Context // main 透传的根 ctx，用于派生每条 WS 连接的 ctx（K8）

	store       *Store
	deviceRepo  *DeviceRepo
	agentRepo   *AgentRepo
	messageRepo *MessageRepo
	auth        *AuthService
	hub         *Hub
	chat        *ChatOrchestrator

	mux        *http.ServeMux
	handler    http.Handler
	httpServer *http.Server
}

// New 完成全部装配：开库迁移 → Repo → Auth → Hub → Chat → 路由 → 启动兜底中断残留流式消息。
func New(ctx context.Context, cfg *config.ServerConfig, version string) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("平台配置不能为空")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	store, err := OpenStore(ctx, cfg.DBPath)
	if err != nil {
		return nil, err
	}

	deviceRepo := NewDeviceRepo(store)
	agentRepo := NewAgentRepo(store)
	messageRepo := NewMessageRepo(store)
	authService := NewAuthService(store, cfg)
	hub := NewHub(cfg, store, deviceRepo, agentRepo, messageRepo)
	chat := NewChatOrchestrator(store, deviceRepo, agentRepo, messageRepo, hub)
	hub.AttachChat(chat)

	srv := &Server{
		cfg:         cfg,
		version:     version,
		startedAt:   time.Now().UTC(),
		rootCtx:     ctx,
		store:       store,
		deviceRepo:  deviceRepo,
		agentRepo:   agentRepo,
		messageRepo: messageRepo,
		auth:        authService,
		hub:         hub,
		chat:        chat,
		mux:         http.NewServeMux(),
	}
	if version == "" {
		srv.version = "0.1.0"
	}
	srv.registerRoutes()
	srv.handler = chain(srv.mux, srv.withRecover, srv.withRequestLog, srv.withCORS)
	srv.httpServer = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 启动时清理过期登录 token；并把上次进程遗留的 streaming 占位消息兜底为 interrupted，
	// 避免半截气泡永久悬挂（§7.5 / 验收要点）。
	if err := authService.PruneExpired(ctx); err != nil {
		slog.Warn("清理过期登录 token 失败", "err", err)
	}
	if affected, err := chat.InterruptStaleStreaming(ctx, ""); err != nil {
		slog.Warn("启动时兜底流式消息失败", "err", err)
	} else if affected > 0 {
		slog.Info("启动时已兜底残留流式消息", "count", affected)
	}
	return srv, nil
}

// registerRoutes 注册 E01~E15 与两条 WS 通道；Go 1.22+ ServeMux 原生支持方法与路径参数。
func (s *Server) registerRoutes() {
	mux := s.mux

	// 公开端点（白名单）
	mux.HandleFunc("GET /api/v1/meta", s.handleMeta)
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)

	// 登录态端点：Authorization: Bearer <登录token>
	mux.HandleFunc("POST /api/v1/auth/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("GET /api/v1/auth/me", s.requireAuth(s.handleMe))

	mux.HandleFunc("GET /api/v1/devices", s.requireAuth(s.handleListDevices))
	mux.HandleFunc("PATCH /api/v1/devices/{deviceId}/remark", s.requireAuth(s.handleUpdateDeviceRemark))

	mux.HandleFunc("GET /api/v1/agents/discovered", s.requireAuth(s.handleListDiscovered))
	mux.HandleFunc("GET /api/v1/agents", s.requireAuth(s.handleListRegistered))
	mux.HandleFunc("POST /api/v1/agents/batch-register", s.requireAuth(s.handleBatchRegister))
	mux.HandleFunc("POST /api/v1/agents/{agentId}/register", s.requireAuth(s.handleRegister))
	mux.HandleFunc("DELETE /api/v1/agents/{agentId}/register", s.requireAuth(s.handleUnregister))
	mux.HandleFunc("PATCH /api/v1/agents/{agentId}/remark", s.requireAuth(s.handleUpdateAgentRemark))
	mux.HandleFunc("GET /api/v1/agents/{agentId}/messages", s.requireAuth(s.handleMessages))
	mux.HandleFunc("GET /api/v1/agents/{agentId}/session", s.requireAuth(s.handleSession))

	// 两条独立 WS 通道：握手鉴权统一用 ?token= （浏览器不能自定义 header）
	mux.HandleFunc("GET /api/v1/ws/adapter", s.handleWSAdapter)
	mux.HandleFunc("GET /api/v1/ws/client", s.handleWSClient)

	// 内嵌前端 + SPA fallback
	mux.Handle("/", s.staticHandler())
}

// Handler 返回包装好中间件的 HTTP Handler，供 httptest 与集成测试直接挂载。
func (s *Server) Handler() http.Handler {
	return s.handler
}

// Run 启动 Hub 后台扫描与 HTTP 服务，阻塞直到 ctx 取消或监听失败，然后优雅关闭。
func (s *Server) Run(ctx context.Context) error {
	s.hub.Run(ctx)

	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", s.cfg.ListenAddr, err)
	}
	slog.Info("平台服务已启动",
		"listen", listener.Addr().String(),
		"domain", s.cfg.Domain,
		"db_path", s.cfg.DBPath,
		"version", s.version,
	)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.httpServer.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	case <-ctx.Done():
		// context.WithoutCancel 保留 ctx 中的值但去掉取消信号，使 5s 关闭窗口有效（K8）。
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return s.Shutdown(shutdownCtx)
	}
}

// Shutdown 依次停止 HTTP、关闭全部 WS 连接、关闭数据库。
func (s *Server) Shutdown(ctx context.Context) error {
	var firstErr error
	if err := s.httpServer.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		firstErr = err
	}
	s.hub.CloseAll()
	if err := s.store.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		return fmt.Errorf("平台服务关闭出错: %w", firstErr)
	}
	return nil
}

// staticHandler 托管 web/dist（真实 Vite 产物或占位页），非 /api 路径统一 SPA fallback 到 index.html。
func (s *Server) staticHandler() http.Handler {
	dist, err := web.Dist()
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeFail(w, protocol.CodeInternalError)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// 未知 API 路径也必须走统一包络（K3），HTTP 仍为 404。
			writeFail(w, protocol.CodeNotFound)
			return
		}
		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "" || clean == "." {
			clean = "index.html"
		}
		if _, statErr := fs.Stat(dist, clean); statErr != nil {
			clean = "index.html" // SPA 路由（/devices、/chat/:id 等）
		}
		// 带内容 hash 的 /assets/* 可长缓存；index.html 必须 no-cache，保证发版即时生效。
		if strings.HasPrefix(clean, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else if clean == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.ServeFileFS(w, r, dist, clean)
	})
}
