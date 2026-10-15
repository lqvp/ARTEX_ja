#!/bin/sh
# ARTEX daemon start script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       Run in the foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   Run persistently in the background
#   ./start.sh -addr :9000           Extra arguments are passed through to artex verbatim
#
# It does exactly one thing: run artex, and decide whether to pull it back up based on
# the exit code after the process exits.
#
#   0       user stopped normally        → exit the loop
#   75      program requested restart    → rerun immediately (page clicked "one-click update" or "roll back")
#   other   crash                       → rerun after backoff (1→2→4… up to 60 seconds)
#
# Deliberately does NOT do downloading, SHA256 verification, or swapping here: those
# logics must be written twice for sh and bat, and they are precisely the part that must
# never go wrong — once a non-runnable binary is swapped in, this script will faithfully
# pull it back up over and over, and the user can only rescue it manually on the machine.
# So verification/swapping stays entirely in Go (the selfupdate package), completed by
# artex itself at startup; the script stays foolproof.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] executable not found: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward stop signals to the artex binary.
#
# This is required under Docker: docker stop only sends SIGTERM to PID 1 (i.e. this
# script), not to child processes. Without forwarding, artex receives no signal, cannot
# do a graceful shutdown, and gets SIGKILLed after 10 seconds, cutting running tasks off
# mid-way.
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
	child=$?

	# Signals interrupt wait and make it return >128. At that point the child is actually
	# still doing a graceful shutdown, so we must wait again to get its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			# Update/rollback is ready: after rerun artex completes the swap at startup (see selfupdate.Bootstrap).
			echo "[artex] restart requested (apply new version)…"
			delay=1
			;;
		*)
			echo "[artex] abnormal exit (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
