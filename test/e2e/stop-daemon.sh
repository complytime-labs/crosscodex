#!/usr/bin/env bash
# Stop the e2e crosscodexd and stub LLM gateway tracked by pidfiles in E2E_DIR.
# Safe when no pidfile exists. Callers: run-daemon.sh (pre-start cleanup) and
# the test:e2e:venom* teardown defers, which need the daemon gone before
# `compose down` removes Postgres under it. Returns only after the process has
# exited (TERM, up to 5s, then KILL, up to 5s).
set -euo pipefail

E2E_DIR="${1:?Missing argument: e2e directory}"

# kill -0 succeeds on a zombie, and an orphan may never be reaped in a
# container whose PID 1 doesn't reap, so a zombie counts as stopped.
alive() {
	local stat
	stat=$(ps -o stat= -p "$1" 2>/dev/null) || return 1
	[[ "$stat" != Z* ]]
}

wait_gone() {
	local pid="$1"
	for _ in $(seq 1 50); do
		alive "$pid" || return 0
		sleep 0.1
	done
	return 1
}

if [[ -f "$E2E_DIR/daemon.pid" ]]; then
	PID=$(cat "$E2E_DIR/daemon.pid")
	if alive "$PID"; then
		echo "Stopping crosscodexd (PID $PID)..."
		kill -TERM "$PID" 2>/dev/null || true
		if ! wait_gone "$PID"; then
			echo "crosscodexd (PID $PID) ignored TERM for 5s; sending KILL"
			kill -KILL "$PID" 2>/dev/null || true
			if ! wait_gone "$PID"; then
				echo "ERROR: crosscodexd (PID $PID) still running after KILL" >&2
				exit 1
			fi
		fi
	fi
	rm -f "$E2E_DIR/daemon.pid"
fi

# A stub gateway normally exits with its daemon; this catches one that didn't.
if [[ -f "$E2E_DIR/llm-stub.pid" ]]; then
	STUB_PID=$(cat "$E2E_DIR/llm-stub.pid")
	if alive "$STUB_PID"; then
		echo "Stopping stub LLM gateway (PID $STUB_PID)..."
		kill -KILL "$STUB_PID" 2>/dev/null || true
		if ! wait_gone "$STUB_PID"; then
			echo "ERROR: stub LLM gateway (PID $STUB_PID) still running after KILL" >&2
			exit 1
		fi
	fi
	rm -f "$E2E_DIR/llm-stub.pid"
fi
