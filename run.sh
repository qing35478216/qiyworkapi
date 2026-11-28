#!/bin/bash
# qiyworkapi 服务管理（launchd 单一管理者）
#
# 为什么不再 nohup 裸起：本机已有 LaunchAgent cn.qiyworkapi（KeepAlive=true）。
# 脚本自己 nohup 一个实例会占住 :7863，launchd 拉起的实例全部 bind 失败退出，
# 形成"两个管理者互相打架 + 刷失败日志"的局面（2026-11-28 实际踩到，runs=19 全 exit 1）。
# 现在统一走 launchctl，脚本只当开关，不持有进程。
#
# 用法: ./run.sh [start|stop|restart|status|logs|install|uninstall|build]
#   start      未运行则启动；已在运行且健康则幂等退出（不打断会话）
#   restart    杀掉 launchd 进程并原地重启（会短暂中断 :7863）
#   stop       卸载 launchd job（KeepAlive 下唯一可靠的停止方式）
#   status     打印 job 状态 / 二进制 / 端口 / 健康 / 野进程
#   install    注册开机自启（登录即启动、崩溃自动拉起）
#   build      编译到真实运行路径（先写 .new 再 mv，不动运行中的 inode）
#
set -u
cd "$(dirname "$0")" || exit 0
REPO=$PWD
LABEL=cn.qiyworkapi
DOMAIN="gui/$(id -u)"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
PORT=7863
LOG_FILE="$REPO/logs/server.log"
PID_FILE="$REPO/data/qiyworkapi.pid"

# 真实运行路径优先（仓库内旧构建已于 2026-11-28 清理，这里只留兜底分支）
BIN=${QIYWORKAPI_BIN:-/Users/qiy/workapi/qiyworkapi}
[ -x "$BIN" ] || BIN="$REPO/qiyworkapi"

mkdir -p "$REPO/data" "$REPO/logs" "$(dirname "$PLIST")"

say() { echo "[$(date '+%H:%M:%S')] $*"; }

# ---- launchd 查询 ----------------------------------------------------------
job_loaded() { launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; }
job_field()  { launchctl print "$DOMAIN/$LABEL" 2>/dev/null | sed -n "s/^[[:space:]]*$1 = //p" | head -1; }
job_pid()    { job_field pid; }
job_state()  { job_field state; }

# ---- 健康检查 --------------------------------------------------------------
api_key() {
  python3 -c "import json;print(json.load(open('config.json')).get('api_key',''))" 2>/dev/null
}
health() {
  local key code
  key=$(api_key)
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 \
    -H "Authorization: Bearer $key" "http://127.0.0.1:$PORT/v1/models" 2>/dev/null)
  echo "${code:-000}"
}

# ---- 野进程（非 launchd 拉起的同款实例）------------------------------------
strays() {
  local jp p
  jp=$(job_pid)
  pgrep -f "qiyworkapi -config config.json" 2>/dev/null | while read -r p; do
    [ "$p" = "$jp" ] || echo "$p"
  done
}
kill_strays() {
  local p found=0
  for p in $(strays); do
    found=1
    say "清理野进程 PID=$p（父进程 $(ps -o ppid= -p "$p" 2>/dev/null | tr -d ' ')）"
    kill "$p" 2>/dev/null
  done
  [ "$found" -eq 1 ] && sleep 1
  return 0
}

# ---- plist 生成（内容不一致才覆盖，覆盖前留备份）----------------------------
plist_body() {
  cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>$HOME</string>
		<key>PATH</key>
		<string>/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
		<key>TZ</key>
		<string>Asia/Shanghai</string>
	</dict>
	<key>KeepAlive</key>
	<true/>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>export TZ=Asia/Shanghai; cd $REPO &amp;&amp; exec $BIN -config config.json &gt;&gt; logs/server.log 2&gt;&amp;1</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>WorkingDirectory</key>
	<string>$REPO</string>
</dict>
</plist>
EOF
}
ensure_plist() {
  local want bak
  want=$(plist_body)
  if [ -f "$PLIST" ] && [ "$(cat "$PLIST")" = "$want" ]; then
    return 0
  fi
  if [ -f "$PLIST" ]; then
    bak="$PLIST.bak.$(date '+%Y%m%d%H%M%S')"
    cp "$PLIST" "$bak" && say "旧 plist 已备份: $bak"
    job_loaded && { launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null; sleep 1; }
  fi
  printf '%s\n' "$want" >"$PLIST"
  say "已写入 $PLIST（BIN=$BIN）"
  return 0
}

# ---- 等就绪 ----------------------------------------------------------------
wait_ready() {
  local i c p
  for i in $(seq 1 40); do
    sleep 0.5
    c=$(health)
    if [ "$c" = "200" ]; then
      p=$(job_pid)
      [ -n "$p" ] && printf '%s\n' "$p" >"$PID_FILE"
      say "就绪: /v1/models -> 200，launchd pid=${p:-?}"
      return 0
    fi
    job_loaded || { say "FAIL: launchd job 已消失，看 $LOG_FILE"; return 1; }
  done
  say "WARN: 健康检查未通过（末次 $c），进程可能仍在启动，看 $LOG_FILE"
  return 1
}

# ---- 子命令 ----------------------------------------------------------------
cmd_start() {
  if [ ! -x "$BIN" ]; then
    say "FAIL: 二进制不存在（$BIN），先跑 ./run.sh build"
    return 1
  fi
  if [ "$(job_state)" = "running" ] && [ "$(health)" = "200" ]; then
    say "已在运行（launchd pid=$(job_pid)），无需操作"
    return 0
  fi
  kill_strays
  ensure_plist
  if job_loaded; then
    launchctl kickstart -k "$DOMAIN/$LABEL" || return 1
    say "已 kickstart -k $LABEL"
  else
    launchctl bootstrap "$DOMAIN" "$PLIST" || return 1
    say "已 bootstrap $LABEL"
  fi
  wait_ready
}

cmd_stop() {
  if job_loaded; then
    launchctl bootout "$DOMAIN/$LABEL" && say "已卸载 launchd job（不会再自动拉起）"
  else
    say "launchd job 未加载"
  fi
  kill_strays
  rm -f "$PID_FILE"
  return 0
}

cmd_restart() {
  say "重启会中断 :$PORT 上的在途请求（含 IDE 会话的模型调用）"
  if job_loaded; then
    kill_strays
    if ! launchctl kickstart -k "$DOMAIN/$LABEL"; then
      say "kickstart 失败，回退到 bootstrap"
      ensure_plist
      launchctl bootstrap "$DOMAIN" "$PLIST" || return 1
    fi
  else
    ensure_plist
    launchctl bootstrap "$DOMAIN" "$PLIST" || return 1
  fi
  wait_ready
}

cmd_status() {
  local st p s last listen
  st=$(job_state); p=$(job_pid)
  last=$(job_field 'last exit code')
  printf 'launchd   %s\n' "${st:-未加载}${p:+ (pid=$p)}"
  printf 'runs      %s\n' "$(job_field runs)"
  printf '退出码     %s\n' "${last:-运行中，无退出记录}"
  if [ -x "$BIN" ]; then
    printf '二进制     %s  %s\n' "$BIN" "$(stat -f '%Sm' -t '%Y-%m-%d %H:%M' "$BIN" 2>/dev/null)"
  else
    printf '二进制     缺失（%s）\n' "$BIN"
  fi
  listen=$(lsof -nP -iTCP:$PORT -sTCP:LISTEN 2>/dev/null | awk 'NR==2{printf "%s pid=%s", $(NF-1), $2}')
  printf '端口       %s\n' "${listen:-无监听}"
  printf '健康       /v1/models -> %s\n' "$(health)"
  s=$(strays | tr '\n' ' ')
  printf '野进程     %s\n' "${s:-无}"
  printf '面板       http://127.0.0.1:%s/panel/\n' "$PORT"
}

cmd_install() {
  ensure_plist
  job_loaded && { launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null; sleep 1; }
  launchctl bootstrap "$DOMAIN" "$PLIST" || return 1
  say "已注册开机自启 $LABEL"
  wait_ready
}

cmd_uninstall() {
  cmd_stop
  if [ -f "$PLIST" ]; then
    mv "$PLIST" "$PLIST.disabled.$(date '+%Y%m%d%H%M%S')" && say "plist 已改名停用（未删除，可回滚）"
  fi
  return 0
}

cmd_build() {
  say "编译: go build -trimpath -o $BIN"
  ( cd "$REPO" && go build -trimpath -o "$BIN.new" ./cmd/server ) || {
    say "FAIL: 编译失败，运行中的二进制未改动"
    rm -f "$BIN.new"
    return 1
  }
  mv -f "$BIN.new" "$BIN" || return 1
  say "已更新二进制，重启生效: ./run.sh restart"
}

case "${1:-start}" in
  start)     cmd_start ;;
  stop)      cmd_stop ;;
  restart)   cmd_restart ;;
  status)    cmd_status ;;
  logs)      tail -f "$LOG_FILE" ;;
  install)   cmd_install ;;
  uninstall) cmd_uninstall ;;
  build)     cmd_build ;;
  *)         echo "用法: ./run.sh [start|stop|restart|status|logs|install|uninstall|build]"; exit 2 ;;
esac
