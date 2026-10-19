#!/bin/sh
# SERIES 守护启动脚本（Linux / macOS / Docker ENTRYPOINT）
#
# 用法：
#   ./start.sh                       前台运行（Ctrl-C 停止）
#   nohup ./start.sh >series.log 2>&1 &   后台常驻
#   ./start.sh -addr :9000           额外参数原样透传给 series
#
# 它只做一件事：把 series 跑起来，进程退出后按退出码决定要不要再拉起。
#
#   0      用户正常停止        → 退出循环
#   其他   崩溃                → 退避后重跑（1→2→4…最多 60 秒）
#
set -u

cd "$(dirname "$0")" || exit 1

BIN=./series
[ -x "$BIN" ] || { echo "[series] 找不到可执行文件 $BIN" >&2; exit 1; }

MAX_DELAY=60

child=0
stopping=0

# 转发停止信号给 series 本体。
#
# Docker 下这是必需的：docker stop 只把 SIGTERM 发给 PID 1（也就是本脚本），
# 不会发给子进程。不转发的话 series 收不到信号、做不了优雅关闭，10 秒后被 SIGKILL
# 硬杀，正在跑的任务直接断在半路。
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# 信号会打断 wait 并让它返回 >128。此时子进程其实还在做优雅关闭，
	# 必须再 wait 一次才能拿到它真正的退出码。
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[series] 已停止"
		exit 0
	fi

	case "$code" in
		0)
			echo "[series] 正常退出"
			exit 0
			;;
		*)
			echo "[series] 异常退出 (code=$code)，${delay}s 后重启" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
