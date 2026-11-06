#!/usr/bin/env bash
# 恢复 web/dist/index.html 占位页（仓库默认交付态）。
# 独立 UTF-8 脚本：避免 GNU Make 在 Windows 控制台转码导致中文乱码。
# 由 `make placeholder` / `make clean` 调用；交付时也可直接 `bash scripts/restore-placeholder.sh`。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "$ROOT/web/dist"

cat > "$ROOT/web/dist/index.html" <<'HTML'
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover" />
  <title>AgentHub</title>
  <style>
    :root { color-scheme: light; }
    body {
      margin: 0;
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
                   "Microsoft YaHei", "Noto Sans CJK SC", sans-serif;
      background: #f7f8fc;
      color: #1f2430;
    }
    main {
      max-width: 30rem;
      padding: 2rem 2.25rem;
      border: 1px solid #e3e6f0;
      border-radius: 14px;
      background: #fff;
      box-shadow: 0 8px 30px rgba(31, 36, 48, 0.06);
      line-height: 1.7;
    }
    h1 { margin: 0 0 .5rem; font-size: 1.4rem; }
    p { margin: .5rem 0; }
    code {
      padding: .1rem .4rem;
      border-radius: 5px;
      background: #f0f2f9;
      font-size: .9em;
    }
    .hint { color: #6b7280; font-size: .9rem; }
  </style>
</head>
<body>
  <main>
    <h1>AgentHub</h1>
    <p>前端尚未构建，请先执行 <code>make web</code>。</p>
    <p class="hint">当前为预置占位页；平台 REST 与 WebSocket 接口已可正常访问（<code>/api/v1/meta</code>）。</p>
  </main>
</body>
</html>
HTML

echo "  PASS: placeholder restored (web/dist/index.html)"
