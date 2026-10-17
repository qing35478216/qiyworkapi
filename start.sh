#!/bin/bash
# 启动 workbuddy2api 服务（前台运行；守护用 run.sh）
cd "$(dirname "$0")" || exit 1

export TZ=Asia/Shanghai

mkdir -p auths data logs

if [ ! -f config.json ]; then
  cp config.example.json config.json
  echo "[start] 已生成 config.json，请确认 api_key"
fi

exec ./wb2api -config config.json
