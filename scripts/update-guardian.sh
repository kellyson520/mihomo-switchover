#!/bin/sh
set -eu

# Update only the guardian executable after the one-time directory-mount
# migration. This script deliberately has no Compose lifecycle operation:
# the running Mihomo process is outside its mutation boundary.

SELF_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$SELF_DIR/.." && pwd)
CONTAINER=${MIHOMO_CONTAINER:-mihomo-cliproxy}
GUARDIAN_ROOT=${GUARDIAN_ROOT:-}
GO_IMAGE=${GO_IMAGE:-golang:1.24-alpine}
PRELIGHT=0
OBSERVE=0

GUARDIAN_BIN_DESTINATION="/guardian/bin"
GUARDIAN_BIN_MODE="directory"
CONTAINER_GUARDIAN_BINARY="$GUARDIAN_BIN_DESTINATION/guardian"

INSPECT_FILE=
BUILD_DIR=
BUILD_ARTIFACT=
LIVE_TMP=
ROLLBACK_TMP=
BACKUP_DIR=
BACKUP_BINARY=
LIVE_BINARY=
mihomo_pid_before=
UPDATE_LOG=
LOCK_FILE=
REPLACED=0
ROLLBACK_ATTEMPTED=0
ROLLBACK_OK=0
EXITING=0

usage() {
    echo "usage: $0 [--preflight] [--observe] [--container NAME] [--guardian-root PATH]" >&2
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --preflight) PRELIGHT=1; shift ;;
        --observe) OBSERVE=1; shift ;;
        --container)
            [ "$#" -ge 2 ] || { usage; exit 2; }
            CONTAINER=$2
            shift 2
            ;;
        --guardian-root)
            [ "$#" -ge 2 ] || { usage; exit 2; }
            GUARDIAN_ROOT=$2
            shift 2
            ;;
        -h|--help) usage; exit 0 ;;
        *) usage; exit 2 ;;
    esac
done

need() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "required command not found: $1" >&2
        exit 1
    }
}

need docker
need python3
need mktemp
need sha256sum
need file
need flock

docker inspect "$CONTAINER" >/dev/null 2>&1 || {
    echo "container not found: $CONTAINER" >&2
    exit 1
}

cleanup() {
    [ -z "$LIVE_TMP" ] || rm -f -- "$LIVE_TMP"
    [ -z "$ROLLBACK_TMP" ] || rm -f -- "$ROLLBACK_TMP"
    [ -z "$BUILD_ARTIFACT" ] || rm -f -- "$BUILD_ARTIFACT"
    [ -z "$BUILD_DIR" ] || rmdir -- "$BUILD_DIR" 2>/dev/null || true
    [ -z "$INSPECT_FILE" ] || rm -f -- "$INSPECT_FILE"
}

log_event() {
    event=$1
    [ -n "$UPDATE_LOG" ] || return 0
    printf '{"event":"%s","time":"%s"}\n' "$event" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$UPDATE_LOG"
}

log_event_best_effort() {
    log_event "$1" || true
}

on_exit() {
    status=$?
    if [ "$EXITING" -eq 1 ]; then
        return "$status"
    fi
    EXITING=1
    if [ "$REPLACED" -eq 1 ] && [ "$ROLLBACK_ATTEMPTED" -eq 0 ]; then
        if ! rollback_binary; then
            log_event_best_effort update_rollback_failed
            echo "update_rollback_failed: previous guardian binary was not restored" >&2
            status=1
        fi
    elif [ "$REPLACED" -eq 1 ] && [ "$ROLLBACK_OK" -eq 0 ]; then
        log_event_best_effort update_rollback_failed
        status=1
    fi
    cleanup
    exit "$status"
}

trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

INSPECT_FILE=$(mktemp "${TMPDIR:-/tmp}/mihomo-guardian-update.inspect.XXXXXX")

docker inspect "$CONTAINER" >"$INSPECT_FILE"
container_status=$(docker inspect --format '{{.State.Status}}' "$CONTAINER")
[ "$container_status" = "running" ] || {
    echo "container is not running: $CONTAINER" >&2
    exit 1
}

if [ -z "$GUARDIAN_ROOT" ]; then
    GUARDIAN_ROOT=$(python3 - "$INSPECT_FILE" <<'PY'
import json
import sys
from pathlib import Path

data = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
if len(data) != 1:
    raise SystemExit("container inspection did not identify exactly one container")
matches = [
    mount.get("Source", "")
    for mount in data[0].get("Mounts", [])
    if mount.get("Destination") == "/guardian/guardian.yaml"
]
if len(matches) != 1 or not matches[0]:
    raise SystemExit("guardian config mount was not identified uniquely")
print(Path(matches[0]).parent)
PY
)
fi

GUARDIAN_ROOT=$(CDPATH= cd -- "$GUARDIAN_ROOT" && pwd) || {
    echo "guardian root is not an accessible directory: $GUARDIAN_ROOT" >&2
    exit 1
}
GUARDIAN_BIN_DIR="$GUARDIAN_ROOT/bin"
LIVE_BINARY="$GUARDIAN_BIN_DIR/guardian"

mount_mode=$(python3 - "$INSPECT_FILE" "$GUARDIAN_ROOT" "$GUARDIAN_BIN_DESTINATION" <<'PY'
import json
import sys
from pathlib import Path

data = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
if len(data) != 1:
    raise SystemExit("container inspection did not identify exactly one container")
root = Path(sys.argv[2]).resolve()
destination = sys.argv[3]
mounts = [mount for mount in data[0].get("Mounts", []) if mount.get("Destination") == destination]
legacy = [mount for mount in data[0].get("Mounts", []) if mount.get("Destination") == "/guardian/bin/guardian"]
if len(mounts) != 1 or legacy:
    raise SystemExit("migration_required=1: guardian binary is not mounted as one directory")
mount = mounts[0]
source = Path(mount.get("Source", ""))
expected = root / "bin"
if mount.get("Type") != "bind" or source != expected or not source.is_absolute():
    raise SystemExit(f"guardian binary directory source is not {expected}")
if mount.get("Mode") != "ro" or mount.get("RW") is not False:
    raise SystemExit("guardian binary directory mount must be read-only")
if source.exists() and source.resolve() != source:
    raise SystemExit("guardian binary directory source is symlinked")
print("directory")
PY
)
[ "$mount_mode" = "$GUARDIAN_BIN_MODE" ] || {
    echo "migration_required=1: use install.sh --migrate-bin-mount during a maintenance window" >&2
    exit 1
}

[ -d "$GUARDIAN_BIN_DIR" ] || { echo "guardian binary directory is missing: $GUARDIAN_BIN_DIR" >&2; exit 1; }
[ -f "$LIVE_BINARY" ] && [ ! -L "$LIVE_BINARY" ] && [ -x "$LIVE_BINARY" ] || {
    echo "guardian binary is not a regular executable: $LIVE_BINARY" >&2
    exit 1
}
[ -f "$GUARDIAN_ROOT/guardian.yaml" ] || { echo "guardian.yaml is missing" >&2; exit 1; }
[ -f "$GUARDIAN_ROOT/controller_secret" ] || { echo "controller_secret is missing" >&2; exit 1; }
[ -f "$GUARDIAN_ROOT/start-guardian.sh" ] || { echo "start-guardian.sh is missing" >&2; exit 1; }
[ -d "$GUARDIAN_ROOT/logs" ] || { echo "guardian logs directory is missing" >&2; exit 1; }
[ -d "$GUARDIAN_ROOT/run" ] || { echo "guardian run directory is missing" >&2; exit 1; }

processes() {
    # The host-side process listing uses a different PID namespace. Signals
    # are sent through docker exec, so discover PIDs in the container instead.
    docker exec "$CONTAINER" ps -eo pid,ppid,comm,args 2>/dev/null
}

find_mihomo_pid() {
    processes | awk 'NR > 1 && $3 == "mihomo" { print $1; exit }'
}

find_process_pid() {
    wanted=$1
    processes | awk -v wanted="$wanted" '
        function ancestor_distance(node, ancestor, current, distance, steps) {
            current = node
            distance = 0
            for (steps = 0; steps < 64; steps++) {
                if (current == ancestor) return distance
                if (!(current in parent) || current == "0" || parent[current] == current) return 0
                current = parent[current]
                distance++
            }
            return 0
        }
        NR > 1 {
            parent[$1] = $2
            command[$1] = $3
            line[$1] = $0
            if ($0 ~ /\/guardian\/start-guardian\.sh/) launcher[$1] = 1
            if ($3 == "guardian" && $0 ~ /[[:space:]]run[[:space:]]/) guardian[$1] = 1
            if ($3 == "guardian" && $0 ~ /[[:space:]]quality-daemon[[:space:]]/) quality[$1] = 1
        }
        END {
            if (wanted == "guardian") {
                for (candidate in guardian) target[candidate] = 1
            } else if (wanted == "quality") {
                for (candidate in quality) target[candidate] = 1
            }
            best_candidate = ""
            best_distance = 999
            for (candidate in target) {
                for (launcher_pid in launcher) {
                    distance = ancestor_distance(candidate, launcher_pid)
                    if (distance > 0 && (distance < best_distance ||
                        (distance == best_distance &&
                         (best_candidate == "" || candidate + 0 < best_candidate + 0)))) {
                        best_candidate = candidate
                        best_distance = distance
                    }
                }
            }
            if (best_candidate != "") print best_candidate
        }
    '
}

find_guardian_pid() { find_process_pid guardian; }
find_quality_pid() { find_process_pid quality; }

mihomo_pid_before=$(find_mihomo_pid)
guardian_pid_before=$(find_guardian_pid)
quality_pid_before=$(find_quality_pid || true)
[ -n "$mihomo_pid_before" ] || { echo "mihomo process was not found" >&2; exit 1; }
[ -n "$guardian_pid_before" ] || { echo "guardian run process was not found" >&2; exit 1; }

container_id_before=$(docker inspect --format '{{.Id}}' "$CONTAINER")
old_hash=$(sha256sum "$LIVE_BINARY" | awk '{print $1}')

echo "container=$CONTAINER"
echo "guardian_root=$GUARDIAN_ROOT"
echo "guardian_bin_mount=$GUARDIAN_BIN_MODE"
echo "mihomo_pid=$mihomo_pid_before"
echo "guardian_pid=$guardian_pid_before"
echo "quality_pid=$quality_pid_before"
echo "old_hash=$old_hash"

if [ "$PRELIGHT" -eq 1 ]; then
    echo "migration_required=0"
    echo "preflight=ok (no build, files, services, containers, or signals changed)"
    exit 0
fi

[ -f "$GUARDIAN_ROOT/guardian.yaml" ] || { echo "guardian.yaml is missing" >&2; exit 1; }
CONTAINER_SECRET_FILE=$(PYTHONPATH="$REPO_DIR" python3 - "$GUARDIAN_ROOT/guardian.yaml" <<'PY'
import sys
from pathlib import Path
from urllib.parse import urlsplit

from scripts.discover import _parse_yaml

config = _parse_yaml(Path(sys.argv[1]).read_text(encoding="utf-8"), "guardian.yaml")
mihomo = config.get("mihomo", {})
if not isinstance(mihomo, dict):
    raise SystemExit("mihomo config section is not a mapping")
secret_file = str(mihomo.get("secret_file", "/guardian/controller_secret")).strip()
proxy = str(mihomo.get("proxy", "")).strip()
parsed = urlsplit(proxy)
if not parsed.hostname or parsed.port is None or not 1 <= parsed.port <= 65535:
    raise SystemExit("mihomo proxy URL does not contain a valid port")
if not secret_file.startswith("/"):
    raise SystemExit("mihomo secret_file must be an absolute container path")
print(secret_file)
print(parsed.port)
PY
)
CONTAINER_SECRET_FILE=$(printf '%s\n' "$CONTAINER_SECRET_FILE" | sed -n '1p')
proxy_port=$(PYTHONPATH="$REPO_DIR" python3 - "$GUARDIAN_ROOT/guardian.yaml" <<'PY'
import sys
from pathlib import Path
from urllib.parse import urlsplit

from scripts.discover import _parse_yaml

config = _parse_yaml(Path(sys.argv[1]).read_text(encoding="utf-8"), "guardian.yaml")
proxy = str(config.get("mihomo", {}).get("proxy", "")).strip()
parsed = urlsplit(proxy)
if parsed.port is None:
    raise SystemExit("mihomo proxy URL does not contain a valid port")
print(parsed.port)
PY
)
proxy_port_hex=$(printf '%04X' "$proxy_port")

LOCK_FILE="$GUARDIAN_ROOT/run/guardian-update.lock"
exec 9>"$LOCK_FILE"
flock -n 9 || { echo "another guardian update is already running" >&2; exit 1; }

UPDATE_LOG="$GUARDIAN_ROOT/logs/guardian-update.jsonl"

term_guardian_child() {
    pid=$1
    kind=$2
    case "$pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    case "$kind" in
        guardian|quality) ;;
        *) return 1 ;;
    esac
    docker exec "$CONTAINER" sh -c '
        pid=$1
        kind=$2
        snapshot=$(ps -eo pid,ppid,comm,args 2>/dev/null) || exit 1
        validated=$(printf "%s\n" "$snapshot" | awk -v target="$pid" -v wanted="$kind" '"'"'
            function has_launcher(node, current, steps) {
                current = parent[node]
                for (steps = 0; steps < 64; steps++) {
                    if (!(current in parent) || current == "0" || parent[current] == current) return 0
                    if (line[current] ~ /\/guardian\/start-guardian\.sh/) return 1
                    current = parent[current]
                }
                return 0
            }
            NR > 1 {
                parent[$1] = $2
                command[$1] = $3
                line[$1] = $0
            }
            END {
                if (!(target in parent) || command[target] != "guardian" || !has_launcher(target)) exit 1
                if (wanted == "guardian" && line[target] !~ /\/guardian\/bin\/guardian[[:space:]]+run[[:space:]]/) exit 1
                if (wanted == "quality" && line[target] !~ /\/guardian\/bin\/guardian[[:space:]]+quality-daemon[[:space:]]/) exit 1
                print target
            }
        '"'"') || exit 1
        [ "$validated" = "$pid" ] || exit 1
        kill -TERM "$pid"
    ' sh "$pid" "$kind"
}

reload_guardian_children() {
    current_guardian_pid=$(find_guardian_pid || true)
    current_quality_pid=$(find_quality_pid || true)
    if [ -n "$current_guardian_pid" ] && ! term_guardian_child "$current_guardian_pid" guardian; then
        return 1
    fi
    if [ -n "$current_quality_pid" ] && [ "$current_quality_pid" != "$current_guardian_pid" ] \
        && ! term_guardian_child "$current_quality_pid" quality; then
        return 1
    fi
}

rollback_binary() {
    ROLLBACK_ATTEMPTED=1
    ROLLBACK_OK=0
    [ -n "$BACKUP_BINARY" ] && [ -f "$BACKUP_BINARY" ] || return 1
    backup_hash=$(sha256sum "$BACKUP_BINARY" | awk '{print $1}') || return 1
    [ "$backup_hash" = "$old_hash" ] || return 1
    if ! ROLLBACK_TMP=$(mktemp "$GUARDIAN_BIN_DIR/.guardian.rollback.XXXXXX"); then
        return 1
    fi
    if ! cp "$BACKUP_BINARY" "$ROLLBACK_TMP"; then
        return 1
    fi
    chmod 0755 "$ROLLBACK_TMP" || return 1
    rollback_hash=$(sha256sum "$ROLLBACK_TMP" | awk '{print $1}') || return 1
    [ "$rollback_hash" = "$old_hash" ] || return 1
    if ! python3 - "$ROLLBACK_TMP" <<'PY'
import os
import sys

path = sys.argv[1]
fd = os.open(path, os.O_RDONLY)
try:
    os.fsync(fd)
finally:
    os.close(fd)
PY
    then
        return 1
    fi
    if ! mv "$ROLLBACK_TMP" "$LIVE_BINARY"; then
        return 1
    fi
    ROLLBACK_TMP=
    if ! python3 - "$GUARDIAN_BIN_DIR" <<'PY'
import os
import sys

directory = os.open(sys.argv[1], os.O_RDONLY)
try:
    os.fsync(directory)
finally:
    os.close(directory)
PY
    then
        return 1
    fi
    live_hash=$(sha256sum "$LIVE_BINARY" | awk '{print $1}') || return 1
    [ "$live_hash" = "$old_hash" ] || return 1
    rollback_guardian_pid_before=$(find_guardian_pid || true)
    rollback_quality_pid_before=$(find_quality_pid || true)
    reload_guardian_children || return 1
    attempt=0
    while [ "$attempt" -lt 60 ]; do
        container_status_now=$(docker inspect --format '{{.State.Status}}' "$CONTAINER" 2>/dev/null || true)
        container_id_after=$(docker inspect --format '{{.Id}}' "$CONTAINER" 2>/dev/null || true)
        mihomo_pid_after=$(find_mihomo_pid || true)
        guardian_pid_after=$(find_guardian_pid || true)
        quality_pid_after=$(find_quality_pid || true)
        visible_hash=$(container_guardian_hash || true)
        quality_reloaded=0
        if [ -z "$rollback_quality_pid_before" ]; then
            quality_reloaded=1
        elif [ -n "$quality_pid_after" ] && [ "$quality_pid_after" != "$rollback_quality_pid_before" ]; then
            quality_reloaded=1
        fi
        if [ "$container_status_now" = "running" ] \
            && [ "$container_id_after" = "$container_id_before" ] \
            && [ "$mihomo_pid_after" = "$mihomo_pid_before" ] \
            && [ -n "$guardian_pid_after" ] \
            && { [ -z "$rollback_guardian_pid_before" ] || [ "$guardian_pid_after" != "$rollback_guardian_pid_before" ]; } \
            && [ "$quality_reloaded" -eq 1 ] \
            && [ "$visible_hash" = "$old_hash" ] \
            && verify_mihomo_runtime; then
            ROLLBACK_OK=1
            log_event_best_effort update_rolled_back
            return 0
        fi
        attempt=$((attempt + 1))
        sleep 1
    done
    return 1
}

container_guardian_hash() {
    hash_tmp=$(mktemp "${TMPDIR:-/tmp}/mihomo-guardian-visible.XXXXXX") || return 1
    if ! docker exec "$CONTAINER" cat "$CONTAINER_GUARDIAN_BINARY" >"$hash_tmp" 2>/dev/null; then
        rm -f -- "$hash_tmp"
        return 1
    fi
    hash_value=$(sha256sum "$hash_tmp" | awk '{print $1}') || {
        rm -f -- "$hash_tmp"
        return 1
    }
    rm -f -- "$hash_tmp"
    printf '%s\n' "$hash_value"
}

verify_mihomo_api() {
    docker exec "$CONTAINER" "$CONTAINER_GUARDIAN_BINARY" status \
        --config /guardian/guardian.yaml \
        --data /guardian/data \
        --secret-file "$CONTAINER_SECRET_FILE" >/dev/null 2>&1
}

verify_proxy_listener() {
    tcp4=$(docker exec "$CONTAINER" cat /proc/net/tcp 2>/dev/null) || return 1
    tcp6=$(docker exec "$CONTAINER" cat /proc/net/tcp6 2>/dev/null) || return 1
    printf '%s\n%s\n' "$tcp4" "$tcp6" | awk -v port="$proxy_port_hex" '
        NR > 1 && $4 == "0A" && $2 ~ (":" port "$") { found = 1 }
        END { exit found ? 0 : 1 }
    '
}

verify_mihomo_runtime() {
    mihomo_pid_after=$(find_mihomo_pid || true)
    [ "$mihomo_pid_after" = "$mihomo_pid_before" ] || return 1
    container_status_now=$(docker inspect --format '{{.State.Status}}' "$CONTAINER" 2>/dev/null || true)
    [ "$container_status_now" = "running" ] || return 1
    verify_mihomo_api || return 1
    verify_proxy_listener || return 1
}

log_event update_started
BUILD_DIR=$(mktemp -d "${TMPDIR:-/tmp}/mihomo-guardian-update.XXXXXX")
BUILD_ARTIFACT="$BUILD_DIR/guardian"

docker run --rm --network "container:$CONTAINER" \
    -v "$REPO_DIR:/src:ro" -v "$BUILD_DIR:/build" -w /src "$GO_IMAGE" \
    sh -c 'go test -mod=vendor ./... && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -trimpath -ldflags="-s -w" -o /build/guardian ./cmd/guardian'

[ -s "$BUILD_ARTIFACT" ] && [ -x "$BUILD_ARTIFACT" ] || {
    echo "built guardian artifact is missing or empty" >&2
    exit 1
}
artifact_type=$(file "$BUILD_ARTIFACT")
case "$artifact_type" in
    *ELF*) ;;
    *) echo "built guardian artifact is not an ELF executable" >&2; exit 1 ;;
esac
case "$artifact_type" in
    *x86-64*|*x86_64*) ;;
    *) echo "built guardian artifact is not Linux amd64: $artifact_type" >&2; exit 1 ;;
esac
new_hash=$(sha256sum "$BUILD_ARTIFACT" | awk '{print $1}')
[ "$new_hash" != "$old_hash" ] || {
    echo "built guardian hash is unchanged: $new_hash" >&2
    exit 1
}

BACKUP_DIR="$GUARDIAN_ROOT/backups/update-$(date -u +%Y%m%dT%H%M%SZ)-$$"
mkdir -p "$BACKUP_DIR"
BACKUP_BINARY="$BACKUP_DIR/guardian"
cp -p "$LIVE_BINARY" "$BACKUP_BINARY"
printf 'old_hash=%s\nnew_hash=%s\ncontainer=%s\nmihomo_pid=%s\n' \
    "$old_hash" "$new_hash" "$CONTAINER" "$mihomo_pid_before" >"$BACKUP_DIR/manifest"
log_event binary_backed_up

LIVE_TMP=$(mktemp "$GUARDIAN_BIN_DIR/.guardian.update.XXXXXX")
cp "$BUILD_ARTIFACT" "$LIVE_TMP"
chmod 0755 "$LIVE_TMP"
python3 - "$LIVE_TMP" <<'PY'
import os
import sys

path = sys.argv[1]
fd = os.open(path, os.O_RDONLY)
try:
    os.fsync(fd)
finally:
    os.close(fd)
directory = os.open(os.path.dirname(path), os.O_RDONLY)
try:
    os.fsync(directory)
finally:
    os.close(directory)
PY
[ "$(sha256sum "$LIVE_TMP" | awk '{print $1}')" = "$new_hash" ] || {
    echo "temporary guardian hash verification failed" >&2
    exit 1
}
REPLACED=1
mv "$LIVE_TMP" "$LIVE_BINARY"
LIVE_TMP=
python3 - "$GUARDIAN_BIN_DIR" <<'PY'
import os
import sys

directory = os.open(sys.argv[1], os.O_RDONLY)
try:
    os.fsync(directory)
finally:
    os.close(directory)
PY

reload_guardian_children
log_event guardian_reloaded

wait_for_updated_guardian() {
    attempt=0
    stable_checks=0
    while [ "$attempt" -lt 60 ]; do
        container_status_now=$(docker inspect --format '{{.State.Status}}' "$CONTAINER" 2>/dev/null || true)
        container_id_after=$(docker inspect --format '{{.Id}}' "$CONTAINER" 2>/dev/null || true)
        mihomo_pid_after=$(find_mihomo_pid || true)
        guardian_pid_after=$(find_guardian_pid || true)
        quality_pid_after=$(find_quality_pid || true)
        visible_hash=$(container_guardian_hash || true)
        quality_reloaded=0
        if [ -z "$quality_pid_before" ]; then
            quality_reloaded=1
        elif [ -n "$quality_pid_after" ] && [ "$quality_pid_after" != "$quality_pid_before" ]; then
            quality_reloaded=1
        fi
        if [ "$container_status_now" = "running" ] \
            && [ "$container_id_after" = "$container_id_before" ] \
            && [ "$mihomo_pid_after" = "$mihomo_pid_before" ] \
            && [ -n "$guardian_pid_after" ] \
            && [ "$guardian_pid_after" != "$guardian_pid_before" ] \
            && [ "$quality_reloaded" -eq 1 ] \
            && [ "$visible_hash" = "$new_hash" ] \
            && verify_mihomo_runtime; then
            stable_checks=$((stable_checks + 1))
            if [ "$stable_checks" -ge 2 ]; then
                return 0
            fi
        else
            stable_checks=0
        fi
        attempt=$((attempt + 1))
        sleep 1
    done
    return 1
}

if wait_for_updated_guardian; then
    log_event update_verified
    REPLACED=0
    echo "update=ok new_hash=$new_hash observe=$OBSERVE mihomo_pid=$mihomo_pid_before"
    exit 0
fi

echo "guardian update verification failed; restoring the previous binary" >&2
if rollback_binary; then
    REPLACED=0
    echo "update=rolled_back mihomo_pid=$mihomo_pid_before" >&2
else
    log_event_best_effort update_rollback_failed
    echo "update_rollback_failed: previous guardian binary was not restored" >&2
fi
exit 1
