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
LIVE_BINARY=
mihomo_pid_before=

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

docker inspect "$CONTAINER" >/dev/null 2>&1 || {
    echo "container not found: $CONTAINER" >&2
    exit 1
}

INSPECT_FILE=$(mktemp "${TMPDIR:-/tmp}/mihomo-guardian-update.inspect.XXXXXX")

cleanup() {
    [ -z "$LIVE_TMP" ] || rm -f -- "$LIVE_TMP"
    [ -z "$ROLLBACK_TMP" ] || rm -f -- "$ROLLBACK_TMP"
    [ -z "$BUILD_ARTIFACT" ] || rm -f -- "$BUILD_ARTIFACT"
    [ -z "$BUILD_DIR" ] || rmdir -- "$BUILD_DIR" 2>/dev/null || true
    [ -z "$INSPECT_FILE" ] || rm -f -- "$INSPECT_FILE"
}
trap cleanup EXIT INT TERM

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

processes() {
    docker top "$CONTAINER" -eo pid,ppid,comm,args 2>/dev/null
}

find_mihomo_pid() {
    processes | awk 'NR > 1 && $3 == "mihomo" { print $1; exit }'
}

find_guardian_pid() {
    processes | awk 'NR > 1 && $3 == "guardian" && $0 ~ /[[:space:]]run[[:space:]]/ { print $1; exit }'
}

find_quality_pid() {
    processes | awk 'NR > 1 && $3 == "guardian" && $0 ~ /[[:space:]]quality-daemon[[:space:]]/ { print $1; exit }'
}

mihomo_pid_before=$(find_mihomo_pid)
guardian_pid=$(find_guardian_pid)
quality_pid=$(find_quality_pid || true)
[ -n "$mihomo_pid_before" ] || { echo "mihomo process was not found" >&2; exit 1; }
[ -n "$guardian_pid" ] || { echo "guardian run process was not found" >&2; exit 1; }

container_id_before=$(docker inspect --format '{{.Id}}' "$CONTAINER")
old_hash=$(sha256sum "$LIVE_BINARY" | awk '{print $1}')

echo "container=$CONTAINER"
echo "guardian_root=$GUARDIAN_ROOT"
echo "guardian_bin_mount=$GUARDIAN_BIN_MODE"
echo "mihomo_pid=$mihomo_pid_before"
echo "guardian_pid=$guardian_pid"
echo "quality_pid=${quality_pid:-none}"
echo "old_hash=$old_hash"

if [ "$PRELIGHT" -eq 1 ]; then
    echo "migration_required=0"
    echo "preflight=ok (no build, files, services, containers, or signals changed)"
    exit 0
fi

UPDATE_LOG="$GUARDIAN_ROOT/logs/guardian-update.jsonl"
log_event() {
    event=$1
    printf '{"event":"%s","time":"%s"}\n' "$event" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$UPDATE_LOG"
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

container_guardian_hash() {
    docker exec "$CONTAINER" cat "$CONTAINER_GUARDIAN_BINARY" 2>/dev/null | sha256sum | awk '{print $1}'
}

term_guardian_child() {
    pid=$1
    case "$pid" in
        ''|*[!0-9]*) return 0 ;;
    esac
    docker exec "$CONTAINER" kill -TERM "$pid" >/dev/null 2>&1 || true
}

reload_guardian_children() {
    guardian_pid=$(find_guardian_pid || true)
    quality_pid=$(find_quality_pid || true)
    [ -z "$guardian_pid" ] || term_guardian_child "$guardian_pid"
    [ -z "$quality_pid" ] || [ "$quality_pid" = "$guardian_pid" ] || term_guardian_child "$quality_pid"
}

reload_guardian_children
log_event guardian_reloaded

wait_for_updated_guardian() {
    attempt=0
    while [ "$attempt" -lt 60 ]; do
        container_status_now=$(docker inspect --format '{{.State.Status}}' "$CONTAINER" 2>/dev/null || true)
        container_id_after=$(docker inspect --format '{{.Id}}' "$CONTAINER" 2>/dev/null || true)
        mihomo_pid_after=$(find_mihomo_pid || true)
        guardian_pid_after=$(find_guardian_pid || true)
        visible_hash=$(container_guardian_hash || true)
        if [ "$container_status_now" = "running" ] \
            && [ "$container_id_after" = "$container_id_before" ] \
            && [ "$mihomo_pid_after" = "$mihomo_pid_before" ] \
            && [ -n "$guardian_pid_after" ] \
            && [ "$guardian_pid_after" != "$guardian_pid" ] \
            && [ "$visible_hash" = "$new_hash" ]; then
            return 0
        fi
        attempt=$((attempt + 1))
        sleep 1
    done
    return 1
}

rollback_binary() {
    ROLLBACK_TMP=$(mktemp "$GUARDIAN_BIN_DIR/.guardian.rollback.XXXXXX")
    cp "$BACKUP_BINARY" "$ROLLBACK_TMP"
    chmod 0755 "$ROLLBACK_TMP"
    python3 - "$ROLLBACK_TMP" <<'PY'
import os
import sys

path = sys.argv[1]
fd = os.open(path, os.O_RDONLY)
try:
    os.fsync(fd)
finally:
    os.close(fd)
PY
    mv "$ROLLBACK_TMP" "$LIVE_BINARY"
    ROLLBACK_TMP=
    python3 - "$GUARDIAN_BIN_DIR" <<'PY'
import os
import sys

directory = os.open(sys.argv[1], os.O_RDONLY)
try:
    os.fsync(directory)
finally:
    os.close(directory)
PY
    log_event update_rolled_back
    reload_guardian_children
}

if wait_for_updated_guardian; then
    log_event update_verified
    echo "update=ok new_hash=$new_hash observe=$OBSERVE mihomo_pid=$mihomo_pid_before"
    exit 0
fi

echo "guardian update verification failed; restoring the previous binary" >&2
rollback_binary
echo "update=rolled_back mihomo_pid=$mihomo_pid_before" >&2
exit 1
