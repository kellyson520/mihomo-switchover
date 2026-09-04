#!/bin/sh
set -eu

backup_dir_arg=
lock_held=0

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
        --backup-dir)
            [ "$#" -ge 2 ] || { echo "--backup-dir requires a value" >&2; exit 2; }
            backup_dir_arg=$2
            shift 2
            ;;
        --lock-held)
            lock_held=1
            shift
            ;;
        *)
            echo "unknown option: $1" >&2
            exit 2
            ;;
    esac
done

[ -n "$guardian_root" ] || guardian_root=/opt/mihomo-cliproxy/guardian
guardian_root=$(CDPATH= cd -- "$guardian_root" && pwd) || {
    echo "guardian root is not an accessible directory: $guardian_root" >&2
    exit 1
}
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required" >&2; exit 1; }
command -v mktemp >/dev/null 2>&1 || { echo "mktemp is required" >&2; exit 1; }
command -v flock >/dev/null 2>&1 || { echo "flock is required" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
backup_root="$guardian_root/backups"
[ -d "$backup_root" ] || { echo "backup directory not found: $backup_root" >&2; exit 1; }
backup_root=$(CDPATH= cd -- "$backup_root" && pwd) || {
    echo "backup directory is not accessible: $backup_root" >&2
    exit 1
}

mkdir -p "$guardian_root/run"
LOCK_FILE="$guardian_root/run/guardian-update.lock"
if [ "$lock_held" -eq 0 ]; then
    exec 9>"$LOCK_FILE"
    flock -n 9 || { echo "another guardian install/update/rollback is already running" >&2; exit 1; }
else
    [ "${GUARDIAN_LOCK_HELD:-}" = 1 ] || {
        echo "--lock-held is reserved for the installer" >&2
        exit 2
    }
fi

manifest_key_count() {
    key=$1
    count=$(grep -c "^${key}=" "$manifest" 2>/dev/null || true)
    printf '%s\n' "$count"
}

manifest_value() {
    key=$1
    sed -n "s/^${key}=//p" "$manifest" | head -n 1
}

manifest_has_key() {
    [ "$(manifest_key_count "$1")" -eq 1 ]
}

manifest_required_value() {
    key=$1
    manifest_has_key "$key" || return 1
    value=$(manifest_value "$key")
    [ -n "$value" ] || return 1
    case "$value" in
        *"$(printf '\r')"*) return 1 ;;
    esac
}

manifest_flag_is_valid() {
    key=$1
    count=$(manifest_key_count "$key")
    if [ "$count" -eq 0 ]; then
        return 0
    fi
    [ "$count" -eq 1 ] || return 1
    value=$(manifest_value "$key")
    case "$value" in
        0|1) return 0 ;;
        *) return 1 ;;
    esac
}

manifest_absolute_value_is_valid() {
    key=$1
    manifest_required_value "$key" || return 1
    value=$(manifest_value "$key")
    case "$value" in
        /*) ;;
        *) return 1 ;;
    esac
    case "$value" in
        *"/../"*|*/..) return 1 ;;
    esac
}

manifest_fixed_path_is_valid() {
    key=$1
    expected=$2
    if ! manifest_has_key "$key"; then
        return 0
    fi
    value=$(manifest_value "$key")
    [ "$value" = "$expected" ] || return 1
    manifest_absolute_value_is_valid "$key"
}

manifest_artifact_is_valid() {
    artifact=$1
    [ -f "$backup_dir/$artifact" ] && [ ! -L "$backup_dir/$artifact" ]
}

manifest_optional_artifact_is_valid() {
    flag_key=$1
    artifact=$2
    if ! manifest_has_key "$flag_key" || [ "$(manifest_value "$flag_key")" = 0 ]; then
        return 0
    fi
    manifest_artifact_is_valid "$artifact"
}

manifest_is_valid() {
    manifest=$1
    backup_dir=${manifest%/manifest}
    [ -f "$manifest" ] && [ ! -L "$manifest" ] || return 1
    [ -d "$backup_dir" ] && [ ! -L "$backup_dir" ] || return 1
    manifest_required_value compose || return 1
    manifest_required_value project_dir || return 1
    manifest_required_value config_path || return 1
    manifest_required_value config_present || return 1
    manifest_flag_is_valid config_present || return 1
    manifest_flag_is_valid old_switcher_present || return 1
    manifest_flag_is_valid old_unit_present || return 1
    manifest_flag_is_valid guardian_state_present || return 1
    manifest_flag_is_valid guardian_config_present || return 1
    manifest_flag_is_valid guardian_launcher_present || return 1
    manifest_flag_is_valid guardian_secret_present || return 1
    manifest_flag_is_valid guardian_binary_present || return 1

    compose=$(manifest_value compose)
    project_dir=$(manifest_value project_dir)
    config_path=$(manifest_value config_path)
    manifest_absolute_value_is_valid compose || return 1
    manifest_absolute_value_is_valid project_dir || return 1
    case "$compose" in "$project_dir"/*) ;; *) return 1 ;; esac
    manifest_absolute_value_is_valid config_path || return 1
    [ -d "$project_dir" ] || return 1
    manifest_artifact_is_valid compose.yml || return 1
    if [ "$(manifest_value config_present)" = 1 ]; then
        manifest_artifact_is_valid mihomo-config.yaml || return 1
    fi
    service_count=$(manifest_key_count service)
    [ "$service_count" -le 1 ] || return 1
    if [ "$service_count" -eq 1 ]; then
        service=$(manifest_value service)
        [ -n "$service" ] || return 1
        case "$service" in
            */*|*"$(printf '\r')"*) return 1 ;;
        esac
    fi
    manifest_fixed_path_is_valid old_switcher "$project_dir/channel_switch.py" || return 1
    manifest_fixed_path_is_valid old_unit "/etc/systemd/system/mihomo-channel-switch.service" || return 1
    manifest_fixed_path_is_valid guardian_state "$guardian_root/data/state.json" || return 1
    manifest_fixed_path_is_valid guardian_config "$guardian_root/guardian.yaml" || return 1
    manifest_fixed_path_is_valid guardian_launcher "$guardian_root/start-guardian.sh" || return 1
    manifest_fixed_path_is_valid guardian_secret "$guardian_root/controller_secret" || return 1
    manifest_fixed_path_is_valid guardian_binary "$guardian_root/bin/guardian" || return 1
    for target_key in old_switcher old_unit guardian_state guardian_config guardian_launcher guardian_secret guardian_binary; do
        flag_key=${target_key}_present
        if manifest_has_key "$flag_key"; then
            manifest_has_key "$target_key" || return 1
        fi
    done
    manifest_optional_artifact_is_valid old_switcher_present old-channel-switch.py || return 1
    manifest_optional_artifact_is_valid old_unit_present old-channel-switch.service || return 1
    manifest_optional_artifact_is_valid guardian_state_present guardian-state.json || return 1
    manifest_optional_artifact_is_valid guardian_config_present guardian-config.yaml || return 1
    manifest_optional_artifact_is_valid guardian_launcher_present start-guardian.sh || return 1
    manifest_optional_artifact_is_valid guardian_secret_present controller-secret || return 1
    if manifest_has_key guardian_binary_present && [ "$(manifest_value guardian_binary_present)" = 1 ]; then
        manifest_artifact_is_valid guardian-binary || return 1
        manifest_required_value guardian_binary_hash || return 1
        expected_hash=$(manifest_value guardian_binary_hash)
        [ "$(printf '%s' "$expected_hash" | wc -c | tr -d ' ')" -eq 64 ] || return 1
        case "$expected_hash" in
            *[!0123456789abcdef]*) return 1 ;;
        esac
        [ "$(sha256sum "$backup_dir/guardian-binary" | awk '{print $1}')" = "$expected_hash" ] || return 1
    fi
    return 0
}

select_backup() {
    if [ -n "$backup_dir_arg" ]; then
        [ -d "$backup_dir_arg" ] && [ ! -L "$backup_dir_arg" ] || return 1
        backup_dir_arg=$(CDPATH= cd -- "$backup_dir_arg" && pwd) || return 1
        case "$backup_dir_arg" in
            "$backup_root"/*) ;;
            *) return 1 ;;
        esac
        backup="$backup_dir_arg/manifest"
        manifest_is_valid "$backup" || return 1
        printf '%s\n' "$backup"
        return 0
    fi
    find "$backup_root" -mindepth 2 -maxdepth 2 -type f -name manifest -printf '%T@ %p\n' 2>/dev/null \
        | sort -rn \
        | while read -r _ manifest; do
            if manifest_is_valid "$manifest"; then
                printf '%s\n' "$manifest"
                break
            fi
        done
}

backup=$(select_backup || true)
[ -n "$backup" ] || { echo "no complete and valid backup manifest found" >&2; exit 1; }
backup_dir=${backup%/manifest}
manifest=$backup
manifest_is_valid "$manifest" || { echo "backup manifest validation failed" >&2; exit 1; }

get_value() {
    key=$1
    manifest_value "$key"
}

compose=$(get_value compose)
project_dir=$(get_value project_dir)
service=$(get_value service)
[ -n "$service" ] || service=$container
config_path=$(get_value config_path)
config_present=$(get_value config_present)
[ -n "$compose" ] && [ -n "$project_dir" ] && [ -n "$config_path" ] || {
    echo "backup manifest is incomplete" >&2
    exit 1
}

echo "using backup: $backup_dir"

# Preserve current quality history and logs before restoring deployment
# artifacts. Rollback intentionally does not delete or replace live quality
# data: reports, baselines, scan progress, and audit records remain available
# for diagnosis after the code/compose rollback.
preserved_dir="$backup_root/rollback-preserved-$(date -u +%Y%m%dT%H%M%SZ)-$$"
mkdir -p "$preserved_dir"
if [ -d "$guardian_root/data/ipquality" ]; then
    cp -a "$guardian_root/data/ipquality" "$preserved_dir/quality-store"
fi
if [ -f "$guardian_root/logs/quality.jsonl" ]; then
    cp -p "$guardian_root/logs/quality.jsonl" "$preserved_dir/quality.jsonl"
fi

transaction_dir=$(mktemp -d "$backup_root/.rollback.XXXXXX")
mkdir -p "$transaction_dir/stage" "$transaction_dir/current"
COMMIT_STARTED=0
COMMIT_COMPLETED=0
RUNTIME_RESTORE_ATTEMPTED=0
RESTORE_IN_PROGRESS=0
EXITING=0

cleanup() {
    [ -z "$transaction_dir" ] || rm -rf -- "$transaction_dir"
}

restore_target() {
    name=$1
    target=$2
    was_present=$3
    if [ "$was_present" = 1 ]; then
        snapshot="$transaction_dir/current/$name"
        [ -f "$snapshot" ] || [ -L "$snapshot" ] || return 1
        [ -d "$(dirname -- "$target")" ] || return 1
        temp=$(mktemp "$target.rollback.XXXXXX") || return 1
        if ! cp -a "$snapshot" "$temp"; then
            rm -f -- "$temp"
            return 1
        fi
        if ! mv -f -- "$temp" "$target"; then
            rm -f -- "$temp"
            return 1
        fi
    else
        if [ -e "$target" ] || [ -L "$target" ]; then
            [ ! -d "$target" ] || return 1
            rm -f -- "$target" || return 1
        fi
    fi
}

restore_previous_files() {
    restore_failed=0
    if ! restore_target compose "$compose" "$compose_current_present"; then restore_failed=1; fi
    if [ "$config_managed" -eq 1 ] && ! restore_target config "$config_path" "$config_current_present"; then restore_failed=1; fi
    if [ "$old_switcher_managed" -eq 1 ] && ! restore_target old_switcher "$old_switcher" "$old_switcher_current_present"; then restore_failed=1; fi
    if [ "$old_unit_managed" -eq 1 ] && ! restore_target old_unit "$old_unit" "$old_unit_current_present"; then restore_failed=1; fi
    if [ "$guardian_state_managed" -eq 1 ] && ! restore_target guardian_state "$guardian_state" "$guardian_state_current_present"; then restore_failed=1; fi
    if [ "$guardian_config_managed" -eq 1 ] && ! restore_target guardian_config "$guardian_config" "$guardian_config_current_present"; then restore_failed=1; fi
    if [ "$guardian_launcher_managed" -eq 1 ] && ! restore_target guardian_launcher "$guardian_launcher" "$guardian_launcher_current_present"; then restore_failed=1; fi
    if [ "$guardian_secret_managed" -eq 1 ] && ! restore_target guardian_secret "$guardian_secret" "$guardian_secret_current_present"; then restore_failed=1; fi
    if [ "$guardian_binary_managed" -eq 1 ] && ! restore_target guardian_binary "$guardian_binary" "$guardian_binary_current_present"; then restore_failed=1; fi
    return "$restore_failed"
}

restore_runtime() {
    [ "$RUNTIME_RESTORE_ATTEMPTED" -eq 1 ] || return 0
    RUNTIME_RESTORE_ATTEMPTED=2
    if ! docker compose -f "$compose" up -d --force-recreate "$service"; then
        echo "CRITICAL: runtime restore failed after rollback error" >&2
        return 1
    fi
    return 0
}

wait_for_runtime() {
    attempt=0
    while [ "$attempt" -lt 60 ]; do
        container_status=$(docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true)
        if [ "$container_status" = running ] \
            && docker exec "$container" ps -eo comm 2>/dev/null \
            | awk 'NR > 1 && $1 == "mihomo" { found = 1 } END { exit found ? 0 : 1 }'; then
            return 0
        fi
        attempt=$((attempt + 1))
        sleep 1
    done
    return 1
}

on_exit() {
    status=$?
    if [ "$EXITING" -eq 1 ]; then
        exit "$status"
    fi
    EXITING=1
    if [ "$status" -ne 0 ] && [ "$COMMIT_STARTED" -eq 1 ] && [ "$RESTORE_IN_PROGRESS" -eq 0 ]; then
        RESTORE_IN_PROGRESS=1
        files_restored=1
        if ! restore_previous_files; then
            echo "CRITICAL: rollback transaction could not restore previous host files" >&2
            status=2
            files_restored=0
        fi
        if [ "$files_restored" -eq 1 ] && ! restore_runtime; then
            status=2
        fi
    fi
    cleanup
    exit "$status"
}

trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

snapshot_target() {
    name=$1
    target=$2
    snapshot_present=0
    [ -d "$(dirname -- "$target")" ] || return 1
    if [ -e "$target" ] || [ -L "$target" ]; then
        [ ! -d "$target" ] || return 1
        cp -a "$target" "$transaction_dir/current/$name" || return 1
        snapshot_present=1
    fi
}

stage_target() {
    name=$1
    target=$2
    artifact=$3
    present=$4
    snapshot_target "$name" "$target" || return 1
    if [ "$present" = 1 ]; then
        [ -f "$artifact" ] && [ ! -L "$artifact" ] || return 1
        cp -p "$artifact" "$transaction_dir/stage/$name" || return 1
    fi
}

commit_target() {
    name=$1
    target=$2
    present=$3
    if [ "$present" = 1 ]; then
        temp=$(mktemp "$target.rollback.XXXXXX") || return 1
        if ! cp -p "$transaction_dir/stage/$name" "$temp"; then
            rm -f -- "$temp"
            return 1
        fi
        if ! mv -f -- "$temp" "$target"; then
            rm -f -- "$temp"
            return 1
        fi
    elif [ -e "$target" ] || [ -L "$target" ]; then
        [ ! -d "$target" ] || return 1
        rm -f -- "$target" || return 1
    fi
}

config_managed=1
old_switcher_managed=0
old_unit_managed=0
guardian_state_managed=0
guardian_config_managed=0
guardian_launcher_managed=0
guardian_secret_managed=0
guardian_binary_managed=0

old_switcher=$(get_value old_switcher)
old_unit=$(get_value old_unit)
guardian_state=$(get_value guardian_state)
guardian_config=$(get_value guardian_config)
guardian_launcher=$(get_value guardian_launcher)
guardian_secret=$(get_value guardian_secret)
guardian_binary=$(get_value guardian_binary)

if manifest_has_key old_switcher_present; then old_switcher_managed=1; fi
if manifest_has_key old_unit_present; then old_unit_managed=1; fi
if manifest_has_key guardian_state_present; then guardian_state_managed=1; fi
if manifest_has_key guardian_config_present; then guardian_config_managed=1; fi
if manifest_has_key guardian_launcher_present; then guardian_launcher_managed=1; fi
if manifest_has_key guardian_secret_present; then guardian_secret_managed=1; fi
if manifest_has_key guardian_binary_present; then guardian_binary_managed=1; fi

old_switcher_present=$(get_value old_switcher_present)
old_unit_present=$(get_value old_unit_present)
guardian_state_present=$(get_value guardian_state_present)
guardian_config_present=$(get_value guardian_config_present)
guardian_launcher_present=$(get_value guardian_launcher_present)
guardian_secret_present=$(get_value guardian_secret_present)
guardian_binary_present=$(get_value guardian_binary_present)

stage_target compose "$compose" "$backup_dir/compose.yml" 1
compose_current_present=$snapshot_present
stage_target config "$config_path" "$backup_dir/mihomo-config.yaml" "$config_present"
config_current_present=$snapshot_present
if [ "$old_switcher_managed" -eq 1 ]; then
    stage_target old_switcher "$old_switcher" "$backup_dir/old-channel-switch.py" "$old_switcher_present"
    old_switcher_current_present=$snapshot_present
fi
if [ "$old_unit_managed" -eq 1 ]; then
    stage_target old_unit "$old_unit" "$backup_dir/old-channel-switch.service" "$old_unit_present"
    old_unit_current_present=$snapshot_present
fi
if [ "$guardian_state_managed" -eq 1 ]; then
    stage_target guardian_state "$guardian_state" "$backup_dir/guardian-state.json" "$guardian_state_present"
    guardian_state_current_present=$snapshot_present
fi
if [ "$guardian_config_managed" -eq 1 ]; then
    stage_target guardian_config "$guardian_config" "$backup_dir/guardian-config.yaml" "$guardian_config_present"
    guardian_config_current_present=$snapshot_present
fi
if [ "$guardian_launcher_managed" -eq 1 ]; then
    stage_target guardian_launcher "$guardian_launcher" "$backup_dir/start-guardian.sh" "$guardian_launcher_present"
    guardian_launcher_current_present=$snapshot_present
fi
if [ "$guardian_secret_managed" -eq 1 ]; then
    stage_target guardian_secret "$guardian_secret" "$backup_dir/controller-secret" "$guardian_secret_present"
    guardian_secret_current_present=$snapshot_present
fi
if [ "$guardian_binary_managed" -eq 1 ]; then
    stage_target guardian_binary "$guardian_binary" "$backup_dir/guardian-binary" "$guardian_binary_present"
    guardian_binary_current_present=$snapshot_present
fi

if [ "$guardian_binary_managed" -eq 1 ] && [ "$guardian_binary_present" = 1 ]; then
    expected_hash=$(get_value guardian_binary_hash)
    staged_hash=$(sha256sum "$transaction_dir/stage/guardian_binary" | awk '{print $1}')
    [ "$staged_hash" = "$expected_hash" ] || {
        echo "backup is incomplete: guardian binary staged hash mismatch" >&2
        exit 1
    }
fi

COMMIT_STARTED=1
commit_target compose "$compose" 1
commit_target config "$config_path" "$config_present"
[ "$old_switcher_managed" -eq 0 ] || commit_target old_switcher "$old_switcher" "$old_switcher_present"
[ "$old_unit_managed" -eq 0 ] || commit_target old_unit "$old_unit" "$old_unit_present"
[ "$guardian_state_managed" -eq 0 ] || commit_target guardian_state "$guardian_state" "$guardian_state_present"
[ "$guardian_config_managed" -eq 0 ] || commit_target guardian_config "$guardian_config" "$guardian_config_present"
[ "$guardian_launcher_managed" -eq 0 ] || commit_target guardian_launcher "$guardian_launcher" "$guardian_launcher_present"
[ "$guardian_secret_managed" -eq 0 ] || commit_target guardian_secret "$guardian_secret" "$guardian_secret_present"
[ "$guardian_binary_managed" -eq 0 ] || commit_target guardian_binary "$guardian_binary" "$guardian_binary_present"
COMMIT_COMPLETED=1

docker compose -f "$compose" config >/dev/null
RUNTIME_RESTORE_ATTEMPTED=1
docker compose -f "$compose" up -d --force-recreate "$service"
if ! wait_for_runtime; then
    echo "rollback restored files but mihomo did not become healthy" >&2
    exit 1
fi
if command -v systemctl >/dev/null 2>&1 && [ "$old_unit_managed" -eq 1 ] && [ "$old_unit_present" = 1 ]; then
    systemctl daemon-reload
    systemctl enable --now mihomo-channel-switch.service
fi

echo "rollback restored compose and legacy switcher; quality history/logs were retained (preserved=$preserved_dir)"
