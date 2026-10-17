#!/bin/bash
# 后台常驻启动：./run.sh [start|stop|restart|status|logs]
cd "$(dirname "$0")" || exit 1

PID_FILE="$PWD/data/qiyworkapi.pid"
LOG_FILE="$PWD/logs/server.log"
mkdir -p data logs

is_running() {
  [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null
}

start() {
  if is_running; then
    echo "已在运行，PID=$(cat "$PID_FILE")"
    return 0
  fi
  export TZ=Asia/Shanghai
  nohup ./qiyworkapi -config config.json >>"$LOG_FILE" 2>&1 &
  echo $! >"$PID_FILE"
  sleep 1
  echo "已启动，PID=$(cat "$PID_FILE")，日志: $LOG_FILE"
  echo "面板: http://127.0.0.1:7863/panel/"
}

stop() {
  if is_running; then
    kill "$(cat "$PID_FILE")" && echo "已停止 PID=$(cat "$PID_FILE")"
  else
    echo "未在运行"
  fi
  /bin/rm -f "$PID_FILE"
}

case "$1" in
  stop)    stop ;;
  restart) stop; sleep 1; start ;;
  status)  is_running && echo "运行中 PID=$(cat "$PID_FILE")" || echo "未运行" ;;
  logs)    tail -f "$LOG_FILE" ;;
  *)       start ;;
esac
