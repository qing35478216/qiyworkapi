#!/bin/bash
# 前台运行 qiyworkapi（调试用；常驻/重启请用 ./run.sh）
# 注意：本机 :7863 由 launchd 的 cn.qiyworkapi 常驻占用，前台跑之前先 ./run.sh stop
set -u
cd "$(dirname "$0")" || exit 1

export TZ=Asia/Shanghai
mkdir -p auths data logs

# 真实运行路径优先（仓库内旧构建已于 2026-11-28 清理）
BIN=${QIYWORKAPI_BIN:-/Users/qiy/workapi/qiyworkapi}
[ -x "$BIN" ] || BIN="$PWD/qiyworkapi"
[ -x "$BIN" ] || { echo "[start] 找不到二进制：$BIN（先 ./run.sh build）"; exit 1; }

if [ ! -f config.json ] && [ -f config.example.json ]; then
  cp config.example.json config.json
  echo "[start] 已生成 config.json，请确认 api_key"
fi

if lsof -nP -iTCP:7863 -sTCP:LISTEN >/dev/null 2>&1; then
  echo "[start] 7863 已被占用（launchd 常驻服务在跑）：先 ./run.sh stop，或直接用 ./run.sh status"
  exit 1
fi

echo "[start] 前台运行 $BIN（Ctrl-C 退出）"
exec "$BIN" -config config.json
