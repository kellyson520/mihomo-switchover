# Fast Channel Failover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make failover complete within two minutes after a corroborated first failure while preserving low normal probe frequency, safe recovery, and non-empty backup provider behavior.

**Architecture:** Subscribe to mihomo's local `/logs?level=error` WebSocket as a non-blocking hint source, keep normal public probes cached at `probe_interval`, and add a bounded failure-only recheck cadence. Classify transport failures separately from reachable HTTP and upstream 5xx responses, and require the configured critical quorum plus a verified backup node. Keep provider health refresh on its own recovery cadence and make provider-filter validation fail closed before deployment.

**Tech Stack:** Go 1.24, standard `net/http`/`context`, YAML v3, Python deployment helpers, Go/Python tests, Docker static build.

---

### Task 1: Add adaptive decision timings

**Files:** `internal/config/config.go`, `internal/config/config_test.go`, `configs/guardian.example.yaml`, `docs/configuration.md`

- [ ] **Step 1: Write the failing tests.** Load a config containing `failure_recheck_interval: 30s` and `recovery_healthcheck_interval: 2m`; assert both durations. Load one with both omitted; assert defaults of 30 seconds and 2 minutes. Add invalid-duration cases and assert the error names the offending field.
- [ ] **Step 2: Run the focused test and verify RED.**

```bash
go test ./internal/config -run 'TestLoad.*(Decision|Interval)' -count=1
```

Expected: failure because the raw schema rejects the new fields.

- [ ] **Step 3: Implement the minimal schema.** Add `FailureRecheckInterval` and `RecoveryHealthcheckInterval` to `DecisionConfig` and `rawDecision`, normalize with the stated defaults, and use `fieldError` for parse failures. Document that the first is used only after an unhealthy public sample and the second throttles native provider healthchecks during recovery.
- [ ] **Step 4: Run the focused test again and verify GREEN.** Run the command from Step 2; expect all matching tests to pass.
- [ ] **Step 5: Commit.**

```bash
git add internal/config/config.go internal/config/config_test.go configs/guardian.example.yaml docs/configuration.md
git commit -m "feat: configure fast failure and recovery checks"
```

### Task 2: Subscribe to mihomo error logs without coupling the guardian loop

**Files:** `internal/mihomo/client.go`, create `internal/mihomo/logstream.go`, `internal/mihomo/logstream_test.go`, `internal/runtime/service.go`, `internal/runtime/service_test.go`, `cmd/guardian/main.go`

- [ ] **Step 1: Write failing tests.** Add a WebSocket test server that validates the Authorization header and Upgrade request, sends a text frame containing a mihomo `Log` payload, and closes the connection. Assert the client extracts only a classified network-error hint and does not return the raw payload. Add a reconnect test and a service test showing a network log hint invalidates a healthy five-minute probe cache, while provider/filter/config error text does not.
- [ ] **Step 2: Run RED.**

```bash
go test ./internal/mihomo ./internal/runtime -run '(Log|Hint)' -count=1
```

Expected: failure because no log-stream client or service hint method exists.
- [ ] **Step 3: Implement the minimal stream client.** Add a standard-library WebSocket handshake and frame reader/writer sufficient for RFC 6455 text, ping/pong, close, and bounded payloads. Connect to `/logs?level=error` over the already validated loopback controller, send the bearer token, reconnect with capped backoff, and return on context cancellation. Parse Mihomo log envelopes and classify only network-related error messages; never return or log the raw message.
- [ ] **Step 4: Add the service hint boundary.** Add a mutex-protected hint timestamp/classification. `ObserveMihomoError` invalidates the active public-probe cache only for network-related classes and records a rate-limited `mihomo_error_hint` event with category only. The service remains safe if the stream is unavailable.
- [ ] **Step 5: Start the watcher from `executeRun`.** Run it in a goroutine after the startup heartbeat. Stream errors are logged as a throttled diagnostic event and do not return from `executeRun` or stop the guardian loop.
- [ ] **Step 6: Run focused tests and commit.**

```bash
go test ./internal/mihomo ./internal/runtime ./cmd/guardian -count=1
git add internal/mihomo internal/runtime/service_test.go cmd/guardian/main.go
git commit -m "feat: use mihomo errors as fast probe hints"
```

### Task 3: Implement bounded fast failure confirmation

**Files:** `internal/runtime/service.go`, `internal/runtime/service_test.go`, `internal/probe/probe_test.go`, `README.md`

- [ ] **Step 1: Write the failing tests.** Use two critical probes and a counting fake. With a one-hour normal interval and 30-second failure interval, return two network failures at t=0, call a second cycle at t=15s and assert no new checks/streak remains 1, then call at t=30s and assert a new sample/streak 2, and t=60s assert the third sample can switch to a verified backup. Add a blocking fake proving the two critical probes run concurrently. Add classification assertions that 401/403/429 are reachable, 503 is `UpstreamHTTPError`, and only `NetworkError` is route-failure evidence.
- [ ] **Step 2: Run the focused tests and verify RED.**

```bash
go test ./internal/runtime ./internal/probe -run 'Test(Service|Classify).*' -count=1
```

Expected: failure because the service has no failure-only cadence or concurrent probe collection.

- [ ] **Step 3: Implement the minimal behavior.** Keep healthy results cached for `ProbeInterval`; when the cached result is unhealthy, use `FailureRecheckInterval`, so an unhealthy result is counted only once per fresh interval. Run enabled critical probes concurrently, count `ReachableHTTP` as pass, retain transport-failure evidence, and do not treat upstream 5xx as node failure. Use an injectable service clock for timestamps and decision time. When provider resolution returns no node, derive the probe key from the current group node, and require main provider verification together with the public result before calling main healthy.
- [ ] **Step 4: Run focused tests and verify GREEN.**

```bash
go test ./internal/runtime ./internal/probe -count=1
```

Expected: all focused and existing package tests pass.
- [ ] **Step 5: Commit.**

```bash
git add internal/runtime/service.go internal/runtime/service_test.go internal/probe/probe_test.go README.md
git commit -m "fix: confirm route failures within bounded window"
```

### Task 4: Isolate provider recovery healthchecks

**Files:** `internal/runtime/service.go`, `internal/runtime/service_test.go`, `docs/configuration.md`

- [ ] **Step 1: Write the failing test.** Configure a two-minute recovery healthcheck interval and an unverified main provider. Call the provider path twice within two minutes and assert one native healthcheck; advance beyond two minutes and assert one more. Make the fake healthcheck fail and assert the service remains fail-closed without returning a guardian-cycle error.
- [ ] **Step 2: Run the test and verify RED.**

```bash
go test ./internal/runtime -run 'TestService.*HealthCheck' -count=1
```

Expected: failure because throttling currently reuses the public probe interval.
- [ ] **Step 3: Implement the minimal change.** Use `RecoveryHealthcheckInterval` only for `providerHealthChecks`; keep failures advisory, reset the map on config reload, and never select a node or channel from the healthcheck method.
- [ ] **Step 4: Run all runtime tests.** `go test ./internal/runtime -count=1`; expect exit 0.
- [ ] **Step 5: Commit.**

```bash
git add internal/runtime/service.go internal/runtime/service_test.go docs/configuration.md
git commit -m "fix: isolate provider recovery healthcheck cadence"
```

### Task 5: Guard backup provider filters before deployment

**Files:** create `scripts/provider_filter_guard.py` and `tests/test_provider_filter_guard.py`; modify `scripts/install.sh` and `docs/configuration.md`

- [ ] **Step 1: Write failing Python tests.** Test a configured filter matching cached names containing `美国`, `🇺🇸`, `US`, and `United States`; assert a positive count. Test a zero-match filter and an unavailable cache; assert `ProviderFilterError` reports only provider name and counts. Test an unfiltered provider is accepted.
- [ ] **Step 2: Run RED.**

```bash
pytest -q tests/test_provider_filter_guard.py
```

Expected: module-not-found failure.
- [ ] **Step 3: Implement the guard.** Parse only top-level `proxy-providers` and cached provider `proxies[].name`, resolve relative paths from the mihomo config directory, evaluate filters with `re.search`, reject invalid regex or zero matches, and never print URLs, tokens, or proxy definitions. Leave filters unchanged.
- [ ] **Step 4: Call the guard in `install.sh` before compose patching, backups, binary build, or container operations.** A failed check exits non-zero without mutating production.
- [ ] **Step 5: Run verification.**

```bash
pytest -q tests/test_provider_filter_guard.py tests/test_mihomo_config_patch.py
sh -n scripts/install.sh scripts/status.sh deploy/start-guardian.sh
```

Expected: exit 0.
- [ ] **Step 6: Commit.**

```bash
git add scripts/provider_filter_guard.py tests/test_provider_filter_guard.py scripts/install.sh docs/configuration.md
git commit -m "fix: reject empty backup provider filters before deploy"
```

### Task 6: Apply and verify production safely

**Files:** production `/opt/mihomo-cliproxy/config/config.yaml` and mounted `/opt/mihomo-cliproxy/guardian/bin/guardian`

- [ ] **Step 1: Read-only check the backup cache and record counts only.** Confirm the replacement expression `(美国|🇺🇸|\\bUS\\b|United States)` matches at least one cached node.
- [ ] **Step 2: Make a timestamped recoverable copy, then change only the backup provider filter.** Validate YAML and positive match count. Do not stop, restart, recreate, or signal mihomo. Do not reload the whole mihomo config because it is a read-only bind mount; preserve the change for the normal provider/config reload path.
- [ ] **Step 3: Run full verification in Docker.**

```bash
go test -mod=vendor ./...
go test -mod=vendor -race ./...
go vet -mod=vendor ./...
pytest -q
docker run --rm --network container:mihomo-cliproxy -v "$PWD:/src" -w /src golang:1.24-alpine sh -c 'go test -mod=vendor ./... && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -trimpath -o dist/guardian ./cmd/guardian'
```

Expected: all commands exit 0 and `dist/guardian` is an executable Linux amd64 binary.
- [ ] **Step 4: Install only the guardian binary.** Copy it to the mounted `guardian/bin/guardian` with mode 0755; do not run Docker lifecycle commands or send a signal to mihomo.
- [ ] **Step 5: Verify production invariants and logs.** Run `sudo ./scripts/status.sh --read-only` and `docker exec mihomo-cliproxy ps -o pid,ppid,comm,args`; confirm container running, mihomo PID unchanged, guardian present, backup node count non-zero, and at least one backup node has `alive:true` plus history. Inspect fresh `probe`, `provider_healthcheck_*`, `provider_unverified`, and `channel_switched` events, and report measured timing only if a real switch occurs.
- [ ] **Step 6: Commit and push repository changes.**

```bash
git add .
git commit -m "fix: accelerate safe channel failover"
git push origin feature/ipquality-stability
```
