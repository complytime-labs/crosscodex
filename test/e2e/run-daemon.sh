#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="${1:?Missing argument: repo root}"
TEST_DIR="${2:?Missing argument: test directory}"
MODE="${3:-hermetic}"
E2E_DIR="$TEST_DIR/e2e"

mkdir -p "$E2E_DIR/config/crosscodex" "$E2E_DIR/objects" "$E2E_DIR/nats" "$E2E_DIR/attestation"

# Pre-start cleanup: kill any prior daemon tracked by pidfile
if [ -f "$E2E_DIR/daemon.pid" ]; then
  OLD_PID=$(cat "$E2E_DIR/daemon.pid")
  if kill -0 "$OLD_PID" 2>/dev/null; then
    echo "Cleaning up stale daemon (PID $OLD_PID)..."
    kill -TERM "$OLD_PID" 2>/dev/null || true
    # Wait up to 5s for graceful exit
    for i in $(seq 1 50); do
      if ! kill -0 "$OLD_PID" 2>/dev/null; then
        break
      fi
      sleep 0.1
    done
    # Force kill if still alive
    if kill -0 "$OLD_PID" 2>/dev/null; then
      kill -KILL "$OLD_PID" 2>/dev/null || true
      sleep 0.5
    fi
  fi
  rm -f "$E2E_DIR/daemon.pid"
fi

# Port guard: ensure 18443 is not bound by a foreign process
if timeout 1 bash -c 'cat < /dev/null > /dev/tcp/127.0.0.1/18443' 2>/dev/null; then
  echo "ERROR: Port 18443 is already in use by another process." >&2
  echo "Cannot start daemon. Kill the foreign process first." >&2
  exit 1
fi

# Copy static config to XDG_CONFIG_HOME location (mode-dependent)
if [ "$MODE" = "llm" ]; then
  cp "$ROOT_DIR/test/e2e/crosscodexd.e2e.llm.yaml" "$E2E_DIR/config/crosscodex/config.yaml"
else
  cp "$ROOT_DIR/test/e2e/crosscodexd.e2e.yaml" "$E2E_DIR/config/crosscodex/config.yaml"
fi

# Read base DSN from environment (set by taskfile)
DSN="${CROSSCODEX_DATABASE_DSN:?CROSSCODEX_DATABASE_DSN not set}"
CERTS_DIR="$TEST_DIR/certs"

# Provision graph_user: run migrations and set password
echo "Provisioning graph_user..."
cd "$ROOT_DIR"
go build -o "$E2E_DIR/provision" ./test/e2e/provision
GRAPH_DSN="$("$E2E_DIR/provision" "$DSN")"

# Generate attestation keypair
go build -o "$E2E_DIR/genkey" ./test/e2e/genkey
"$E2E_DIR/genkey" "$E2E_DIR/attestation"

# Export config overrides via CROSSCODEX_ env vars
export XDG_CONFIG_HOME="$E2E_DIR/config"
export CROSSCODEX_DATABASE_DSN="$DSN"
export CROSSCODEX_DATABASE_GRAPH_DSN="$GRAPH_DSN"
export CROSSCODEX_NATS_EMBEDDED_STORE_DIR="$E2E_DIR/nats"
export CROSSCODEX_STORAGE_OBJECTS_BASE_PATH="$E2E_DIR/objects"
export CROSSCODEX_TLS_CA="$CERTS_DIR/ca.pem"
export CROSSCODEX_TLS_CERT="$CERTS_DIR/server.pem"
export CROSSCODEX_TLS_KEY="$CERTS_DIR/server-key.pem"
export CROSSCODEX_ATTESTATION_PRIVATE_KEY_PATH="$E2E_DIR/attestation/private.pem"
export CROSSCODEX_ATTESTATION_PUBLIC_KEY_PATH="$E2E_DIR/attestation/public.pem"

echo "Starting crosscodexd --role all..."
"$ROOT_DIR/bin/crosscodexd" --role all >"$E2E_DIR/daemon.log" 2>&1 &
DAEMON_PID=$!
echo "$DAEMON_PID" >"$E2E_DIR/daemon.pid"

# Fail-fast readiness: poll health endpoint, checking process is alive each iteration
echo "Waiting for daemon health..."
for i in $(seq 1 60); do
  # Check if daemon process is still alive
  if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
    echo "Daemon process exited prematurely (e.g., bind failure):" >&2
    cat "$E2E_DIR/daemon.log" >&2
    exit 1
  fi
  # Check if health endpoint responds
  if curl -sf --cacert "$CERTS_DIR/ca.pem" https://127.0.0.1:18443/healthz >/dev/null 2>&1; then
    echo "Daemon ready."
    exit 0
  fi
  sleep 1
done

echo "Daemon failed to become healthy within 60s:" >&2
cat "$E2E_DIR/daemon.log" >&2
exit 1
