#!/bin/sh
set -eu

# One-shot installer.  It is deliberately conservative: discovery is read
# only, ambiguity aborts, and every live mutation happens after a backup.

SELF_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$SELF_DIR/.." && pwd)
export PYTHONPATH="$REPO_DIR${PYTHONPATH:+:$PYTHONPATH}"
CONTAINER=${MIHOMO_CONTAINER:-mihomo-cliproxy}
COMPOSE_PATH=${MIHOMO_COMPOSE:-}
PRELIGHT=0
OBSERVE=0

usage() {
    echo "usage: $0 [--preflight] [--observe] [--compose PATH] [--container NAME]" >&2
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --preflight) PRELIGHT=1; shift ;;
        --observe) OBSERVE=1; shift ;;
        --compose) [ "$#" -ge 2 ] || { usage; exit 2; }; COMPOSE_PATH=$2; shift 2 ;;
        --container) [ "$#" -ge 2 ] || { usage; exit 2; }; CONTAINER=$2; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) usage; exit 2 ;;
    esac
done

need() {
    command -v "$1" >/dev/null 2>&1 || { echo "required command not found: $1" >&2; exit 1; }
}

need docker
need python3
need mktemp
[ "$(id -u)" -eq 0 ] || { echo "install.sh must run as root" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "docker compose plugin is required" >&2; exit 1; }
docker inspect "$CONTAINER" >/dev/null 2>&1 || { echo "container not found: $CONTAINER" >&2; exit 1; }

find_compose() {
    if [ -n "$COMPOSE_PATH" ]; then
        [ -f "$COMPOSE_PATH" ] || { echo "compose file not found: $COMPOSE_PATH" >&2; exit 1; }
        return
    fi
    candidates=""
    for file in "$PWD/docker-compose.yml" "$PWD/docker-compose.yaml" "$PWD/compose.yml" "$PWD/compose.yaml" \
        "/opt/mihomo-cliproxy/docker-compose.yml" "/opt/mihomo-cliproxy/docker-compose.yaml"; do
        [ -f "$file" ] && candidates="$candidates\n$file"
    done
    label_paths=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$CONTAINER" 2>/dev/null || true)
    old_ifs=$IFS
    IFS=,
    for file in $label_paths; do
        [ -f "$file" ] && candidates="$candidates\n$file"
    done
    IFS=$old_ifs
    selected=$(printf '%b\n' "$candidates" | sed '/^$/d' | awk '!seen[$0]++')
    count=$(printf '%s\n' "$selected" | sed '/^$/d' | wc -l | tr -d ' ')
    [ "$count" -eq 1 ] || { echo "compose file could not be identified uniquely (candidates: $count); pass --compose" >&2; exit 1; }
    COMPOSE_PATH=$selected
}

find_compose
COMPOSE_PATH=$(CDPATH= cd -- "$(dirname -- "$COMPOSE_PATH")" && pwd)/$(basename -- "$COMPOSE_PATH")
PROJECT_DIR=$(dirname -- "$COMPOSE_PATH")
GUARDIAN_ROOT=${GUARDIAN_ROOT:-$PROJECT_DIR/guardian}
QUALITY_TEMPLATE="$GUARDIAN_ROOT/guardian.yaml"
[ -f "$QUALITY_TEMPLATE" ] || QUALITY_TEMPLATE="$REPO_DIR/configs/guardian.example.yaml"

GUARDIAN_TMPDIR=${TMPDIR:-/tmp}
INSPECT_FILE=$(mktemp "$GUARDIAN_TMPDIR/mihomo-guardian.inspect.XXXXXX")
DISCOVERY_FILE=$(mktemp "$GUARDIAN_TMPDIR/mihomo-guardian.discovery.XXXXXX")
QUALITY_TARGETS_FILE=$(mktemp "$GUARDIAN_TMPDIR/mihomo-guardian.quality-targets.XXXXXX")
QUALITY_TCP_FILE=$(mktemp "$GUARDIAN_TMPDIR/mihomo-guardian.tcp.XXXXXX")
QUALITY_TCP6_FILE=$(mktemp "$GUARDIAN_TMPDIR/mihomo-guardian.tcp6.XXXXXX")
cleanup() {
    rm -f "$INSPECT_FILE" "$DISCOVERY_FILE" "$QUALITY_TARGETS_FILE" \
        "$QUALITY_TCP_FILE" "$QUALITY_TCP6_FILE"
}
trap cleanup EXIT INT TERM

docker inspect "$CONTAINER" >"$INSPECT_FILE"

CONFIG_PATH=$(python3 - "$INSPECT_FILE" <<'PY'
import json
import sys

data = json.load(open(sys.argv[1], encoding="utf-8"))[0]
for mount in data.get("Mounts", []):
    if mount.get("Destination") == "/root/.config/mihomo/config.yaml":
        source = mount.get("Source", "")
        if source:
            print(source)
            break
PY
)
[ -n "$CONFIG_PATH" ] && [ -f "$CONFIG_PATH" ] || { echo "mihomo config bind mount was not found; refusing to guess" >&2; exit 1; }

python3 "$REPO_DIR/scripts/discover.py" \
    --compose "$COMPOSE_PATH" \
    --config "$CONFIG_PATH" \
    --inspect-json "$INSPECT_FILE" \
    --format json >"$DISCOVERY_FILE"

json_value() {
    path=$1
    python3 - "$DISCOVERY_FILE" "$path" <<'PY'
import json
import sys

data = json.load(open(sys.argv[1], encoding="utf-8"))
value = data
for part in sys.argv[2].split("."):
    if isinstance(value, dict):
        value = value.get(part)
    else:
        value = None
    if value is None:
        break
if value is not None and not isinstance(value, (dict, list)):
    print(value)
PY
}

DISCOVERED_CONTAINER=$(json_value container_name || true)
DISCOVERED_SERVICE=$(json_value service_name || true)
DISCOVERED_API=$(json_value api || true)
[ -n "$DISCOVERED_API" ] || DISCOVERED_API=$(json_value api_url || true)
DISCOVERED_PROXY=$(json_value proxy || true)
[ -n "$DISCOVERED_PROXY" ] || DISCOVERED_PROXY=$(json_value proxy_url || true)
DISCOVERED_CHANNEL=$(json_value groups.channel || true)
DISCOVERED_MAIN=$(json_value groups.main || true)
DISCOVERED_BACKUP=$(json_value groups.backup || true)
json_list_value() {
    path=$1
    python3 - "$DISCOVERY_FILE" "$path" <<'PY'
import json
import sys

data = json.load(open(sys.argv[1], encoding="utf-8"))
value = data
for part in sys.argv[2].split("."):
    value = value.get(part) if isinstance(value, dict) else None
    if value is None:
        break
if isinstance(value, list):
    print(",".join(str(item) for item in value))
elif value is not None and not isinstance(value, dict):
    print(value)
PY
}
DISCOVERED_MAIN_PROVIDER=$(json_list_value "providers.$DISCOVERED_MAIN" || true)
DISCOVERED_BACKUP_PROVIDER=$(json_list_value "providers.$DISCOVERED_BACKUP" || true)
[ "$DISCOVERED_CONTAINER" = "$CONTAINER" ] || { echo "discovery container mismatch; refusing to continue" >&2; exit 1; }
[ -n "$DISCOVERED_SERVICE" ] || { echo "discovery service name is missing; refusing to continue" >&2; exit 1; }
[ -n "$DISCOVERED_API" ] && [ -n "$DISCOVERED_PROXY" ] || { echo "discovery did not return internal API and proxy URLs" >&2; exit 1; }
has_secret=$(json_value has_secret || true)
case "$has_secret" in
    true|True|1) ;;
    *) echo "mihomo controller secret is not configured; refusing to continue" >&2; exit 1 ;;
esac

QUALITY_DECLARED=$(PYTHONPATH="$REPO_DIR" python3 - "$QUALITY_TEMPLATE" <<'PY'
import json
import sys
from pathlib import Path

from scripts.discover import quality_targets_from_text

targets = quality_targets_from_text(Path(sys.argv[1]).read_text(encoding="utf-8"))
print(json.dumps(targets, ensure_ascii=False, separators=(",", ":")))
PY
)
case "$QUALITY_DECLARED" in
    "[]") printf '%s\n' '[]' >"$QUALITY_TARGETS_FILE" ;;
    *)
        if ! docker exec "$CONTAINER" cat /proc/net/tcp >"$QUALITY_TCP_FILE"; then
            echo "quality preflight could not read container /proc/net/tcp; refusing to guess ports" >&2
            exit 1
        fi
        if ! docker exec "$CONTAINER" cat /proc/net/tcp6 >"$QUALITY_TCP6_FILE"; then
            echo "quality preflight could not read container /proc/net/tcp6; refusing to guess ports" >&2
            exit 1
        fi
        PYTHONPATH="$REPO_DIR" python3 - "$CONFIG_PATH" "$QUALITY_TEMPLATE" "$QUALITY_TCP_FILE" "$QUALITY_TCP6_FILE" "$QUALITY_TARGETS_FILE" <<'PY'
import json
import sys
from pathlib import Path

from scripts.discover import prepare_quality_targets, quality_targets_from_text

config_text = Path(sys.argv[1]).read_text(encoding="utf-8")
guardian_text = Path(sys.argv[2]).read_text(encoding="utf-8")
targets = quality_targets_from_text(guardian_text)
prepared = prepare_quality_targets(
    config_text,
    targets,
    proc_tcp=Path(sys.argv[3]).read_text(encoding="ascii"),
    proc_tcp6=Path(sys.argv[4]).read_text(encoding="ascii"),
)
Path(sys.argv[5]).write_text(
    json.dumps(prepared, ensure_ascii=False, separators=(",", ":")) + "\n",
    encoding="utf-8",
)
PY
        ;;
esac

echo "discovered container=$CONTAINER"
echo "discovered service=$DISCOVERED_SERVICE"
echo "discovered compose=$COMPOSE_PATH"
echo "discovered mihomo_config=$CONFIG_PATH"
echo "discovered api=$DISCOVERED_API"
echo "discovered proxy=$DISCOVERED_PROXY"
echo "discovered groups channel=$DISCOVERED_CHANNEL main=$DISCOVERED_MAIN backup=$DISCOVERED_BACKUP"
echo "discovered providers main=${DISCOVERED_MAIN_PROVIDER:-unknown} backup=${DISCOVERED_BACKUP_PROVIDER:-unknown}"

(cd "$PROJECT_DIR" && docker compose -f "$COMPOSE_PATH" config >/dev/null) || { echo "compose config validation failed" >&2; exit 1; }
PATCHED_COMPOSE=$(python3 - "$COMPOSE_PATH" "$GUARDIAN_ROOT" "$PROJECT_DIR" <<'PY'
import sys
from pathlib import Path

from scripts.compose_patch import patch_compose

source = Path(sys.argv[1]).read_text(encoding="utf-8")
print(patch_compose(source, sys.argv[2], sys.argv[3]), end="")
PY
)
[ -n "$PATCHED_COMPOSE" ] || { echo "compose patch produced empty output" >&2; exit 1; }
printf '%s' "$PATCHED_COMPOSE" | (cd "$PROJECT_DIR" && docker compose -f - config >/dev/null) 2>/dev/null || {
    echo "patched compose failed validation" >&2
    exit 1
}
PYTHONPATH="$REPO_DIR" python3 - "$CONFIG_PATH" "$DISCOVERED_MAIN" "$DISCOVERED_BACKUP" "$QUALITY_TARGETS_FILE" <<'PY'
import json
import sys
from pathlib import Path
from scripts.discover import _parse_yaml
from scripts.mihomo_config_patch import patch_provider_groups, patch_quality_targets

text = Path(sys.argv[1]).read_text(encoding="utf-8")
patched = patch_provider_groups(text, sys.argv[2], sys.argv[3])
targets = json.loads(Path(sys.argv[4]).read_text(encoding="utf-8"))
patched = patch_quality_targets(patched, targets)
_parse_yaml(patched, "patched mihomo config")
PY

if [ "$PRELIGHT" -eq 1 ]; then
    echo "preflight=ok (no files, services, or containers changed)"
    exit 0
fi

build_binary() {
    mkdir -p "$REPO_DIR/dist"
    docker run --rm --network "container:$CONTAINER" \
        -v "$REPO_DIR:/src" -w /src golang:1.24-alpine \
        sh -c 'go test -mod=vendor ./... && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -trimpath -o dist/guardian ./cmd/guardian'
    test -x "$REPO_DIR/dist/guardian"
}

backup_dir="$GUARDIAN_ROOT/backups/$(date -u +%Y%m%dT%H%M%SZ)-$$"
mkdir -p "$GUARDIAN_ROOT/bin" "$GUARDIAN_ROOT/data" "$GUARDIAN_ROOT/logs" "$GUARDIAN_ROOT/run" "$backup_dir"
cp -p "$COMPOSE_PATH" "$backup_dir/compose.yml"
cp -p "$CONFIG_PATH" "$backup_dir/mihomo-config.yaml"
old_switcher="$PROJECT_DIR/channel_switch.py"
old_unit=/etc/systemd/system/mihomo-channel-switch.service
[ -f "$old_switcher" ] && cp -p "$old_switcher" "$backup_dir/old-channel-switch.py" || true
[ -f "$old_unit" ] && cp -p "$old_unit" "$backup_dir/old-channel-switch.service" || true
[ -f "$GUARDIAN_ROOT/data/state.json" ] && cp -p "$GUARDIAN_ROOT/data/state.json" "$backup_dir/guardian-state.json" || true
[ -f "$GUARDIAN_ROOT/guardian.yaml" ] && cp -p "$GUARDIAN_ROOT/guardian.yaml" "$backup_dir/guardian-config.yaml" || true
[ -d "$GUARDIAN_ROOT/data/ipquality" ] && cp -a "$GUARDIAN_ROOT/data/ipquality" "$backup_dir/quality-store" || true
[ -f "$GUARDIAN_ROOT/logs/quality.jsonl" ] && cp -p "$GUARDIAN_ROOT/logs/quality.jsonl" "$backup_dir/quality.jsonl" || true
cat >"$backup_dir/manifest" <<EOF
compose=$COMPOSE_PATH
project_dir=$PROJECT_DIR
service=$DISCOVERED_SERVICE
config_path=$CONFIG_PATH
config_present=1
old_switcher=$old_switcher
old_switcher_present=$([ -f "$old_switcher" ] && echo 1 || echo 0)
old_unit=$old_unit
old_unit_present=$([ -f "$old_unit" ] && echo 1 || echo 0)
guardian_state=$GUARDIAN_ROOT/data/state.json
guardian_state_present=$([ -f "$GUARDIAN_ROOT/data/state.json" ] && echo 1 || echo 0)
guardian_config=$GUARDIAN_ROOT/guardian.yaml
guardian_config_present=$([ -f "$GUARDIAN_ROOT/guardian.yaml" ] && echo 1 || echo 0)
quality_store=$GUARDIAN_ROOT/data/ipquality
quality_store_present=$([ -d "$GUARDIAN_ROOT/data/ipquality" ] && echo 1 || echo 0)
quality_log=$GUARDIAN_ROOT/logs/quality.jsonl
quality_log_present=$([ -f "$GUARDIAN_ROOT/logs/quality.jsonl" ] && echo 1 || echo 0)
EOF

build_binary
install -m 0755 "$REPO_DIR/dist/guardian" "$GUARDIAN_ROOT/bin/guardian"
install -m 0755 "$REPO_DIR/deploy/start-guardian.sh" "$GUARDIAN_ROOT/start-guardian.sh"
guardian_template="$REPO_DIR/configs/guardian.example.yaml"
[ -f "$GUARDIAN_ROOT/guardian.yaml" ] && guardian_template="$GUARDIAN_ROOT/guardian.yaml"
PYTHONPATH="$REPO_DIR" python3 - "$guardian_template" "$GUARDIAN_ROOT/guardian.yaml" "$COMPOSE_PATH" "$CONFIG_PATH" "$QUALITY_TARGETS_FILE" <<'PY'
import json
import sys
from pathlib import Path

from scripts.discover import load_discovery, render_guardian_config

template = Path(sys.argv[1]).read_text(encoding="utf-8")
discovery = load_discovery(sys.argv[3], sys.argv[4], None)
quality_targets = json.loads(Path(sys.argv[5]).read_text(encoding="utf-8"))
Path(sys.argv[2]).write_text(
    render_guardian_config(template, discovery, quality_targets=quality_targets),
    encoding="utf-8",
)
Path(sys.argv[2]).chmod(0o640)
PY

SECRET_SOURCE=${MIHOMO_SECRET_FILE:-}
if [ -z "$SECRET_SOURCE" ]; then SECRET_SOURCE=$(json_value host_secret_file || true); fi
if [ -z "$SECRET_SOURCE" ] && [ -f "$PROJECT_DIR/.controller_secret" ]; then SECRET_SOURCE=$PROJECT_DIR/.controller_secret; fi
if [ -n "$SECRET_SOURCE" ] && [ -f "$SECRET_SOURCE" ]; then
    install -m 0640 "$SECRET_SOURCE" "$GUARDIAN_ROOT/controller_secret"
else
    python3 - "$CONFIG_PATH" "$GUARDIAN_ROOT/controller_secret" <<'PY'
import re
import sys
from pathlib import Path

text = Path(sys.argv[1]).read_text(encoding="utf-8")
match = re.search(r"^\s*secret:\s*(?:['\"]([^'\"]+)['\"]|([^#\s]+))\s*(?:#.*)?$", text, re.M)
if not match or not (match.group(1) or match.group(2)):
    raise SystemExit("mihomo controller secret file not found and config has no secret")
Path(sys.argv[2]).write_text((match.group(1) or match.group(2)).strip() + "\n", encoding="utf-8")
Path(sys.argv[2]).chmod(0o640)
PY
fi

PYTHONPATH="$REPO_DIR" python3 - "$CONFIG_PATH" "$DISCOVERED_MAIN" "$DISCOVERED_BACKUP" "$QUALITY_TARGETS_FILE" <<'PY'
import json
import sys
from pathlib import Path
from scripts.mihomo_config_patch import patch_file

patch_file(
    sys.argv[1],
    sys.argv[2],
    sys.argv[3],
    json.loads(Path(sys.argv[4]).read_text(encoding="utf-8")),
)
PY

# Start conservatively. Auto mode is enabled only after the read-only smoke
# test below, so a bad public route cannot trigger a channel write at startup.
sed -i '0,/^  mode: auto$/s//  mode: observe/' "$GUARDIAN_ROOT/guardian.yaml"

printf '%s' "$PATCHED_COMPOSE" >"$COMPOSE_PATH.guardian.tmp"
mv "$COMPOSE_PATH.guardian.tmp" "$COMPOSE_PATH"

if command -v systemctl >/dev/null 2>&1 && [ -f "$old_unit" ]; then
    systemctl stop mihomo-channel-switch.service || true
    systemctl disable mihomo-channel-switch.service || true
fi

rollback_or_abort() {
    if ! "$REPO_DIR/scripts/rollback.sh" --guardian-root "$GUARDIAN_ROOT" --container "$CONTAINER"; then
        echo "CRITICAL: automatic rollback failed; inspect $GUARDIAN_ROOT/backups and keep mihomo under manual observation" >&2
        exit 2
    fi
}

if ! (cd "$PROJECT_DIR" && docker compose -f "$COMPOSE_PATH" up -d --force-recreate "$DISCOVERED_SERVICE"); then
    echo "container recreation failed; invoking rollback" >&2
    rollback_or_abort
    exit 1
fi

wait_for_runtime() {
    attempt=0
    while [ "$attempt" -lt 60 ]; do
        running=$(docker inspect --format '{{.State.Running}}' "$CONTAINER" 2>/dev/null || true)
        if [ "$running" = true ] && docker top "$CONTAINER" -eo pid,comm,args 2>/dev/null | grep -q '[m]ihomo' && docker top "$CONTAINER" -eo pid,comm,args 2>/dev/null | grep -q '[g]uardian'; then
            return 0
        fi
        attempt=$((attempt + 1))
        sleep 1
    done
    return 1
}

if ! wait_for_runtime; then
    echo "mihomo/guardian did not become healthy; invoking rollback" >&2
    rollback_or_abort
    exit 1
fi

echo "guardian started in observe mode; mihomo remains the production process"
if [ "$OBSERVE" -eq 0 ]; then
    # The smoke test is intentionally read-only and expects the normal 401/403
    # response from an unauthenticated vendor endpoint as reachability.
    proxy_internal_port=$(printf '%s' "$DISCOVERED_PROXY" | sed 's/.*://')
    proxy_host_port=$(docker port "$CONTAINER" "$proxy_internal_port/tcp" 2>/dev/null | sed -n '1{s/.*://;p;}' || true)
    if [ -z "$proxy_host_port" ]; then
        echo "proxy host port is not published; keeping observe mode" >&2
        exit 1
    fi
    proxy_scheme=${DISCOVERED_PROXY%%:*}
    case "$proxy_scheme" in
        http|https)
            smoke_code=$(curl --noproxy '' --silent --show-error --output /dev/null --write-out '%{http_code}' --max-time 12 \
                --proxy "http://127.0.0.1:$proxy_host_port" https://api.openai.com/v1/models || true)
            ;;
        socks5|socks5h)
            smoke_code=$(curl --noproxy '' --silent --show-error --output /dev/null --write-out '%{http_code}' --max-time 12 \
                --socks5-hostname "127.0.0.1:$proxy_host_port" https://api.openai.com/v1/models || true)
            ;;
        *)
            echo "unsupported discovered proxy scheme: $proxy_scheme" >&2
            rollback_or_abort
            exit 1
            ;;
    esac
    case "$smoke_code" in
        2*|3*|4*) ;;
        *)
            echo "proxied smoke test failed (HTTP $smoke_code); invoking rollback" >&2
            rollback_or_abort
            exit 1
            ;;
    esac
    sed -i '0,/^  mode: observe$/s//  mode: auto/' "$GUARDIAN_ROOT/guardian.yaml"
    guardian_pid=$(docker top "$CONTAINER" -eo pid,comm,args 2>/dev/null | awk '$2 == "guardian" {print $1; exit}')
    if [ -n "$guardian_pid" ]; then
        docker exec "$CONTAINER" sh -c "kill -TERM $guardian_pid" || true
    fi
    echo "smoke=ok mode=auto; only guardian was reloaded"
else
    echo "mode=observe (requested); no automatic switching enabled"
fi

echo "install=ok backup=$backup_dir"
