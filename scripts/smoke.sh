#!/usr/bin/env bash
# =============================================================================
# AgentHub · 端到端冒烟（T05 / B5.4）
#
# 唯一入口，Windows Git Bash / Linux bash 均可运行。
# 真实跑通 1 台设备 + 1 个假 CLI 的：
#   启动 → 设备注册 → agent 注册 → WS 发消息/流式回复/落库 → 会话续接 → 取消注册清空
# 每步输出中文日志 + PASS/FAIL；失败非零退出并用 trap 清理进程与临时目录。
# 只依赖 go / curl / bash 基础命令，不需要 jq / python。
# =============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

TMP="$(mktemp -d)"
# Go 程序与配置文件里必须用 Windows 原生路径；shell 内部仍用 POSIX 路径。
WTMP="$(cygpath -w "$TMP" 2>/dev/null || printf '%s' "$TMP")"
EXE=""
if [ "$(go env GOOS)" = "windows" ]; then EXE=".exe"; fi

PORT=$(( 20000 + ($$ % 20000) ))
PASSWORD="smoke-password-$$"
DEVICE_TOKEN="smoke-device-token-$$"
SESSION_ID="ses-fake-smoke-$$"
MARKER="$WTMP/resume.marker"
BASE="http://127.0.0.1:${PORT}/api/v1"
SERVER_PID=""
ADAPTER_PID=""

step() { printf '\n=== [smoke] %s ===\n' "$1"; }
pass() { printf '  PASS: %s\n' "$1"; }
fail() { printf '  FAIL: %s\n' "$1" >&2; exit 1; }

cleanup() {
  if [ -n "$ADAPTER_PID" ]; then kill "$ADAPTER_PID" 2>/dev/null || true; fi
  if [ -n "$SERVER_PID" ]; then kill "$SERVER_PID" 2>/dev/null || true; fi
  wait 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
step "0/8 编译本机 server / adapter / 冒烟辅助程序（CGO_ENABLED=0）"
# 说明：bin/ 下是交叉编译产物（Linux 版无法在 Windows 本机执行），
#       冒烟脚本编译本机平台二进制；make build 的 5 个产物由独立验收步骤验证。
CGO_ENABLED=0 go build -o "$TMP/server$EXE" ./cmd/server
CGO_ENABLED=0 go build -o "$TMP/adapter$EXE" ./cmd/adapter
CGO_ENABLED=0 go build -o "$TMP/fake-cli$EXE" ./scripts/smoke
CGO_ENABLED=0 go build -o "$TMP/smoke-helper$EXE" ./scripts/smoke
"$TMP/smoke-helper$EXE" genconfig \
  -tmp "$WTMP" -port "$PORT" -fake "$WTMP/fake-cli$EXE" \
  -password "$PASSWORD" -token "$DEVICE_TOKEN"

# ---------------------------------------------------------------------------
step "1/8 启动真实平台服务（随机端口 ${PORT}）"
"$TMP/server$EXE" -config "$WTMP/server.json" > "$TMP/server.log" 2>&1 &
SERVER_PID=$!
READY=0
for _ in $(seq 1 40); do
  if curl -sf "$BASE/health" > /dev/null 2>&1; then READY=1; break; fi
  sleep 0.3
done
[ "$READY" = "1" ] || { cat "$TMP/server.log"; fail "平台服务未就绪"; }
curl -sf "$BASE/health" | grep -q '"db":"ok"' || fail "健康检查未返回 db:ok"
pass "平台服务就绪，SQLite 自动建库"

# ---------------------------------------------------------------------------
step "2/8 启动真实适配器（agent_paths 指向假 CLI，PATH 收敛避免命中本机真实 CLI）"
PATH="$TMP" AGENTHUB_SMOKE_MARKER="$MARKER" AGENTHUB_SMOKE_SESSION_ID="$SESSION_ID" \
  "$TMP/adapter$EXE" -config "$WTMP/adapter.json" > "$TMP/adapter.log" 2>&1 &
ADAPTER_PID=$!
pass "适配器已启动（device_id 持久化到临时目录）"

# ---------------------------------------------------------------------------
step "3/8 登录并断言设备与 agent 已上报且在线"
LOGIN="$(curl -sf -X POST -H "Content-Type: application/json" \
  -d "{\"password\":\"$PASSWORD\"}" "$BASE/auth/login")"
TOKEN="$(printf '%s' "$LOGIN" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || fail "登录未返回 token"
DEVICES=""
for _ in $(seq 1 40); do
  DEVICES="$(curl -sf -H "Authorization: Bearer $TOKEN" "$BASE/devices")"
  if printf '%s' "$DEVICES" | grep -q ':claude'; then break; fi
  sleep 0.5
done
printf '%s' "$DEVICES" | grep -q '"online":true' || { cat "$TMP/adapter.log"; fail "设备未在线"; }
printf '%s' "$DEVICES" | grep -q ':claude' || { cat "$TMP/adapter.log"; fail "假 CLI agent 未上报"; }
AGENT_ID="$(printf '%s' "$DEVICES" | grep -o '"id":"[^"]*"' \
  | sed 's/"id":"//; s/"$//' | grep ':claude$' | head -1)"
[ -n "$AGENT_ID" ] || fail "无法解析 agent_id"
ENC_AGENT_ID="$(printf '%s' "$AGENT_ID" | sed 's/:/%3A/g')"
pass "设备 + agent 已上报且在线: ${AGENT_ID}"

# ---------------------------------------------------------------------------
step "4/8 注册 agent 并断言列表四要素"
REGISTER="$(curl -sf -X POST -H "Authorization: Bearer $TOKEN" \
  "$BASE/agents/$ENC_AGENT_ID/register")"
printf '%s' "$REGISTER" | grep -q '"registered":true' || fail "注册失败: $REGISTER"
AGENTS="$(curl -sf -H "Authorization: Bearer $TOKEN" "$BASE/agents")"
printf '%s' "$AGENTS" | grep -q '"display_name"' || fail "列表缺少类型/展示名"
printf '%s' "$AGENTS" | grep -q '"device_name"' || fail "列表缺少所属设备"
printf '%s' "$AGENTS" | grep -q '"online":true' || fail "列表缺少在线状态"
pass "已注册，列表含名称 / 类型 / 在线 / 所属设备四要素"

# ---------------------------------------------------------------------------
step "5/8 WS 全链路：chat.send → send_ack → chat.chunk → chat.done → 落库"
step "6/8 会话续接：第二轮 chat.send 带 resume_id"
"$TMP/smoke-helper$EXE" ws \
  -base "http://127.0.0.1:${PORT}" \
  -password "$PASSWORD" \
  -agent "$AGENT_ID" \
  -marker "$MARKER"

# ---------------------------------------------------------------------------
step "7/8 取消注册：清空会话并校验 D-6 铁律"
DELETED="$(curl -sf -X DELETE -H "Authorization: Bearer $TOKEN" \
  "$BASE/agents/$ENC_AGENT_ID/register")"
printf '%s' "$DELETED" | grep -q '"messages_cleared":true' || fail "未返回 messages_cleared:true"
pass "取消注册返回 messages_cleared:true"

# 按 E14 契约：未注册 agent 取历史返回 3002（会话已不可读取）。
UNREG_MSGS="$(curl -s -H "Authorization: Bearer $TOKEN" "$BASE/agents/$ENC_AGENT_ID/messages")"
printf '%s' "$UNREG_MSGS" | grep -q '"code":3002' || fail "取消注册后历史消息仍可读取: $UNREG_MSGS"
pass "取消注册后历史消息不可再读取（3002）"

# 必须在「重新注册」之前断言未注册态。
DISCOVERED="$(curl -sf -H "Authorization: Bearer $TOKEN" "$BASE/agents/discovered")"
printf '%s' "$DISCOVERED" | grep -q '"registered":false' || fail "发现页该 agent 未回到未注册态"
pass "发现页该 agent 回到未注册态，之后可重新注册"

# 重新注册会新建会话 → 历史为空，直接证明旧会话数据已被级联清空。
curl -sf -X POST -H "Authorization: Bearer $TOKEN" \
  "$BASE/agents/$ENC_AGENT_ID/register" > /dev/null
FRESH_MSGS="$(curl -sf -H "Authorization: Bearer $TOKEN" "$BASE/agents/$ENC_AGENT_ID/messages")"
printf '%s' "$FRESH_MSGS" | grep -q '"items":\[\]' || fail "重新注册后历史不为空: $FRESH_MSGS"
pass "重新注册新建会话，历史消息为空（旧会话已清空）"

AFTER_DEVICES="$(curl -sf -H "Authorization: Bearer $TOKEN" "$BASE/devices")"
printf '%s' "$AFTER_DEVICES" | grep -q ':claude' || fail "取消注册误删了 agent 行"
printf '%s' "$AFTER_DEVICES" | grep -q '"online":true' || fail "取消注册影响了设备在线态"
pass "devices 行与 agent 行保留，设备仍在线（不删行、不断 WS）"

# ---------------------------------------------------------------------------
step "8/8 清理进程与临时目录"
printf '\n'
echo "=============================================================="
echo "  SMOKE PASS: 注册 → 对话 → 流式落库 → 续接 → 取消注册清空 全链路通过"
echo "=============================================================="
