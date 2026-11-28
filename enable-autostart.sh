#!/bin/bash
# 兼容入口：逻辑已统一到 ./run.sh（launchd 单一管理者），这里只做参数转发
# 用法： ./enable-autostart.sh [start|stop|status]  ==  ./run.sh [install|stop|status]
set -u
DIR=$(cd "$(dirname "$0")" && pwd)

case "${1:-start}" in
  start|install) exec "$DIR/run.sh" install ;;
  stop)          exec "$DIR/run.sh" stop ;;
  status)        exec "$DIR/run.sh" status ;;
  *)             echo "用法: ./enable-autostart.sh [start|stop|status]"; exit 2 ;;
esac
