package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// ---------- 统一响应写入 ----------

// writeEnvelope 以 application/json; charset=utf-8 写出响应包络。
func writeEnvelope(w http.ResponseWriter, envelope Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(protocol.HTTPStatus(envelope.Code))
	if err := json.NewEncoder(w).Encode(envelope); err != nil {
		slog.Error("写出 REST 响应失败", "err", err)
	}
}

// writeOK 写成功响应（HTTP 恒为 200）。
func writeOK(w http.ResponseWriter, data any) {
	writeEnvelope(w, OK(data))
}

// writeFail 按错误码写失败响应（data 恒为 null）。
func writeFail(w http.ResponseWriter, code int) {
	writeEnvelope(w, Fail(code))
}

// writeFailMessage 写失败响应并覆盖中文文案。
func writeFailMessage(w http.ResponseWriter, code int, message string) {
	writeEnvelope(w, FailMessage(code, message))
}

// ---------- 中间件 ----------

type middleware func(http.Handler) http.Handler

// chain 按「从外到内」顺序包装中间件。
func chain(handler http.Handler, middlewares ...middleware) http.Handler {
	for index := len(middlewares) - 1; index >= 0; index-- {
		handler = middlewares[index](handler)
	}
	return handler
}

// statusRecorder 记录响应状态码，供请求日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

// Hijack 转发底层 Hijacker 能力。gorilla/websocket 的 Upgrade 依赖 http.Hijacker，
// 若包装 ResponseWriter 时丢失该接口，两条 WS 通道会握手失败（HTTP 500）。
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("底层 ResponseWriter 不支持 http.Hijacker")
	}
	return hijacker.Hijack()
}

// Flush 转发 Flush 能力，避免包装后影响流式响应。
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// withRecover 捕获 panic，统一返回 5000 内部错误；日志只记错误本身，绝不打印请求体（含密码/prompt）。
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("HTTP 处理发生 panic",
					"method", r.Method,
					"path", r.URL.Path, // 脱敏：只打 path，不打 RawQuery（WS 的 ?token= 不能落日志）
					"panic", fmt.Sprint(recovered),
				)
				writeFail(w, protocol.CodeInternalError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withRequestLog 输出结构化 JSON 请求日志（K7）：只打 method / path / status / 耗时，
// 不打印完整 URL、Authorization 头、请求体（密码 / prompt / token 一律不落日志）。
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		slog.Info("http_request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// withCORS 仅对本地开发源（Vite dev server）放行跨域；生产由 nginx 同源反代。
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if isLocalDevOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions && origin != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLocalDevOrigin 判断是否为 localhost / 127.0.0.1 的开发源。
func isLocalDevOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	return strings.HasPrefix(origin, "http://localhost:") ||
		strings.HasPrefix(origin, "http://127.0.0.1:") ||
		strings.HasPrefix(origin, "https://localhost:") ||
		strings.HasPrefix(origin, "https://127.0.0.1:")
}

// ---------- 鉴权中间件 ----------

type contextKey string

const (
	ctxKeyToken     contextKey = "auth_token"
	ctxKeyExpiresAt contextKey = "auth_expires_at"
)

// requireAuth 校验 Authorization: Bearer <登录token>。
// 设备令牌永远不能通过本中间件（D-2：与登录密码 / 登录 token 彻底分离）。
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeFail(w, protocol.CodeUnauthorized)
			return
		}
		expiresAt, code := s.auth.Validate(r.Context(), token)
		if code != protocol.CodeOK {
			writeFail(w, code)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyToken, token)
		ctx = context.WithValue(ctx, ctxKeyExpiresAt, expiresAt)
		next(w, r.WithContext(ctx))
	}
}

// bearerToken 从 Authorization 头提取 Bearer token；格式不符返回空串。
func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(header) < 7 || !strings.EqualFold(header[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(header[7:])
}

// tokenFromContext 取出鉴权中间件注入的 token 与过期时间。
func tokenFromContext(ctx context.Context) (string, string, bool) {
	token, okToken := ctx.Value(ctxKeyToken).(string)
	expiresAt, okExpiry := ctx.Value(ctxKeyExpiresAt).(string)
	return token, expiresAt, okToken && okExpiry && token != ""
}

// ---------- 请求体解析 ----------

// maxRequestBodyBytes 是 REST 请求体上限（1 MiB），远大于业务报文。
const maxRequestBodyBytes = 1 << 20

// decodeJSON 解析 JSON 请求体；空体 / 非法 JSON / 多值 JSON 一律返回 error。
func decodeJSON(r *http.Request, target any) error {
	if target == nil {
		return errors.New("请求体接收者为空")
	}
	reader := io.LimitReader(r.Body, maxRequestBodyBytes+1)
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("解析请求体失败: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("请求体包含多余的 JSON 值")
	}
	return nil
}

// maskToken 把 token 脱敏为「前 6 位 + ***」，供日志使用（K7）。
func maskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 6 {
		return "***"
	}
	return token[:6] + "***"
}
