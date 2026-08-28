# Mihomo Guardian Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and safely inject a persistent, same-container mihomo guardian that routes all external probes through mihomo, keeps provider nodes sticky, switches channels only on corroborated evidence, and never stops mihomo when guardian fails.

**Architecture:** A statically linked Go binary and launcher script are mounted into the existing `mihomo-cliproxy` container. The launcher remains PID 1, starts mihomo as the production proxy, and independently restarts guardian when guardian crashes or loses the mihomo API; guardian never stops mihomo. Guardian talks to the local controller API directly, sends every external HTTP probe through `127.0.0.1:7890`, persists state/logs under `/opt/mihomo-cliproxy/guardian`, and exits after a lost heartbeat so the launcher can restart only guardian. The installer patches the existing compose file transactionally and keeps timestamped rollback copies.

**Tech Stack:** Go standard library, `gopkg.in/yaml.v3`, Docker BuildKit builder (`golang:1.24-alpine`), POSIX-compatible shell, Python 3 host-side installer tests, Go `testing` package.

---

## Repository and file map

All paths below are relative to `/上传/mihomo-guardian` unless an absolute deployment path is shown.

- Create `go.mod`, `go.sum`: Go module and locked YAML dependency.
- Create `cmd/guardian/main.go`: guardian CLI entrypoint.
- Create `internal/config/config.go`: YAML schema, defaults, validation, and file reload.
- Create `internal/mihomo/client.go`: direct local mihomo REST API client and proxy/group models.
- Create `internal/probe/probe.go`: HTTP-over-mihomo probe client and status classification.
- Create `internal/decision/engine.go`: pure failover, recovery, cooldown, quorum, and sticky-node decision logic.
- Create `internal/state/store.go`: atomic JSON state and provider lock persistence.
- Create `internal/purity/advisor.go`: optional proxied IP/ASN/risk advisor; never a single-sample switch trigger.
- Create `internal/logging/logger.go`: JSONL logging, redaction, and size-based rotation.
- Create `internal/runtime/runtime.go`: heartbeat loop, reload loop, probe cycle, and decision application.
- Create `deploy/start-guardian.sh`: same-container launcher that keeps mihomo alive while restarting guardian independently.
- Create `configs/guardian.example.yaml`: one editable behavior configuration with safe defaults.
- Create `deploy/Dockerfile.builder`: reproducible static binary/test builder; no runtime image replacement.
- Create `scripts/install.sh`: preflight, build, backup, idempotent compose/config injection, migration, and smoke test.
- Create `scripts/rollback.sh`: restore the most recent verified backup and restore old systemd switcher.
- Create `scripts/status.sh`: read-only container, API, channel, node, process, and log status.
- Create `tests/test_compose_patch.py`: host-side idempotent compose patch and rollback tests.
- Create `tests/fixtures/mihomo-compose.yml`: minimal copy of the current compose shape for patch tests.
- Create `README.md`: install, one-file config, operation, safety behavior, and rollback instructions.
- Create `.gitignore`: build artifacts, local data, and test output exclusions.

The existing `/opt/mihomo-cliproxy/channel_switch.py` and `mihomo-channel-switch.service` are not edited in the repository. The installer backs them up, disables the old service, and leaves the old files recoverable.

### Task 1: Establish the Go module and safe configuration contract

**Files:**
- Create: `go.mod`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`
- Create: `configs/guardian.example.yaml`
- Create: `.gitignore`

- [ ] **Step 1: Write failing configuration tests first.**

Add tests that prove the configuration has a single source of behavior, applies documented defaults, rejects direct external probing, rejects missing groups, and reloads only a complete valid document:

```go
func TestLoadRejectsExternalProbeWithoutMihomoProxy(t *testing.T) {
    _, err := LoadBytes([]byte(`
mihomo:
  api: http://127.0.0.1:9090
  proxy: ""
  secret_file: /tmp/secret
groups:
  channel: CHANNEL
  main: MAIN
  backup: BACKUP-USA
probes:
  - id: openai
    url: https://api.openai.com/v1/models
`))
    if err == nil || !strings.Contains(err.Error(), "mihomo proxy") {
        t.Fatalf("expected proxy validation error, got %v", err)
    }
}

func TestLoadAppliesConservativeDefaults(t *testing.T) {
    cfg, err := LoadBytes(validMinimalConfig(t))
    if err != nil { t.Fatal(err) }
    if cfg.Decision.Interval != 15*time.Second { t.Fatalf("interval=%s", cfg.Decision.Interval) }
    if cfg.Decision.FailuresBeforeSwitch != 3 { t.Fatalf("fail threshold=%d", cfg.Decision.FailuresBeforeSwitch) }
    if cfg.Decision.RecoveriesBeforeSwitch != 2 { t.Fatalf("recovery threshold=%d", cfg.Decision.RecoveriesBeforeSwitch) }
    if cfg.Decision.LinkLossGrace != 15*time.Second { t.Fatalf("link grace=%s", cfg.Decision.LinkLossGrace) }
    if cfg.Decision.MinHold != 120*time.Second { t.Fatalf("min hold=%s", cfg.Decision.MinHold) }
}

func TestReloadKeepsPreviousConfigWhenNewFileIsInvalid(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "guardian.yaml")
    if err := os.WriteFile(path, validMinimalConfig(t), 0600); err != nil { t.Fatal(err) }
    loader := NewLoader(path)
    old, err := loader.Load()
    if err != nil { t.Fatal(err) }
    if err := os.WriteFile(path, []byte("decision: [broken"), 0600); err != nil { t.Fatal(err) }
    current, changed, err := loader.ReloadIfChanged(old)
    if err == nil || changed || current.Decision.FailuresBeforeSwitch != old.Decision.FailuresBeforeSwitch {
        t.Fatalf("invalid reload was accepted: changed=%v err=%v", changed, err)
    }
}
```

- [ ] **Step 2: Run the new tests in the builder and confirm the expected RED failure.**

Run:

```bash
docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine sh -c 'go test ./internal/config -run "TestLoad" -v'
```

Expected: FAIL because the package and loader functions do not exist yet; there must be no compile-success false positive.

- [ ] **Step 3: Implement the minimal config schema and loader.**

Use `yaml.v3` with explicit structs. Durations are parsed from strings. `LoadBytes` must validate `mihomo.proxy` is an `http://127.0.0.1:7890` or `socks5://127.0.0.1:7890` URL, controller API is loopback, group names are non-empty, probe IDs/URLs are unique, and critical quorum is within the number of enabled probes. Unknown fields are rejected with `yaml.Decoder.KnownFields(true)`.

The default example must set `mode: auto`, but the installer first supports `--observe` and only enables auto after smoke checks. No API key field is present anywhere in the schema.

- [ ] **Step 4: Run the focused tests and then all package tests.**

Run the same Docker test command and expect all configuration tests to pass with zero warnings.

- [ ] **Step 5: Commit the module/config contract.**

```bash
git add go.mod go.sum internal/config/config.go internal/config/config_test.go configs/guardian.example.yaml .gitignore
git commit -m "feat: add guardian configuration contract"
```

### Task 2: Implement mihomo API access and enforce proxied external requests

**Files:**
- Create: `internal/mihomo/client.go`
- Create: `internal/probe/probe.go`
- Test: `internal/mihomo/client_test.go`
- Test: `internal/probe/probe_test.go`

- [ ] **Step 1: Write failing API and proxy-enforcement tests.**

Use `httptest.Server` to exercise JSON API responses and a local HTTP proxy that records requests. Tests must prove controller requests use the direct local API client while external requests arrive at the proxy, and that an empty proxy URL is rejected before any external request is made:

```go
func TestExternalClientSendsRequestThroughMihomoProxy(t *testing.T) {
    target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r) { w.WriteHeader(http.StatusUnauthorized) }))
    defer target.Close()
    var proxied int32
    proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r) {
        atomic.AddInt32(&proxied, 1)
        w.WriteHeader(http.StatusUnauthorized)
    }))
    defer proxy.Close()

    client, err := NewExternalClient(proxy.URL, 2*time.Second)
    if err != nil { t.Fatal(err) }
    result := client.Check(context.Background(), ProbeSpec{ID: "openai", URL: target.URL})
    if result.Class != ReachableHTTP { t.Fatalf("class=%s", result.Class) }
    if atomic.LoadInt32(&proxied) != 1 { t.Fatalf("external request bypassed proxy") }
}

func TestExternalClientRejectsDirectMode(t *testing.T) {
    if _, err := NewExternalClient("", time.Second); err == nil {
        t.Fatal("expected direct external access to be rejected")
    }
}
```

- [ ] **Step 2: Run the focused tests and confirm RED.**

```bash
docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine sh -c 'go test ./internal/mihomo ./internal/probe -v'
```

Expected: FAIL because the client types and methods are not implemented.

- [ ] **Step 3: Implement the direct mihomo client.**

Implement `Client` with `GetProxy`, `ListProxies`, `Heartbeat`, `Delay`, `SetProxy`, and `RefreshProvider`. It must use a transport with no proxy and only accept loopback API URLs. Encode every group/node name as a path segment. Treat HTTP 401/403 as an authentication/configuration error, not a node failure.

- [ ] **Step 4: Implement the external probe client through mihomo.**

Create a dedicated `http.Transport` whose only proxy is the configured mihomo proxy URL. Do not fall back to `http.DefaultTransport`. Classify 200–499 as `reachable_http`, 5xx as `upstream_http_error`, and DNS/TCP/TLS/timeout failures as their specific class. Record status and duration but redact query credentials.

- [ ] **Step 5: Add provider-backed node verification behavior.**

For provider-backed groups, read `/providers/proxies/{provider}` and match group candidates by
node name. Accept a candidate only when mihomo reports `alive: true` and includes health history;
use the latest recorded delay only as a stable selection score. Do not temporarily select every
candidate in production traffic. Some mihomo Alpha versions return 404 for
`/proxies/{node}/delay` even though provider metadata contains per-node health. Keep the direct
`/proxies/{name}/delay` path for groups without a provider mapping, where the API supports it.

- [ ] **Step 6: Run focused tests, then commit.**

```bash
docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine sh -c 'go test ./internal/mihomo ./internal/probe -v'
git add internal/mihomo internal/probe
git commit -m "feat: probe external APIs through mihomo"
```

### Task 3: Build persistence and the pure switching decision engine

**Files:**
- Create: `internal/state/store.go`
- Create: `internal/decision/engine.go`
- Test: `internal/state/store_test.go`
- Test: `internal/decision/engine_test.go`

- [ ] **Step 1: Write failing state and decision tests.**

Cover atomic restart persistence, corrupted-state recovery, sticky node ordering, consecutive-failure switching, recovery hysteresis, cooldown, quorum, and “no verified candidate means no switch”:

```go
func TestDecisionSwitchesOnlyAfterThreeCorroboratedFailures(t *testing.T) {
    e := NewEngine(DecisionConfig{FailuresBeforeSwitch: 3, RecoveriesBeforeSwitch: 2, MinHold: 0})
    state := State{CurrentChannel: "MAIN"}
    input := Input{CurrentHealthy: false, BackupHealthy: true, BackupNode: "backup-1"}
    for i := 1; i <= 2; i++ {
        action := e.Evaluate(state, input)
        if action.Kind != Noop { t.Fatalf("failure %d action=%s", i, action.Kind) }
        state = action.State
    }
    action := e.Evaluate(state, input)
    if action.Kind != SwitchChannel || action.Channel != "BACKUP-USA" { t.Fatalf("action=%+v", action) }
}

func TestStickyNodeWinsOverFasterAlternative(t *testing.T) {
    got := ChooseNode("main-old", []Candidate{{Name: "main-fast", Healthy: true, Score: 99}, {Name: "main-old", Healthy: true, Score: 10}})
    if got != "main-old" { t.Fatalf("node=%s", got) }
}

func TestNoVerifiedBackupNeverSwitches(t *testing.T) {
    action := NewEngine(DecisionConfig{FailuresBeforeSwitch: 1}).Evaluate(
        State{CurrentChannel: "MAIN"}, Input{CurrentHealthy: false, BackupHealthy: false})
    if action.Kind != Noop { t.Fatalf("action=%s", action.Kind) }
}

func TestStateStoreAtomicallyRecoversCorruptState(t *testing.T) {
    path := filepath.Join(t.TempDir(), "state.json")
    if err := os.WriteFile(path, []byte("{"), 0600); err != nil { t.Fatal(err) }
    store := NewStore(path)
    state, err := store.Load()
    if err != nil { t.Fatal(err) }
    if state.CurrentChannel != "MAIN" { t.Fatalf("state=%+v", state) }
    matches, _ := filepath.Glob(path + ".corrupt.*")
    if len(matches) != 1 { t.Fatalf("corrupt backup count=%d", len(matches)) }
}
```

- [ ] **Step 2: Run the tests and confirm RED.**

```bash
docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine sh -c 'go test ./internal/state ./internal/decision -v'
```

Expected: FAIL because the state and decision packages do not exist.

- [ ] **Step 3: Implement atomic state storage.**

Write JSON to `<path>.tmp`, `fsync`, close, then `os.Rename`. On malformed JSON, rename the original to `.corrupt.<UTC timestamp>` and return defaults. Store current channel, failure/recovery streaks, hold-until, forced mode, and provider lock records.

- [ ] **Step 4: Implement deterministic decision logic.**

The engine is pure and takes state plus probe evidence. It never switches on a single probe, never switches without a verified candidate, honors manual force mode and minimum hold, resets counters only on valid evidence, and returns an action with a reason and evidence IDs. `ChooseNode` always tries the persisted node first, then provider order, then score, with stable tie-breaking.

- [ ] **Step 5: Run focused tests, then commit.**

```bash
docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine sh -c 'go test ./internal/state ./internal/decision -v'
git add internal/state internal/decision
git commit -m "feat: add sticky failover decision engine"
```

### Task 4: Add runtime probes, purity advisory, structured logs, and hot reload

**Files:**
- Create: `internal/purity/advisor.go`
- Create: `internal/logging/logger.go`
- Create: `internal/runtime/runtime.go`
- Test: `internal/purity/advisor_test.go`
- Test: `internal/logging/logger_test.go`
- Test: `internal/runtime/runtime_test.go`
- Modify: `internal/config/config.go`

- [ ] **Step 1: Write failing tests for safety behavior.**

Test that purity is advisory, secrets are redacted, invalid reload leaves the active configuration unchanged, and a lost heartbeat stops all decision work:

```go
func TestPurityWarningCannotCreateSwitchAction(t *testing.T) {
    action := NewEngine(DecisionConfig{FailuresBeforeSwitch: 1}).Evaluate(
        State{CurrentChannel: "MAIN"}, Input{CurrentHealthy: true, Purity: PurityResult{Warning: "datacenter"}})
    if action.Kind != Noop { t.Fatalf("purity changed routing: %s", action.Kind) }
}

func TestLoggerRedactsSecrets(t *testing.T) {
    var b bytes.Buffer
    logger := NewLogger(&b, LoggerConfig{MaxBytes: 1 << 20})
    logger.Event("probe", map[string]any{"url": "https://x.test?a=token-secret&key=api-secret", "secret": "controller-secret"})
    out := b.String()
    for _, forbidden := range []string{"token-secret", "api-secret", "controller-secret"} {
        if strings.Contains(out, forbidden) { t.Fatalf("secret leaked: %s", forbidden) }
    }
}

func TestRuntimeExitsAfterMihomoLinkGrace(t *testing.T) {
    fake := &FakeMihomo{HeartbeatErrors: []error{nil, errors.New("down"), errors.New("down")}}
    rt := NewRuntime(fake, RuntimeConfig{LinkLossGrace: 20 * time.Millisecond, Tick: 5 * time.Millisecond})
    err := rt.Run(context.Background())
    if !errors.Is(err, ErrMihomoLinkLost) { t.Fatalf("err=%v", err) }
    if fake.DecisionCalls != 0 { t.Fatalf("decision ran after lost link: %d", fake.DecisionCalls) }
    if !fake.Terminated { t.Fatal("mihomo was not terminated") }
}
```

- [ ] **Step 2: Run focused tests and confirm RED.**

```bash
docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine sh -c 'go test ./internal/purity ./internal/logging ./internal/runtime -v'
```

Expected: FAIL because these packages and runtime interfaces are not implemented.

- [ ] **Step 3: Implement purity advisory and logger.**

Use the already-proxied external client for IP metadata. Treat service errors as `unknown`, not bad. Logger emits one JSON object per line with timestamp, event, channel, node, status, duration, and reason; redact values for keys matching `secret`, `token`, `key`, `password`, and strip sensitive URL query values. Rotate before exceeding configured bytes and retain the configured count.

- [ ] **Step 4: Implement runtime cycle and hot reload.**

At each cycle: heartbeat direct mihomo API first; if the heartbeat fails, start a monotonic grace timer and do no probes or decisions. Once the grace expires, call the supervisor termination callback and return `ErrMihomoLinkLost`. If heartbeat succeeds, reload the config by mtime, refresh provider metadata only when configured, probe current/candidates through mihomo, evaluate the pure engine, apply a verified action, persist state, and emit a complete evidence log.

- [ ] **Step 5: Run focused tests, then commit.**

```bash
docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine sh -c 'go test ./internal/purity ./internal/logging ./internal/runtime -v'
git add internal/purity internal/logging internal/runtime internal/config/config.go
git commit -m "feat: add proxied probes and safe runtime loop"
```

### Task 5: Implement the independent guardian launcher and CLI

**Files:**
- Create: `deploy/start-guardian.sh`
- Create: `cmd/guardian/main.go`
- Test: `cmd/guardian/main_test.go`
- Test: `tests/test_launcher.py`

- [ ] **Step 1: Write failing launcher and CLI tests.**

Test that run arguments keep mihomo defaults and the launcher restarts guardian without signaling mihomo:

```go
func TestParseRunCommandKeepsMihomoDefaults(t *testing.T) {
    args, err := parseRunArgs([]string{"run", "--config", "/guardian/guardian.yaml"})
    if err != nil { t.Fatal(err) }
    if args.MihomoPath != "/mihomo" || args.MihomoConfigDir != "/root/.config/mihomo" {
        t.Fatalf("args=%+v", args)
    }
}
```

```python
def test_launcher_restarts_guardian_without_killing_mihomo(tmp_path):
    script = Path("deploy/start-guardian.sh").read_text()
    assert "kill \"$mihomo_pid\"" not in script
    assert "guardian_pid" in script
    assert "wait \"$mihomo_pid\"" in script
```

- [ ] **Step 2: Run the tests and confirm RED.**

```bash
docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine sh -c 'go test ./cmd/guardian -v'
pytest -q tests/test_launcher.py
```

Expected: FAIL because the launcher test and CLI implementation do not exist.

- [ ] **Step 3: Implement the independent launcher.**

`deploy/start-guardian.sh` starts `/mihomo -d /root/.config/mihomo` in the background, records its PID, then loops `/guardian/bin/guardian run ...` in a separate child loop. Guardian failures are logged and restarted after one second. The guardian loop never signals or waits on mihomo as a prerequisite for restarting guardian. The launcher waits on mihomo; when mihomo exits, it terminates the guardian loop and exits with mihomo's status. SIGTERM/SIGINT are the only paths that intentionally terminate both children.

- [ ] **Step 4: Implement the guardian CLI without owning mihomo.**

`run` loads the single config, opens the persistent store/log, creates direct local API and mihomo-proxied external clients, and runs the guardian loop. It never starts, stops, or signals `/mihomo`; startup/API loss makes only the guardian process return non-zero so the launcher can restart it. Support:

```text
guardian run --config /guardian/guardian.yaml --data /guardian/data --logs /guardian/logs --secret-file /guardian/controller_secret
guardian status --config ...
guardian switch main|backup --config ...
guardian auto --config ...
guardian probe --config ...
guardian reload --config ...
```

Manual commands use the direct mihomo API and write an audited event; they never bypass the configured proxy for external checks. Forced switching has an expiry and is persisted.

- [ ] **Step 5: Run lifecycle and launcher tests and commit.**

```bash
docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine sh -c 'go test ./cmd/guardian -v'
pytest -q tests/test_launcher.py
git add deploy/start-guardian.sh cmd/guardian tests/test_launcher.py
git commit -m "feat: restart guardian without stopping mihomo"
```

### Task 6: Add the one-file configuration and local build pipeline

**Files:**
- Modify: `configs/guardian.example.yaml`
- Create: `deploy/Dockerfile.builder`
- Create: `Makefile`
- Create: `README.md`

- [ ] **Step 1: Write the complete example configuration.**

It must contain the existing group mapping and proxied endpoints, with safe defaults:

```yaml
mihomo:
  api: http://127.0.0.1:9090
  proxy: http://127.0.0.1:7890
  secret_file: /guardian/controller_secret
groups:
  channel: CHANNEL
  main: MAIN
  backup: BACKUP-USA
decision:
  mode: auto
  interval: 15s
  failures_before_switch: 3
  recoveries_before_switch: 2
  min_hold: 120s
  link_loss_grace: 15s
  critical_quorum: 2
probes:
  - id: openai
    url: https://api.openai.com/v1/models
    critical: true
  - id: gemini
    url: https://generativelanguage.googleapis.com/v1beta/models
    critical: true
  - id: anthropic
    url: https://api.anthropic.com/v1/models
    critical: false
  - id: openrouter
    url: https://openrouter.ai/api/v1/models
    critical: false
purity:
  enabled: true
  automatic_switch: false
  urls:
    - https://api.ipify.org
    - https://ipinfo.io/json
logging:
  max_bytes: 10485760
  retain: 7
reload:
  check_interval: 2s
```

- [ ] **Step 2: Add the Docker builder and Make targets.**

The builder runs tests before producing a `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` binary. `make test` runs `go test ./...`; `make build` writes `dist/guardian`; `make check` runs tests plus `go vet ./...`.

- [ ] **Step 3: Document the no-direct-network rule and operator commands.**

README must state that `9090` is local control only, all external probe traffic exits through `7890`, no API keys are used, `MAIN/BACKUP-USA` are converted to sticky `select` groups, and the controller exits with mihomo on API-link loss.

- [ ] **Step 4: Run builder tests and commit.**

```bash
docker build --target test -f deploy/Dockerfile.builder .
docker build --target binary -f deploy/Dockerfile.builder .
git add configs/guardian.example.yaml deploy/Dockerfile.builder Makefile README.md
git commit -m "build: add guardian builder and operator docs"
```

### Task 7: Implement transactional injection, migration, status, and rollback

**Files:**
- Create: `scripts/install.sh`
- Create: `scripts/rollback.sh`
- Create: `scripts/status.sh`
- Create: `tests/test_compose_patch.py`
- Create: `tests/fixtures/mihomo-compose.yml`
- Create: `scripts/compose_patch.py`
- Modify: `README.md`

- [ ] **Step 1: Write failing installer patch tests.**

Test that the compose patch is idempotent, preserves ports/networks/providers, adds only persistent mounts, sets the launcher as PID 1, and refuses a compose file whose target service is not uniquely found:

```python
def test_patch_is_idempotent_and_preserves_existing_service(tmp_path):
    source = (Path(__file__).parent / "fixtures" / "mihomo-compose.yml").read_text()
    once = patch_compose(source, "/opt/mihomo-cliproxy/guardian")
    twice = patch_compose(once, "/opt/mihomo-cliproxy/guardian")
    assert twice == once
    assert "127.0.0.1:7891:7890" in twice
    assert "1panel-network" in twice
    assert "/opt/mihomo-cliproxy/providers:/root/.config/mihomo/providers" in twice
    assert "entrypoint: [\"/bin/sh\", \"/guardian/start-guardian.sh\"]" in twice
    assert "/opt/mihomo-cliproxy/guardian/data:/guardian/data" in twice

def test_patch_refuses_missing_target_service():
    with pytest.raises(ValueError, match="mihomo-cliproxy"):
        patch_compose("services:\n  other:\n    image: x\n", "/opt/x")
```

- [ ] **Step 2: Run the Python tests and confirm RED.**

```bash
pytest -q tests/test_compose_patch.py
```

Expected: FAIL because `scripts/compose_patch.py` and its fixture do not exist.

- [ ] **Step 3: Implement an idempotent compose patcher.**

Use Python standard library only. Parse the known compose structure by indentation and exact service name; do not regex-replace arbitrary YAML. Insert or replace only the launcher `entrypoint`, six persistent bind mounts (launcher, binary, config, data, logs, secret), and its empty command. Do not alter existing ports, networks, provider mount, image, or restart policy. Write to a temp file and atomically replace after validating the required strings.

- [ ] **Step 4: Implement install preflight and transactional backup.**

`install.sh` must:

1. Require root, Docker, `docker compose`, the target container, target compose, mihomo config, and secret file.
2. Build/test the static binary before stopping anything.
3. Create `/opt/mihomo-cliproxy/guardian/{bin,data,logs,run}` and copy only the binary/config/secret mount metadata there.
4. Back up compose, mihomo config, old switcher, systemd unit, and current state under `/opt/mihomo-cliproxy/guardian/backups/<UTC timestamp>`.
5. Stop and disable `mihomo-channel-switch.service` before installing the new writer.
6. Patch compose and run `docker compose config` before recreating the container.
7. Recreate only `mihomo-cliproxy` with `docker compose up -d --force-recreate mihomo-cliproxy`.
8. Wait for container running, guardian heartbeat, and `PROXY → CHANNEL` status; fail closed if any check times out.
9. Run a read-only probe smoke test; only then leave `decision.mode: auto` enabled. On any failure, call rollback and return non-zero.

The script never deletes provider files, state, logs, backups, or old systemd files. It refuses to operate if the live compose has unexpected target structure.

- [ ] **Step 5: Implement rollback and read-only status.**

`rollback.sh` selects the newest complete backup only after checking its manifest, restores files atomically, brings the container back with the original compose, and re-enables the old systemd service. `status.sh` prints container status, PID 1 command, direct API health, current `CHANNEL`, current `MAIN`/`BACKUP-USA` nodes, state age, and the last 20 redacted log events; it must not mutate anything.

- [ ] **Step 6: Run installer tests and commit.**

```bash
pytest -q tests/test_compose_patch.py
git add scripts tests README.md
git commit -m "feat: add persistent mihomo injection and rollback"
```

### Task 8: Build, verify, stage deploy, and activate only with evidence

**Files:**
- Modify: all implementation files only if verification finds a defect.

- [ ] **Step 1: Run the complete local test suite in a clean builder.**

```bash
docker build --target test -f deploy/Dockerfile.builder .
docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine sh -c 'go test ./... && go vet ./...'
pytest -q
```

Expected: exit 0, all Go and Python tests pass, no vet errors.

- [ ] **Step 2: Build the exact binary that will be mounted.**

```bash
docker build --target binary -f deploy/Dockerfile.builder .
docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine sh -c 'test -x dist/guardian && file dist/guardian'
```

Verify the binary is Linux amd64 and statically linked before copying it to the host runtime directory.

- [ ] **Step 3: Run installer preflight without mutating the live stack.**

```bash
scripts/install.sh --preflight --compose /opt/mihomo-cliproxy/docker-compose.yml
scripts/status.sh --read-only
```

Expected: target container, ports, current groups, secret file, provider mounts, Docker restart policy, and old service are reported; no compose/config/service change occurs.

- [ ] **Step 4: Install with backups and capture fresh evidence.**

```bash
scripts/install.sh --compose /opt/mihomo-cliproxy/docker-compose.yml
scripts/status.sh --read-only
docker restart mihomo-cliproxy
scripts/status.sh --read-only
```

Verify that after restart the same container has the launcher as PID 1, mihomo is its child, guardian is an independent child, state is loaded from the host mount, current channel/node are unchanged unless evidence required otherwise, and external probe logs show the mihomo proxy path.

- [ ] **Step 5: Exercise fail-closed behavior in a disposable test mode before live failure simulation.**

Use the runtime fake API integration test to force heartbeat loss. Confirm guardian exits after the grace period without sending termination to mihomo and without making a decision after heartbeat loss; do not deliberately break the live channel while the号池 is active.

- [ ] **Step 6: Final verification and commit.**

```bash
git diff --check
git status --short
git log --oneline -8
docker inspect mihomo-cliproxy --format '{{.Path}} {{json .Args}} {{json .Mounts}}'
scripts/status.sh --read-only
```

Only after all outputs are clean, commit any final docs/test adjustments and report exact test counts, container state, current channel/node, and backup path. If any live verification fails, stop and use `scripts/rollback.sh`; do not claim completion.

## Plan self-review

- Spec coverage: channel switching, vendor probes through mihomo, sticky provider nodes, advisory purity, logs, hot reload, persistent state, same-container lifecycle, restart survival, rollback, and conservative no-candidate behavior are covered by Tasks 1–8.
- Dependency direction: Tasks 4, 5, and 8 explicitly test startup/API loss, guardian-only restart, mihomo process continuity, and no decision after link loss.
- No direct external access: Task 2 tests proxy routing and Task 6 documents the rule; config validation rejects an empty/direct external proxy.
- No placeholders: all tasks name exact files, commands, expected outcomes, and concrete interfaces/tests; no `TODO`, `TBD`, or unspecified “appropriate handling” steps are used.
- Type consistency: `Config`, `DecisionConfig`, `State`, `Input`, `Action`, `Client`, `Runtime`, and `Supervisor` are introduced in the tasks before use and share the same names.
