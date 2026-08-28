#!/bin/sh
set -eu

if [ "${1:-}" != "--read-only" ]; then
    echo "usage: $0 --read-only [--container NAME]" >&2
    exit 2
fi
shift

container=${MIHOMO_CONTAINER:-mihomo-cliproxy}
while [ "$#" -gt 0 ]; do
    case "$1" in
        --container)
            [ "$#" -ge 2 ] || { echo "--container requires a value" >&2; exit 2; }
            container=$2
            shift 2
            ;;
        *)
            echo "unknown option: $1" >&2
            exit 2
            ;;
    esac
done

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
docker inspect "$container" >/dev/null 2>&1 || { echo "container not found: $container" >&2; exit 1; }

status=$(docker inspect --format '{{.State.Status}}' "$container")
pid1=$(docker inspect --format '{{.Path}} {{join .Args " "}}' "$container")
mount_json=$(docker inspect --format '{{json .Mounts}}' "$container")
guardian_root=$(python3 - "$mount_json" <<'PY'
import json
import sys

mounts = json.loads(sys.argv[1])
for mount in mounts:
    if mount.get("Destination") == "/guardian/guardian.yaml":
        source = mount.get("Source", "")
        print(source.rsplit("/", 1)[0] if "/" in source else source)
        break
PY
)

echo "container=$container"
echo "container_status=$status"
echo "pid1=$pid1"
echo "guardian_root=${guardian_root:-unknown}"
if [ "$status" = running ]; then
    echo "processes:"
    docker top "$container" -eo pid,ppid,comm,args 2>/dev/null || true
fi

if [ -n "${guardian_root:-}" ] && [ -f "$guardian_root/guardian.yaml" ]; then
    set +e
    ports=$(python3 - "$guardian_root/guardian.yaml" <<'PY'
import re
import sys

text = open(sys.argv[1], encoding="utf-8").read()
api = re.search(r"^\s+api:\s*[^:]+://[^:]+:(\d+)\s*$", text, re.M)
proxy = re.search(r"^\s+proxy:\s*[^:]+://[^:]+:(\d+)\s*$", text, re.M)
print((api.group(1) if api else "") + " " + (proxy.group(1) if proxy else ""))
PY
)
    groups=$(python3 - "$guardian_root/guardian.yaml" <<'PY'
import re
import sys

text = open(sys.argv[1], encoding="utf-8").read()
section = re.search(r"(?ms)^groups:\s*\n(.*?)(?=^[A-Za-z0-9_-]+:|\Z)", text)
values = {}
if section:
    for key, value in re.findall(r"^  (channel|main|backup):\s*(\S+)", section.group(1), re.M):
        values[key] = value.strip("'\"")
print(values.get("channel", ""), values.get("main", ""), values.get("backup", ""))
PY
)
    channel_group=$(printf '%s' "$groups" | awk '{print $1}')
    main_group=$(printf '%s' "$groups" | awk '{print $2}')
    backup_group=$(printf '%s' "$groups" | awk '{print $3}')
    api_port=$(printf '%s' "$ports" | awk '{print $1}')
    proxy_port=$(printf '%s' "$ports" | awk '{print $2}')
    secret_file="$guardian_root/controller_secret"
    if [ -n "$api_port" ] && [ -f "$secret_file" ] && [ "$status" = running ]; then
        host_api=$(docker port "$container" "$api_port/tcp" 2>/dev/null | sed -n '1{s/.*://;p;}' || true)
        if [ -n "$host_api" ]; then
            secret=$(tr -d '\r\n' <"$secret_file")
            channel_path=$(python3 - "$channel_group" <<'PY'
from urllib.parse import quote
import sys
print(quote(sys.argv[1], safe=""))
PY
)
            channel_json=$(curl --noproxy '*' --silent --show-error --fail --max-time 3 \
                -H "Authorization: Bearer $secret" "http://127.0.0.1:$host_api/proxies/$channel_path" 2>/dev/null || true)
            if [ -n "$channel_json" ]; then
                python3 - "$channel_json" <<'PY'
import json
import sys

data = json.loads(sys.argv[1])
print("channel_now=" + str(data.get("now", "")))
PY
                for group in "$main_group" "$backup_group"; do
                    group_path=$(python3 - "$group" <<'PY'
from urllib.parse import quote
import sys
print(quote(sys.argv[1], safe=""))
PY
)
                    group_json=$(curl --noproxy '*' --silent --show-error --fail --max-time 3 \
                        -H "Authorization: Bearer $secret" "http://127.0.0.1:$host_api/proxies/$group_path" 2>/dev/null || true)
                    if [ -n "$group_json" ]; then
                        python3 - "$group" "$group_json" <<'PY'
import json
import sys

data = json.loads(sys.argv[2])
print(sys.argv[1] + "_now=" + str(data.get("now", "")))
PY
                    fi
                done
            else
                echo "api_health=unavailable"
            fi
        else
            echo "api_health=host-port-not-published"
        fi
    else
        echo "api_health=not-configured"
    fi
    set -e
    state_file="$guardian_root/data/state.json"
    if [ -f "$state_file" ]; then
        echo "state_age=$(stat -c '%y' "$state_file" 2>/dev/null || true)"
    else
        echo "state=missing"
    fi
    log_file="$guardian_root/logs/guardian.jsonl"
    if [ -f "$log_file" ]; then
        echo "last_events:"
        tail -n 20 "$log_file"
    fi
fi
