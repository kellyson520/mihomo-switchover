# Configurable IP Quality and Stability Implementation Plan

For agentic workers: REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

Goal: Add a persistent, isolated quality daemon that scans user-defined mihomo groups in configured order, evaluates every node in all-scope targets, combines IPQuality-style evidence with hourly mihomo delay history, and lets the realtime guardian use only validated recommendations without touching CHANNEL during scanning.

Architecture: Keep guardian run as the only process allowed to modify production groups. Add a separately supervised guardian quality-daemon that selects nodes only in generated GUARDIAN-QUALITY- groups, sends every public request through a per-target loopback mihomo listener, and writes atomic reports and recommendations under the persistent data mount. A node identity is target/provider/node/IP. The first valid score for an identity is immutable; later scores are saved as latest and best; a 20-point drop from that baseline is required before stickiness can be released.

Tech Stack: Go 1.24, standard library, vendored gopkg.in/yaml.v3, Python 3 standard library for narrow config transformations, POSIX sh, pytest, and Docker-based Go verification.

---

## Task 1: Extend the single-file configuration contract

Files:
- Modify: internal/config/config.go
- Test: internal/config/config_test.go
- Modify: configs/guardian.example.yaml
- Modify: docs/configuration.md
- Modify: skills/mihomo-guardian-production/SKILL.md

- [ ] Step 1: Write failing configuration tests.

Test configured target order, custom source groups, locked/all scope, defaults, duplicate IDs, incomplete order, missing lock keys, invalid filters, duplicate listeners, non-loopback listeners, invalid thresholds, and compatibility when quality is absent. Use a fixture with these values: order primary/reserve; primary source group MAIN, provider main-channel, locked scope and lock key main; reserve source group BACKUP-USA, provider backup-channel, all scope, filter 美国, listeners 17990 and 17991.

- [ ] Step 2: Run focused tests and verify RED.

    docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine \
      sh -c 'go test -mod=vendor ./internal/config -run Quality -v'

Expected: compile or assertion failure because the quality schema does not exist.

- [ ] Step 3: Add the schema and normalization.

Add QualityConfig, QualityTarget, QualityThresholds, QualityStabilityConfig, and QualityRetentionConfig. QualityTarget fields are ID, SourceGroup, Provider, Scope, LockKey, NodeFilter, and Listener. Parse durations from strings and default full scan to 720h, retry to 24h, per-node timeout to 180s, stability summary to 1h, history window to 24h, and stale cutoff to 26h.

Validate IDs with [a-z0-9][a-z0-9_-]{0,31}; require every target exactly once in quality.order; allow only locked/all scope; require lock_key for locked scope; require unique HTTP loopback listeners with explicit ports; compile node_filter; validate score, confidence, sample, retention, and latency bounds. Enabled quality requires one target. Missing or disabled quality remains valid for existing deployments.

- [ ] Step 4: Run configuration tests and verify GREEN.

Run the same Docker command and expect every focused configuration test to pass.

- [ ] Step 5: Update the example and operator documents.

Document user-defined targets and order, locked versus all, score increase behavior, immutable baseline, IP identity, hourly mihomo history aggregation, and the 20-point baseline drop. Do not describe MAIN, BACKUP-USA, or 美国 as required names.

- [ ] Step 6: Commit.

    git add internal/config/config.go internal/config/config_test.go \
      configs/guardian.example.yaml docs/configuration.md \
      skills/mihomo-guardian-production/SKILL.md
    git commit -m "feat: add configurable quality targets"

## Task 2: Inject isolated quality groups and listeners

Files:
- Modify: scripts/mihomo_config_patch.py
- Modify: scripts/discover.py
- Modify: scripts/install.sh
- Test: tests/test_mihomo_config_patch.py
- Test: tests/test_discovery.py
- Create: tests/test_quality_patch.py

- [ ] Step 1: Write failing patch tests.

Use custom source groups MAIN, BACKUP-USA, and EU-RESERVE. Assert one generated GUARDIAN-QUALITY- plus target ID select group and one guardian-quality- plus target ID listener per target. Preserve all source groups, CHANNEL, providers, ports, networks, and rules. Test provider-backed targets, static-node targets, idempotency, name collisions, duplicate ports, missing source groups, non-loopback listeners, and empty static groups.

- [ ] Step 2: Run focused Python tests and verify RED.

    pytest -q tests/test_quality_patch.py tests/test_mihomo_config_patch.py tests/test_discovery.py

Expected: missing function or assertion failures.

- [ ] Step 3: Implement patch_quality_targets.

The function signature is:

    def patch_quality_targets(
        text: str,
        targets: Sequence[Mapping[str, object]],
    ) -> str:

Parse only top-level proxy-groups and listeners using the existing narrow parser. For each target derive GUARDIAN-QUALITY- plus the validated target ID, resolve source_group, use the target provider when present, or copy exact static proxy names otherwise. Add a mixed listener on 127.0.0.1 and the target port. Replace only blocks generated by the tool; reject user-name collisions and any source-group or CHANNEL mutation. Preserve mode, owner, line endings, and unrelated YAML.

- [ ] Step 4: Implement deterministic port discovery.

Preserve an existing valid target port. Otherwise try 17990, 17991, and consecutive ports; reject ports in mihomo configuration and ports bound in container proc/net/tcp and proc/net/tcp6 read through read-only docker exec. Return unique ports and fail closed when the socket table is unavailable. Write loopback listener URLs into generated quality target infrastructure without replacing user-selected valid ports.

- [ ] Step 5: Run tests and commit.

    pytest -q tests/test_quality_patch.py tests/test_mihomo_config_patch.py tests/test_discovery.py

Expected: all focused tests pass.

    git add scripts/mihomo_config_patch.py scripts/discover.py scripts/install.sh \
      tests/test_quality_patch.py tests/test_mihomo_config_patch.py tests/test_discovery.py
    git commit -m "feat: inject isolated quality listeners"

## Task 3: Persist quality identities and reports

Files:
- Create: internal/quality/model.go
- Create: internal/quality/store.go
- Test: internal/quality/store_test.go

- [ ] Step 1: Write failing store tests.

Test that saving scores 80 then 94 for the same target/provider/node/IP leaves baseline 80, latest 94, and best 94. Test that changing IP from 1.2.3.4 to 5.6.7.8 creates separate records and a new baseline. Also test atomic writes, corrupt-file preservation, hashed safe filenames, progress cursor persistence, retention, and recommendation storage isolated from state.json.

- [ ] Step 2: Run tests and verify RED.

    docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine \
      sh -c 'go test -mod=vendor ./internal/quality -run QualityStore -v'

Expected: package or symbol failure because internal/quality does not exist.

- [ ] Step 3: Define domain types.

Create NodeKey, Report, NodeRecord, Baseline, StabilitySnapshot, ScanProgress, Recommendation, and SourceEvidence. NodeKey contains target, provider, node name, IP family, and IP. Report contains identity, vendor results, risk evidence, provider health/history freshness, quality score, stability score, effective score, confidence, completeness, and typed errors.

- [ ] Step 4: Implement atomic storage.

Use /guardian/data/ipquality with nodes, history, latest-target files, stability.json, stability-history.jsonl, recommendations.json, scan-progress.json, and scan.lock. Write 0600 temporary files, Sync, close, and rename. Lock concurrent writers. Move malformed files to .corrupt-UTC-timestamp. Complete reports create a baseline only when absent, always update latest, update best only upward, update last-good only for an eligible report, and never change baseline automatically.

- [ ] Step 5: Run tests and commit.

    docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine \
      sh -c 'go test -mod=vendor ./internal/quality -v'

Expected: all store tests pass.

    git add internal/quality/model.go internal/quality/store.go internal/quality/store_test.go
    git commit -m "feat: persist quality identities and reports"

## Task 4: Collect IPQuality-style evidence and calculate stability

Files:
- Create: internal/quality/ipquality.go
- Create: internal/quality/scorer.go
- Create: internal/quality/stability.go
- Test: internal/quality/ipquality_test.go
- Test: internal/quality/scorer_test.go
- Test: internal/quality/stability_test.go

- [ ] Step 1: Write failing scoring tests.

Cover 401/403/429 as reachable, 5xx/network errors as failed, two-of-three IP consensus, risk-source majority, IP conflicts, missing-source confidence, score clamping, and exact 20-point baseline detection. Test that EffectiveScore(90, 70) equals 84.

- [ ] Step 2: Run tests and verify RED.

    docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine \
      sh -c 'go test -mod=vendor ./internal/quality -run "Score|IP|Stability" -v'

Expected: missing symbols.

- [ ] Step 3: Implement proxied source collection.

The collector receives a probe.ExternalClient built for the target listener and uses only explicit HTTPS sources. Add typed adapters for plain-text IP sources, JSON identity sources, and risk sources containing ASN, organization, hosting, proxy, VPN, Tor, blacklist, and abuse fields. Reuse configured vendor probes for OpenAI, Gemini, Anthropic, OpenRouter, and DeepSeek. Run at least two attempts for critical vendors and require two agreeing IP sources for complete identity. Preserve timeout, DNS, TLS, HTTP, parse, and source errors. Never invoke shell tools or dynamically download IPQuality.

- [ ] Step 4: Implement the weighted score.

Use quality components vendor reachability 30, IP/ASN/region consistency 15, risk/blacklist 20, and data confidence 5. Use stability components availability 50%, latency 30%, jitter 20%. Calculate effective_score = quality_score * 0.70 + stability_score * 0.30. Normalize only available components, mark incomplete and lower confidence for missing/disagreeing required data, round at the end, and clamp 0–100.

- [ ] Step 5: Implement hourly stability aggregation.

Add:

    func AggregateStability(
        proxies []mihomo.Proxy,
        node string,
        now time.Time,
        cfg config.QualityStabilityConfig,
    ) StabilitySnapshot

Use provider history inside the configured window, reject stale entries, require minimum samples, compute p50/p95/max/jitter and sample coverage, and return unknown for insufficient evidence. Never create public requests or mihomo delay calls here.

- [ ] Step 6: Run all quality tests and commit.

    docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine \
      sh -c 'go test -mod=vendor ./internal/quality -v'

Expected: all evidence, score, and stability tests pass.

    git add internal/quality/ipquality.go internal/quality/scorer.go \
      internal/quality/stability.go internal/quality/*_test.go
    git commit -m "feat: score IP quality and mihomo stability"

## Task 5: Scan arbitrary targets sequentially

Files:
- Create: internal/quality/scanner.go
- Test: internal/quality/scanner_test.go

- [ ] Step 1: Write failing scanner tests.

Use a fake mihomo API recording every selection. Test configured order, locked scope from state.ProviderLocks[lock_key], all scope preserving provider/source order and applying only target filter, one-node failure continuing later nodes, progress resume, stale history becoming unverified, and zero writes to source groups or CHANNEL.

- [ ] Step 2: Run tests and verify RED.

    docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine \
      sh -c 'go test -mod=vendor ./internal/quality -run Scanner -v'

Expected: missing scanner symbols.

- [ ] Step 3: Define scanner interfaces.

Use:

    type MihomoAPI interface {
        Heartbeat(context.Context) error
        GetProxy(context.Context, string) (mihomo.Proxy, error)
        GetProvider(context.Context, string) (mihomo.Provider, error)
        SetProxy(context.Context, string, string) error
    }

    type Scanner struct {
        API      MihomoAPI
        Reports  *Store
        State    *state.Store
        Logger   Logger
        External func(string, time.Duration) (*probe.ExternalClient, error)
    }

    func (s *Scanner) Scan(context.Context, config.Config) error
    func (s *Scanner) ScanTarget(context.Context, config.Config, config.QualityTarget) error

Resolve the source group exactly, intersect provider inventory with source-group nodes, apply the target regex, and derive the isolated group as GUARDIAN-QUALITY- plus target ID.

- [ ] Step 4: Implement fail-closed sequential scanning.

For every target in quality.order, load guardian state, resolve candidates, select only the generated quality group, build the target listener client, collect evidence, read provider history, calculate scores, save the report, update the cursor, and log completion. Use per-node contexts. Continue after any node failure. If heartbeat fails, stop without selection or public requests and return a quality-link error. Only reports with valid identity, confidence, connectivity, provider health, and stability can become recommendations.

- [ ] Step 5: Run scanner tests and commit.

Run the focused quality package command and expect all scanner tests to pass.

    git add internal/quality/scanner.go internal/quality/scanner_test.go
    git commit -m "feat: scan configured targets sequentially"

## Task 6: Generate recommendations and integrate realtime application

Files:
- Create: internal/quality/recommendation.go
- Test: internal/quality/recommendation_test.go
- Modify: internal/runtime/service.go
- Test: internal/runtime/service_test.go

- [ ] Step 1: Write failing recommendation tests.

Test that a higher-scoring alternative does not replace a connected sticky node; a baseline drop of 19 does not release it; a drop of 20 requires complete report, confidence at least 70, matching IP, provider alive/history, and fresh vendor connectivity; stale/corrupt recommendations are ignored; and applying a node recommendation never writes cfg.Groups.Channel.

- [ ] Step 2: Run tests and verify RED.

    docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine \
      sh -c 'go test -mod=vendor ./internal/quality ./internal/runtime -run "Quality|Recommendation|Sticky" -v'

Expected: missing integration or failing assertions.

- [ ] Step 3: Implement recommendation validation.

A recommendation contains target ID, source group, provider, node, IP identity, report time, effective score, baseline score, and reason. Accept only when fresh, complete, above minimum confidence/candidate score, connected, provider-healthy, and matching current target/provider/IP. Custom targets that do not correspond to the existing realtime main/backup roles remain reportable but are never written to production groups by the generic service.

- [ ] Step 4: Integrate runtime.Service.

Load recommendations after heartbeat and before provider candidate selection. Keep sticky-first behavior. If the current locked node is connected and above baseline minus 20, return Noop even when another node is better. If disconnected or confirmed to have dropped 20 points, recheck a candidate with provider metadata and critical probes, then call SetProxy only on the source provider group. Never use a recommendation to call SetProxy on cfg.Groups.Channel. Save score rises and rejection reasons without altering valid locks.

- [ ] Step 5: Run tests and commit.

    docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine \
      sh -c 'go test -mod=vendor ./internal/quality ./internal/runtime -v'

Expected: all recommendation and runtime safety tests pass.

    git add internal/quality/recommendation.go internal/quality/recommendation_test.go \
      internal/runtime/service.go internal/runtime/service_test.go
    git commit -m "feat: apply validated quality recommendations"

## Task 7: Add CLI and independent launcher supervision

Files:
- Modify: cmd/guardian/main.go
- Modify: deploy/start-guardian.sh
- Test: cmd/guardian/main_test.go
- Modify: tests/test_launcher.py

- [ ] Step 1: Write failing CLI and launcher tests.

Test quality-daemon parsing, one-shot quality run, quality status, and audited quality baseline-reset. Assert that a distinct quality child loop retries without signaling mihomo_pid, receives persistent data/log/config paths, and is cleaned only during launcher TERM/INT shutdown.

- [ ] Step 2: Run tests and verify RED.

    docker run --rm -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine \
      sh -c 'go test -mod=vendor ./cmd/guardian -run Quality -v'
    pytest -q tests/test_launcher.py

Expected: missing command/parser and launcher assertions.

- [ ] Step 3: Implement CLI commands.

Commands:

    guardian quality-daemon --config PATH --data PATH --logs PATH --secret-file PATH
    guardian quality run --config PATH --data PATH --logs PATH --secret-file PATH [--target ID]
    guardian quality status --config PATH --data PATH
    guardian quality baseline-reset --config PATH --data PATH --target ID --node NAME --ip ADDRESS

Use the existing direct mihomo API client and secret handling. Return a quality-link error on local API loss so only the quality supervisor restarts the daemon. Baseline reset requires exact target/node/IP identity, preserves old history, and emits an audit event.

- [ ] Step 4: Add a separate quality loop.

Start and wait for quality-daemon independently. Each loop retries only its own child after one second. The quality loop never signals mihomo. On launcher TERM/INT clean up both child loops; when mihomo exits, stop both loops and exit with mihomo status. Keep guardian-only restart behavior unchanged.

- [ ] Step 5: Run tests and commit.

Run the focused CLI and launcher tests and expect all pass.

    git add cmd/guardian/main.go cmd/guardian/main_test.go \
      deploy/start-guardian.sh tests/test_launcher.py
    git commit -m "feat: supervise isolated quality daemon"

## Task 8: Integrate installer, rollback, status, docs, and skill

Files:
- Modify: scripts/install.sh
- Modify: scripts/rollback.sh
- Modify: scripts/status.sh
- Modify: scripts/discover.py
- Modify: tests/test_repository_contract.py
- Modify: README.md
- Modify: docs/configuration.md
- Modify: skills/mihomo-guardian-production/SKILL.md

- [ ] Step 1: Write failing installer/documentation contract tests.

Test read-only preflight reporting every custom target, source group, provider, scope, filter, listener, and port. Test aborts for missing groups, incomplete order, listener capability failure, generated-name collision, and non-loopback listeners. Test backups include mihomo config, guardian config, quality reports, progress, and manifest; rollback retains quality history and logs; status hides secrets; docs explain custom target editing and all score/safety rules.

- [ ] Step 2: Run tests and verify RED.

    pytest -q tests/test_repository_contract.py tests/test_quality_patch.py

Expected: missing quality installer/status/documentation markers.

- [ ] Step 3: Implement transactional installation.

Order:

1. Discover live Compose/config/provider/API/proxy data read-only.
2. Load the user guardian config; generate defaults only when quality is absent.
3. Validate custom targets without replacing source groups, providers, filters, or order.
4. Discover stable loopback listener ports.
5. Patch mihomo in memory and validate it.
6. Complete preflight with no writes.
7. Create a timestamped backup.
8. Atomically write binary/config/quality directories and patched mihomo config.
9. Apply the existing Compose launcher patch.
10. Verify mihomo, realtime guardian, quality daemon, production proxy, and every quality listener.
11. Preserve observe mode until smoke checks pass.

If Alpha lacks listeners or patch validation fails, abort before writes and leave the live deployment unchanged.

- [ ] Step 4: Extend rollback/status/docs.

Rollback restores configuration and generated listener state but never deletes /guardian/data/ipquality/history, /guardian/logs, or /guardian/backups. Read-only status shows quality daemon state, target IDs, listener ports, scan cursor, baseline/latest scores, and report times without secrets or raw report bodies. Document target editing, locked/all, fixed user order, hourly history, weights, baseline-drop, score-up behavior, IP changes, commands, backup, observe, rollback, and guardian-only restart.

- [ ] Step 5: Run tests and commit.

    pytest -q tests/test_repository_contract.py tests/test_quality_patch.py \
      tests/test_mihomo_config_patch.py tests/test_discovery.py

Expected: all focused tests pass.

    git add scripts/install.sh scripts/rollback.sh scripts/status.sh \
      scripts/discover.py tests/test_repository_contract.py README.md \
      docs/configuration.md skills/mihomo-guardian-production/SKILL.md
    git commit -m "feat: deploy configurable quality scanning safely"

## Task 9: Full regression and production-safe verification

Files:
- Modify only files required by a failing verification test.

- [ ] Step 1: Run Python tests.

    pytest -q

Expected: zero failures and every skip understood.

- [ ] Step 2: Run Go tests and vet through the mihomo namespace.

    docker run --rm --network container:mihomo-cliproxy \
      -e HTTPS_PROXY=http://127.0.0.1:7890 \
      -e HTTP_PROXY=http://127.0.0.1:7890 \
      -v /上传/mihomo-guardian:/src -w /src golang:1.24-alpine \
      sh -c 'go test -mod=vendor ./... && go vet -mod=vendor ./...'

Expected: all packages pass and vet reports no errors.

- [ ] Step 3: Build and inspect.

    make build CONTAINER=mihomo-cliproxy
    file dist/guardian

Expected: exit 0 and a static Linux amd64 executable.

- [ ] Step 4: Run safety scans.

    git diff --check
    git status --short
    rg -n --hidden --glob '!vendor/**' --glob '!.git/**' \
      'controller_secret|Bearer [A-Za-z0-9._-]{16,}|api[_-]?key|token:|password:' \
      internal scripts configs docs skills README.md

Expected: only documented field names and redaction tests match; no real secret, state, log, cache, or build artifact is staged.

- [ ] Step 5: Run live read-only checks before apply.

    ./scripts/status.sh --read-only --container mihomo-cliproxy
    ./scripts/install.sh --preflight --container mihomo-cliproxy

Expected: current mihomo process/channel, custom targets, provider mappings, loopback listener ports, and persistent root are reported; no service, container, or configuration changes occur.

- [ ] Step 6: Review the final diff before push or production apply.

    git status --short --branch
    git log -12 --oneline --decorate

Push or apply only after explicit user authorization and fresh verification review.
