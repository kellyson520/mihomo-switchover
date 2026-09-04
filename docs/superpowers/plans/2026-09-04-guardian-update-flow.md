# Guardian Update Flow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make guardian reinjection safe and make later binary updates atomic, persistent, rollbackable, and independent from the production Mihomo process.

**Architecture:** The Compose injector mounts the persistent guardian `bin` directory instead of a single binary inode. A first migration is explicit and may recreate the Compose service during a maintenance window; subsequent `update-guardian.sh` operations only replace the host binary atomically and TERM guardian/quality children so the existing launcher starts the new process. Preflight and contract tests reject ambiguous mounts and dangerous operations before any live mutation.

**Tech Stack:** POSIX shell, Python 3, pytest, Docker Compose inspection, Go vendor build, Linux ELF and SHA-256 verification.

---

### Task 1: Persist guardian binary as a directory mount

**Files:**
- Modify: `scripts/compose_patch.py:_ensure_mounts`
- Test: `tests/test_compose_patch.py`

- [ ] **Step 1: Write failing mount migration tests**

Add tests that pass a Compose service containing the legacy line
`- /opt/x/guardian/bin/guardian:/guardian/bin/guardian:ro` and assert the patched output contains
`- /opt/x/guardian/bin:/guardian/bin:ro`, contains no `/guardian/bin/guardian` volume target, is
idempotent, and leaves `7891`, `network_mode`, `providers`, `environment`, and the service command
unchanged except for the guardian entrypoint/command contract.

- [ ] **Step 2: Run the focused tests and verify the expected failure**

Run:

```sh
pytest -q tests/test_compose_patch.py
```

Expected: the new directory-mount assertions fail because the current patch still emits a single-file
`/guardian/bin/guardian` mount.

- [ ] **Step 3: Implement directory mount normalization**

Change `_ensure_mounts` to declare the binary mount as
`("/guardian/bin", root / "bin", "ro")` using the exact existing formatting conventions. Before
adding required mounts, remove or replace any volume whose target is `/guardian/bin/guardian`; reject
an existing `/guardian/bin` mount whose source differs from `root / "bin"` or whose mode is not `ro`.
Keep all unrelated volume lines and all non-volume service properties byte-for-byte unchanged.

- [ ] **Step 4: Run the focused tests and the Python suite**

Run:

```sh
pytest -q tests/test_compose_patch.py
pytest -q
```

Expected: both commands exit 0, and the legacy migration, directory idempotence, and Mihomo-field
preservation tests pass.

- [ ] **Step 5: Commit the isolated patch change**

```sh
git add scripts/compose_patch.py tests/test_compose_patch.py
git commit -m "fix: mount guardian binary directory for atomic updates"
```

### Task 2: Add safe install migration gating

**Files:**
- Modify: `scripts/install.sh`
- Test: `tests/test_launcher.py`
- Test: `tests/test_repository_contract.py`

- [ ] **Step 1: Write failing installer contract tests**

Add assertions that `install.sh` accepts `--migrate-bin-mount`, detects both
`/guardian/bin/guardian` and `/guardian/bin` from `docker inspect`, reports
`migration_required=1` for the legacy form, refuses a legacy non-preflight installation unless the
flag is present, and leaves the existing guarded
`docker compose -f "$COMPOSE_PATH" up -d --force-recreate "$DISCOVERED_SERVICE"` path only
inside the explicit migration route. Assert that `docker stop`, `docker restart`, `docker kill`, and
`docker compose down` do not appear as executable commands in the installer.

- [ ] **Step 2: Run the focused tests and verify they fail**

Run:

```sh
pytest -q tests/test_launcher.py tests/test_repository_contract.py
```

Expected: the tests fail because the installer has no migration flag or mount-type preflight.

- [ ] **Step 3: Implement mount-type detection before live writes**

After `docker inspect "$CONTAINER" > "$INSPECT_FILE"`, derive a single `GUARDIAN_BIN_MOUNT_MODE`
and `GUARDIAN_BIN_SOURCE` with Python. Accept exactly one of:

```text
legacy-file:/absolute/path/guardian/bin/guardian
directory:/absolute/path/guardian/bin
```

Reject missing, duplicate, writable, symlinked, or ambiguous mounts. Print the mode in discovery
output. In non-preflight mode, if the mode is `legacy-file` and `--migrate-bin-mount` is absent, exit
before `mkdir`, backup, build, `install`, Compose writes, or process signals with a maintenance-window
message. Keep `--preflight` read-only and report `migration_required=1` for legacy mounts.

- [ ] **Step 4: Implement explicit migration argument handling**

Add `MIGRATE_BIN_MOUNT=0`, parse `--migrate-bin-mount`, and include the flag in `usage()`. Permit the
existing backup/build/config/Compose recreate path only when this flag is set for a legacy mount;
directory-mounted installs may retain the existing explicit reinjection behavior. Ensure the generated
Compose is validated before writing it and keep rollback failure fatal.

- [ ] **Step 5: Run tests and shell syntax validation**

Run:

```sh
pytest -q tests/test_launcher.py tests/test_repository_contract.py
sh -n scripts/install.sh
```

Expected: all focused tests pass and `sh -n` exits 0.

- [ ] **Step 6: Commit the migration gate**

```sh
git add scripts/install.sh tests/test_launcher.py tests/test_repository_contract.py
git commit -m "feat: gate legacy guardian mount migration"
```

### Task 3: Create atomic guardian updater

**Files:**
- Create: `scripts/update-guardian.sh`
- Test: `tests/test_update_guardian.py`

- [ ] **Step 1: Write failing updater contract tests**

Create source-level tests for these exact invariants:

```python
from pathlib import Path

SCRIPT = (Path(__file__).parents[1] / "scripts" / "update-guardian.sh").read_text()

def test_update_script_requires_directory_mount_and_preflight_is_read_only():
    assert 'GUARDIAN_BIN_DESTINATION="/guardian/bin"' in SCRIPT
    assert 'GUARDIAN_BIN_MODE="directory"' in SCRIPT
    assert '--preflight' in SCRIPT
    assert 'migration_required=1' in SCRIPT
    assert 'exit 0' in SCRIPT

def test_update_script_builds_to_temp_verifies_elf_hash_and_renames_atomically():
    assert 'mktemp' in SCRIPT
    assert 'sha256sum' in SCRIPT
    assert 'file "$BUILD_ARTIFACT"' in SCRIPT or 'readelf' in SCRIPT
    assert 'mv "$LIVE_TMP" "$LIVE_BINARY"' in SCRIPT

def test_update_script_only_terms_guardian_children():
    assert 'kill -TERM "$pid"' in SCRIPT
    assert 'guardian_pid' in SCRIPT
    assert 'quality_pid' in SCRIPT
    assert 'kill -TERM "$mihomo_pid"' not in SCRIPT

def test_update_script_preserves_mihomo_pid_and_rolls_back_binary_on_failed_verification():
    assert 'mihomo_pid_before' in SCRIPT
    assert 'mihomo_pid_after' in SCRIPT
    assert 'update_rolled_back' in SCRIPT
    assert 'mv "$ROLLBACK_TMP" "$LIVE_BINARY"' in SCRIPT
    assert 'docker compose' not in SCRIPT
```

The assertions must inspect real script text and require the script to contain `mktemp`, `sha256sum`,
`readelf` or `file`, `mv`, a backup path under `backups`, a Mihomo PID snapshot, a guardian-only TERM,
and a rollback branch. They must reject `docker stop`, `docker restart`, `docker kill`, `docker compose
down`, and any `kill` of the saved Mihomo PID.

- [ ] **Step 2: Run the focused tests and verify the expected failure**

Run:

```sh
pytest -q tests/test_update_guardian.py
```

Expected: collection fails because `scripts/update-guardian.sh` does not exist.

- [ ] **Step 3: Implement read-only discovery and preflight**

Implement options `--preflight`, `--observe`, `--container`, and `--guardian-root`. Discover the
guardian root from the `/guardian/guardian.yaml` mount when not supplied, then inspect the container
mounts and require exactly one read-only directory mount with destination `/guardian/bin`, no legacy
file mount, and a source equal to `guardian_root/bin`. Require the container to be running and the
host source directory, `guardian.yaml`, `controller_secret`, and `start-guardian.sh` to exist. On
`--preflight`, print mount mode and target paths, then exit before build, backup, rename, or signal.

- [ ] **Step 4: Implement isolated build and artifact validation**

Build with:

```sh
docker run --rm --network "container:$CONTAINER" \
  -v "$REPO_DIR:/src" -w /src golang:1.24-alpine \
  sh -c 'go test -mod=vendor ./... && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -trimpath -ldflags="-s -w" -o dist/guardian.update.tmp ./cmd/guardian'
```

Require the artifact to be a regular executable Linux ELF, calculate its SHA-256, and reject an empty
or non-amd64 build before touching the live binary. Do not print configuration, secret, provider JSON,
or URLs.

- [ ] **Step 5: Implement backup, atomic replacement, and guardian-only reload**

Create `guardian_root/backups/update-<UTC>-<pid>`, copy the current binary and write a manifest with
old/new hashes. Copy the verified artifact into a temporary file in `guardian_root/bin`, set mode
0755, fsync it through a small Python one-liner or `sync`, verify its hash, and use `mv` within the same
directory to replace `guardian`. Snapshot the Mihomo PID from `docker top` before replacement. Find
guardian and quality child PIDs from `docker top`/launcher ancestry and send TERM only to those PIDs.

- [ ] **Step 6: Implement bounded verification and binary-only rollback**

Wait up to 60 seconds for a guardian process using the new hash/version while the container remains
running. Require the saved Mihomo PID to still exist and remain the same, and require the container's
proxy process to remain present. On timeout, restore the saved binary with an atomic `mv`, TERM only
guardian/quality children again, and exit non-zero with `update_rolled_back`. Never recreate or restart
the container in this failure path. `--observe` must preserve the existing guardian configuration mode;
it only controls the updater's reporting.

- [ ] **Step 7: Run tests and shell syntax validation**

Run:

```sh
pytest -q tests/test_update_guardian.py
sh -n scripts/update-guardian.sh
```

Expected: all updater contracts pass and the shell script parses successfully.

- [ ] **Step 8: Commit the updater**

```sh
git add scripts/update-guardian.sh tests/test_update_guardian.py
git commit -m "feat: add atomic guardian updater"
```

### Task 4: Document the two update paths and operator safeguards

**Files:**
- Modify: `README.md`
- Modify: `docs/configuration.md`
- Modify: `skills/mihomo-guardian-production/SKILL.md`
- Test: `tests/test_repository_contract.py`

- [ ] **Step 1: Write failing documentation contract assertions**

Require the three operator documents to mention `update-guardian.sh`, `--preflight`,
`--migrate-bin-mount`, `migration_required=1`, directory mount `/guardian/bin:/guardian/bin`, atomic
rename, guardian-only reload, and the fact that first migration needs a maintenance window while daily
updates do not recreate Mihomo.

- [ ] **Step 2: Run the documentation tests and verify failure**

Run:

```sh
pytest -q tests/test_repository_contract.py
```

Expected: the new markers fail because the update flow is not documented.

- [ ] **Step 3: Document commands and failure handling**

Add a short “更新流程” section to each document. Show:

```sh
sudo ./scripts/install.sh --preflight
sudo ./scripts/install.sh --migrate-bin-mount --observe
sudo ./scripts/update-guardian.sh --preflight
sudo ./scripts/update-guardian.sh
```

State that the first command pair is the only path that may recreate the service, requires a planned
maintenance window, and must be observed before auto mode. State that daily updates use atomic host
replacement and only guardian/quality child TERM; the Mihomo PID, port, config, provider, and quality
store must remain unchanged. Include exact rollback behavior and log event names without exposing
secrets.

- [ ] **Step 4: Run documentation tests and content checks**

Run:

```sh
pytest -q tests/test_repository_contract.py
git diff --check
```

Expected: all contract tests pass and `git diff --check` exits 0.

- [ ] **Step 5: Commit documentation**

```sh
git add README.md docs/configuration.md skills/mihomo-guardian-production/SKILL.md tests/test_repository_contract.py
git commit -m "docs: document safe guardian update paths"
```

### Task 5: Full verification and production-safe reinjection handoff

**Files:**
- Verify: all repository files and production read-only state

- [ ] **Step 1: Run complete repository verification**

Run:

```sh
git diff --check
pytest -q
make check CONTAINER=mihomo-cliproxy
make build CONTAINER=mihomo-cliproxy
file dist/guardian
```

Expected: Python tests pass, Go tests/vet pass, the static Linux amd64 build succeeds, and `file`
reports an executable Linux amd64 ELF.

- [ ] **Step 2: Run production preflight only**

Run:

```sh
sudo ./scripts/status.sh --read-only --container mihomo-cliproxy
sudo ./scripts/install.sh --preflight --container mihomo-cliproxy
sudo ./scripts/update-guardian.sh --preflight --container mihomo-cliproxy
```

Expected for the current production state: the status command reports Mihomo and guardian processes;
install preflight reports `migration_required=1` for the old single-file mount; updater preflight
refuses daily update and explains that explicit migration is required. No container, service, config,
binary, or process changes are allowed in this step.

- [ ] **Step 3: Hand off the one-time migration requirement**

Do not run `--migrate-bin-mount` in this session unless a maintenance window is explicitly active.
Report the exact preflight evidence and the one-time command. After the operator runs the migration,
future Gemini/OpenAI binary reinjection must use `update-guardian.sh`, not `install.sh`.
