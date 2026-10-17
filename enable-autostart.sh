#!/bin/bash
# 在【你自己的终端】里执行本脚本，注册 launchd 用户级常驻服务（登录即启动、崩溃自动拉起）
# 用法： ./enable-autostart.sh [start|stop|status]
# 注意：不要在受限沙箱/IDE 内置终端里跑，launchctl 需要真实 GUI 会话权限

LABEL="cn.qiyworkapi"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
DOMAIN="gui/$(id -u)"

case "${1:-start}" in
  start)
    cp "$PLIST" "$PLIST" 2>/dev/null
    launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null   # 先注销旧的，忽略报错
    launchctl bootstrap "$DOMAIN" "$PLIST" && echo "已注册常驻服务 $LABEL"
    sleep 1
    curl -s http://127.0.0.1:7863/healthz; echo
    echo "面板: http://127.0.0.1:7863/panel/"
    ;;
  stop)
    launchctl bootout "$DOMAIN/$LABEL" && echo "已注销常驻服务 $LABEL"
    ;;
  status)
    launchctl print "$DOMAIN/$LABEL" 2>/dev/null | head -5 || echo "未注册"
    ;;
esac
