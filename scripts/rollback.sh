#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "rollback must run as root" >&2
    exit 1
fi

container=${MIHOMO_CONTAINER:-mihomo-cliproxy}
guardian_root=${GUARDIAN_ROOT:-}
while [ "$#" -gt 0 ]; do
    case "$1" in
        --container)
            [ "$#" -ge 2 ] || { echo "--container requires a value" >&2; exit 2; }
            container=$2
            shift 2
            ;;
        --guardian-root)
            [ "$#" -ge 2 ] || { echo "--guardian-root requires a value" >&2; exit 2; }
            guardian_root=$2
            shift 2
            ;;
        *)
            echo "unknown option: $1" >&2
            exit 2
            ;;
    esac
done

[ -n "$guardian_root" ] || guardian_root=/opt/mihomo-cliproxy/guardian
backup_root="$guardian_root/backups"
[ -d "$backup_root" ] || { echo "backup directory not found: $backup_root" >&2; exit 1; }

backup=$(find "$backup_root" -mindepth 2 -maxdepth 2 -type f -name manifest -printf '%T@ %p\n' 2>/dev/null \
    | sort -rn | awk 'NR==1 {print $2}')
[ -n "$backup" ] || { echo "no complete backup manifest found" >&2; exit 1; }
backup_dir=${backup%/manifest}

get_value() {
    key=$1
    sed -n "s/^${key}=//p" "$backup" | head -n 1
}

compose=$(get_value compose)
project_dir=$(get_value project_dir)
service=$(get_value service)
[ -n "$service" ] || service=$container
[ -f "$backup_dir/compose.yml" ] || { echo "backup is incomplete: compose.yml" >&2; exit 1; }
[ -n "$compose" ] && [ -n "$project_dir" ] || { echo "backup manifest is incomplete" >&2; exit 1; }

echo "using backup: $backup_dir"
cp -p "$backup_dir/compose.yml" "$compose.rollback.tmp"
mv "$compose.rollback.tmp" "$compose"
if [ "$(get_value config_present)" = 1 ] && [ -f "$backup_dir/mihomo-config.yaml" ]; then
    config_path=$(get_value config_path)
    cp -p "$backup_dir/mihomo-config.yaml" "$config_path.rollback.tmp"
    mv "$config_path.rollback.tmp" "$config_path"
fi
if [ "$(get_value old_switcher_present)" = 1 ] && [ -f "$backup_dir/old-channel-switch.py" ]; then
    old_switcher=$(get_value old_switcher)
    cp -p "$backup_dir/old-channel-switch.py" "$old_switcher.rollback.tmp"
    mv "$old_switcher.rollback.tmp" "$old_switcher"
fi
if [ "$(get_value old_unit_present)" = 1 ] && [ -f "$backup_dir/old-channel-switch.service" ]; then
    old_unit=$(get_value old_unit)
    cp -p "$backup_dir/old-channel-switch.service" "$old_unit.rollback.tmp"
    mv "$old_unit.rollback.tmp" "$old_unit"
fi
if [ "$(get_value guardian_state_present)" = 1 ] && [ -f "$backup_dir/guardian-state.json" ]; then
    state_path=$(get_value guardian_state)
    cp -p "$backup_dir/guardian-state.json" "$state_path.rollback.tmp"
    mv "$state_path.rollback.tmp" "$state_path"
fi
if [ "$(get_value guardian_config_present)" = 1 ] && [ -f "$backup_dir/guardian-config.yaml" ]; then
    guardian_config=$(get_value guardian_config)
    cp -p "$backup_dir/guardian-config.yaml" "$guardian_config.rollback.tmp"
    mv "$guardian_config.rollback.tmp" "$guardian_config"
fi

docker compose -f "$compose" config >/dev/null
docker compose -f "$compose" up -d --force-recreate "$service"
if command -v systemctl >/dev/null 2>&1 && [ "$(get_value old_unit_present)" = 1 ]; then
    systemctl daemon-reload
    systemctl enable --now mihomo-channel-switch.service
fi
echo "rollback restored compose and legacy switcher; guardian data/logs/backups were retained"
