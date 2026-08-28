#!/bin/sh
set -eu

MIHOMO_BIN=${MIHOMO_BIN:-/mihomo}
MIHOMO_CONFIG_DIR=${MIHOMO_CONFIG_DIR:-/root/.config/mihomo}
GUARDIAN_BIN=${GUARDIAN_BIN:-/guardian/bin/guardian}
GUARDIAN_CONFIG=${GUARDIAN_CONFIG:-/guardian/guardian.yaml}
GUARDIAN_DATA=${GUARDIAN_DATA:-/guardian/data}
GUARDIAN_LOGS=${GUARDIAN_LOGS:-/guardian/logs}
GUARDIAN_SECRET=${GUARDIAN_SECRET:-/guardian/controller_secret}
LAUNCHER_LOG=${LAUNCHER_LOG:-$GUARDIAN_LOGS/launcher.log}

mkdir -p "$GUARDIAN_DATA" "$GUARDIAN_LOGS"

log() {
    stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    printf '%s %s\n' "$stamp" "$*" >> "$LAUNCHER_LOG"
}

"$MIHOMO_BIN" -d "$MIHOMO_CONFIG_DIR" &
mihomo_pid=$!
log "mihomo started pid=$mihomo_pid"

guardian_loop() {
    current_guardian_pid=
    trap 'if [ -n "${current_guardian_pid:-}" ]; then kill "$current_guardian_pid" 2>/dev/null || true; fi; exit 0' TERM INT
    while kill -0 "$mihomo_pid" 2>/dev/null; do
        set +e
        "$GUARDIAN_BIN" run \
            --config "$GUARDIAN_CONFIG" \
            --data "$GUARDIAN_DATA" \
            --logs "$GUARDIAN_LOGS" \
            --secret-file "$GUARDIAN_SECRET" &
        current_guardian_pid=$!
        wait "$current_guardian_pid"
        guardian_status=$?
        set -e
        current_guardian_pid=
        log "guardian exited status=$guardian_status; mihomo remains running, retrying in 1s"
        sleep 1
    done
}

guardian_loop &
guardian_pid=$!

shutdown() {
    trap - TERM INT
    log "shutdown requested; stopping launcher children"
    kill "$guardian_pid" 2>/dev/null || true
    kill "$mihomo_pid" 2>/dev/null || true
    wait "$guardian_pid" 2>/dev/null || true
    wait "$mihomo_pid" 2>/dev/null || true
    exit 0
}

trap shutdown TERM INT

set +e
wait "$mihomo_pid"
mihomo_status=$?
set -e
kill "$guardian_pid" 2>/dev/null || true
wait "$guardian_pid" 2>/dev/null || true
log "mihomo exited status=$mihomo_status; launcher exiting"
exit "$mihomo_status"
