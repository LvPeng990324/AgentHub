#!/usr/bin/env bash
# 交付前置校验（D-10）：确保 web/dist 是「真实 SPA 产物」而不是占位页。
#
# 背景：`make placeholder` 只替换 index.html、**不会删除 assets/**，所以仅检查
# 「assets 下存在 *.js」无法拦截「先 make placeholder 再编 bin/」这种坏顺序；
# 必须同时确认 index.html 引用了 /assets/index-*.js 且该文件真实存在。
# 任一不满足 → 立即 exit 1，避免产出内嵌占位页的坏二进制（R-02 静默失效）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

fail() {
  echo "[check-web] 错误：前端产物缺失：请先执行 \`make web\` 再编译，否则单二进制会内嵌占位页" >&2
  if [ -n "${1:-}" ]; then
    echo "[check-web] 详情：$1" >&2
  fi
  exit 1
}

INDEX="web/dist/index.html"
[ -f "$INDEX" ] || fail "缺少 $INDEX"

REF="$(grep -o '/assets/index-[^"]*\.js' "$INDEX" 2>/dev/null | head -1 || true)"
[ -n "$REF" ] || fail "index.html 未引用 /assets/index-*.js（当前仍是占位页）"

JS="web/dist${REF}"
[ -f "$JS" ] || fail "index.html 引用的 $REF 在磁盘上不存在"

echo "[check-web] PASS: 前端产物就绪（$(basename "$REF")）"
