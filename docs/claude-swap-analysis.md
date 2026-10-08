# Claude switching: claude-swap analysis and Switcher integration

## Source and verification

- Source: <https://github.com/realiti4/claude-swap>.
- Examined commit: `3a4e5c14873eb5b32f182d55c68da98ac8c0db45`, package version `0.27.0b1`.
- Clone: temporary checkout under the coding-session directory, not a runtime dependency.
- Upstream tests: `pytest -n 4 --dist loadgroup` in an isolated HOME, with
  `CLAUDE_CONFIG_DIR`, `CLAUDE_SECURESTORAGE_CONFIG_DIR`, and `GITHUB_ACTIONS` unset.
  Result: 2,284 passed, 4 skipped. `test_real_store_guard.py` was excluded because
  it deliberately targets real-store paths. CI-only real-Keychain tests stayed skipped.
- License: MIT. Attribution and full notice are in `THIRD_PARTY_NOTICES.md`.
  Plain binaries expose that notice through `switcher licenses`; macOS bundles
  include it in `Contents/Resources/ThirdPartyNotices.md`.

## What it actually switches

Claude-swap is a **Claude Code native-login switcher** for the CLI and Code
editor integration. It is not a Claude Desktop sign-in switcher or an HTTP
proxy. It manages native OAuth or API-key credentials, their account identity,
and backup generations. It leaves local projects and MCP definitions in place.

The core operation is in `switcher.py::_perform_switch`:

1. Resolve the destination and check session-profile drift.
2. Resolve the live credential's identity before mutation locks when local
   token-lineage evidence cannot identify its owner.
3. Take its own account-store lock, both Claude Code refresh locks, then the
   config lock. Re-read everything under those locks.
4. Capture the latest outgoing token only into its owning slot. Preserve
   foreign/unmanaged displaced credentials instead of poisoning another slot.
5. Compose target account-owned credentials with the **live machine-owned**
   OAuth integration fields.
6. Write native credentials and splice only `oauthAccount` into the current
   config. Update the selected slot, with rollback for partial failure.
7. Tell the user about running-client credential caches. macOS may take roughly
   30 seconds to notice; reopening Code applies the login immediately.

Do not use `/logout` as the switching mechanism: Claude Code can revoke the
departing account's saved refresh token.

## Repository map

| Files | Responsibility |
|---|---|
| `__init__`, `__main__`, `cli` | Package entry points, command parsing, add/switch/run/config/import/export/purge dispatch, human/JSON exit contracts. |
| `switcher` | Account orchestration, slot identities, outgoing capture, identity provenance, consume gates, default-profile switching, backups, recovery, quarantine coordination. |
| `credentials`, `macos_keychain` | Native and per-slot storage; macOS Keychain vs file routing; absence vs unreadability; API-key/OAuth mutual exclusion; shared-field composition; previous-generation retention. |
| `paths`, `fsutil`, `locking`, `claude_locks` | Profile/environment resolution, bounded filesystem retries, store exclusion, proper-lockfile directory locks and heartbeat/staleness rules shared with Claude Code. |
| `oauth` | Profile lookup, token fingerprints, refresh-grant outcomes, access/login expiry, usage parsing, account headroom, reset times. |
| `usage_store`, `poll_policy`, `cache` | Persisted last-good measurements, leased/coalesced fetch claims, error/backoff state, adaptive polling and bounded usage-API request budgets. |
| `autoswitch`, `pace` | Proactive threshold/strategy policy, target freshness, cooldown and hysteresis, dead-token quarantine, weekly usage pacing/projections. |
| `session`, `process_detection`, `mappings` | Per-terminal account profiles using `CLAUDE_CONFIG_DIR`, PID/IDE tracking, live-profile drift guards, config/MCP sharing, optional history sharing, directory-to-account mappings. |
| `models`, `snapshot_source`, `json_output` | Typed coherent snapshots, rollback records, monotonic usage reconciliation, stable additive JSON contracts for UIs/scripts. |
| `settings`, `migrations`, `transfer` | Validated persisted preferences, old-layout/backend migrations, portable account-only or full exports, imports and usage-reading exchange. |
| `printer`, `appearance`, `logging_config` | Terminal rendering, terminal background detection, logs, diagnostic formatting and provenance labels. |
| `menubar`, `launch_agent` | Optional rumps-based macOS UI and launchd service lifecycle. |
| `tui/app`, `dashboard`, `widgets`, `modals`, `autoview`, `data`, `theme`, `cswap.tcss` | Textual dashboard, asynchronous snapshots, account actions, monitoring, display settings and styling. |
| `update_check` | Version/update notification without controlling switching correctness. |
| `pyproject.toml`, `uv.lock` | Python 3.12+ packaging; Textual/truststore runtime dependencies, rumps optional on macOS, pytest development dependencies. |
| `.github/workflows/ci.yml`, `publish.yml` | Linux/Windows/macOS tests, CI-isolated Keychain contracts, package publishing. |
| `tests` | Fixture-isolated account-switch, storage, ownership, lock, session, refresh, API-key, UI, import/export, cadence, migration and lifecycle coverage. In-memory Keychain/profile fakes guard ordinary tests. |

### Native storage details

- Default config home: `~/.claude`; credentials fallback:
  `<config-home>/.credentials.json`.
- Global config: legacy `<config-home>/.config.json` if present; otherwise
  `~/.claude.json`, or `<CLAUDE_CONFIG_DIR>/.claude.json` for a custom profile.
- macOS default OAuth service: `Claude Code-credentials`, account name from
  `$USER`, then OS username. Managed API key uses the separate `Claude Code` service.
- Custom OAuth service hashes the **raw NFC-normalized** config-dir string with
  SHA-256 and uses its first eight hex digits. Resolving/cleaning before hashing
  would select a different Keychain item.
- `CLAUDE_SECURESTORAGE_CONFIG_DIR` can override secure storage separately from
  config identity; upstream resolves those independently rather than mixing stores.
- Denied/locked Keychain reads are not genuine absence. The plaintext fallback
  may hold a superseded refresh-token generation and must not be consumed.
- Upstream writes through `/usr/bin/security`; its short payload uses stdin hex.
  Long payloads fall back to argv. Switcher's port refuses oversized stdin
  writes instead of placing credentials in process arguments.

### Ownership and locks

Machine-shared credential fields are an explicit allowlist:
`mcpOAuth`, `mcpOAuthClientConfig`, `mcpXaaIdp`, `mcpXaaIdpConfig`, `pluginSecrets`.
The live copy wins, including key absence. `claudeAiOauth`, `trustedDeviceToken`,
and unknown fields remain target-account-owned.

Native refresh lock order:

1. `<config-home>/.oauth_refresh.lock`, stale after 60 seconds.
2. `<config-home>.lock`, same 60-second staleness.
3. `<global-config>.lock`, stale after 10 seconds.

Locks use atomic directory creation, mtime heartbeats, bounded acquisition,
and removal only by the owner. Switcher's existing Go mutexes alone cannot
coordinate with Claude Code's external refresh process.

Upstream's consume gate reads the current credential generation and classifies
its owner before refresh. An active native login or running isolated profile
can own the refresh grant; blindly POSTing a backup copy can invalidate the
other consumer or falsely mark the account dead. Successor persistence and
generation checks matter as much as the refresh request itself.

### Switcher's refresh and transaction recovery

An expired active native login is refreshed using the verified locked live
credential. The refresh locks serialize it with Claude Code's own refresh;
the issued successor is retained before writes, then installed in the native
stores and account record before the locks release. This follows upstream's
`_fetch_active_usage` path. Always deferring an expired active login to Code
strands idle clients and prevents quota checks, which caused the stale-usage
regression in v0.5.6.

Active recovery retains the native preimage and profile paths. It repairs a
pending native write before usage observation, import, or manual switching,
without restoring a consumed predecessor. Native identity and exact credential
generations are fenced; unrelated project-setting changes stay in place.
Known Keychain writer limits are checked before consuming the live grant.

Before any refresh POST, Switcher saves a private consumption intent.
An HTTP gateway error, timeout, lost response, or crash leaves that intent in
place. Only a recognized OAuth rejection with HTTP 400, 401, or 403 clears it
without an issued successor. A possibly consumed predecessor is never retried
just because an upstream error is transient.

Issued successors are saved to a private sidecar before account persistence.
For expired legacy records, the new access token resolves organization identity
after those bytes are retained. If sidecar storage fails, the account file holds
the issued bytes with a private pending-recovery marker. That marker survives
restart and requires identity verification and cleanup before another grant.
If verification already finished before a failed save, recovery reuses the
matching verified in-memory successor instead of requiring another lookup.
If both stores fail, the running process retains the bytes in memory while the
durable consumption intent blocks reuse after a restart.

Cleanup removes the intent before its sidecar. Explicit re-login archives old
recovery files and any in-memory-only successor after the new login is saved.
If archival fails, the bytes remain available and retirement is retried after
storage repair. Re-importing the same native predecessor cannot clear an
uncertain consumption intent; a genuinely new login can supersede it.
Imports keep native locks through account persistence to avoid
capturing one generation and saving it after a concurrent switch or refresh.

Native switching saves an exact snapshot and a pending transaction before
mutation. After native writes verify, the proxy's atomic routing state includes
the transaction receipt. Recovery checks that durable receipt if the final
native journal marker was lost, so an already committed switch is not rolled
back. Partial, uncommitted switches restore the original snapshot only when
each live store still matches the before or after version. Foreign changes are
left in place and the backup remains available.

### Session mode and autoswitch

`cswap run` uses separate profile directories for parallel account sessions.
It seeds credential files, tracks live PIDs, captures rotated credentials after
exit, and avoids overwriting a live session profile. Optional transcript sharing
merges history then links it; ordinary user config sharing is separate from
history sharing. Directory mappings select a profile per project.

The proactive engine is separate from switching mechanics. It considers account
5h/7d limits and optional per-model limits, ranks candidates by headroom or
soonest reset, freshens before activation, and uses cooldown/hysteresis to avoid
flip-flopping. Usage polling has its own budget/backoff/lease system. These are
not proof that an HTTP proxy transparently switches every native client.

## Comparison with Switcher v0.5.5

| Concern | Switcher before integration | Native integration |
|---|---|---|
| Manual Claude selection | Changed only proxy `active["claude"]`. | Claude account action changes native Code credential/config plus proxy selection after verification. |
| Claude CLI connection | Informational setup row; adding an account did not configure native routing. | Uses native credential stores directly. No `ANTHROPIC_BASE_URL` rewrite and no claim of a native request test. |
| Credential import | Hardcoded unsuffixed Keychain read, extracted only OAuth token fields. | Correct profile store; full account-scoped credential and identity capture, verified from token identity. |
| Active token rotations | Saved copy refreshed independently under Switcher-only mutex. | Reconcile verified native rotations; expired live generations refresh under Claude locks and persist to native storage before release. |
| Inactive token refresh | Ordinary provider POST/save. | Native ownership gate, compatible locks, bounded grant, durable successor recovery before another consumption. |
| Shared MCP credentials | Not part of the stored account. | Preserve live shared fields during composition, never carry another account's device token. |
| Config/history | Native config unchanged. | Splice `oauthAccount`, preserve projects/MCP/preferences. Actual transcripts and Desktop session indexes remain untouched. |
| Transaction failure | Only proxy-state write. | Private snapshots and journal, native verification, rollback, conflict-aware interrupted-switch recovery. |
| Account display | Proxy active could be mistaken for native active. | Separate native availability/activity in account views; settings explain native vs proxy vs Desktop behavior. |

## Integration boundaries and differences

- The port lives in `internal/claudecode`, with provider and proxy adapters.
  No Python subprocess/runtime or `cswap` installation is required.
- Explicit local Claude account actions perform native switching. Proxy-driven
  exhaustion failover remains a route selection, not a promise of native hot
  switching. The native badge represents observed native state.
- Default and same-store custom profiles are supported. Split config/secure
  storage environments are refused with an actionable message instead of
  guessing which profile should change. App-launched and terminal-launched
  environments can differ; the status reports the server's config path.
- The port fails closed on malformed config, inaccessible Keychain, or
  unresolved identity. It does not copy unverifiable live credentials into a
  saved account. This is stricter than some upstream legacy recovery paths.
  Failed bridge initialization also blocks legacy independent refresh; it does
  not silently bypass native ownership checks.
- Snapshots and refresh-successor sidecars are private and journaled under
  `~/.switcher/claude-native/`. They follow Switcher's existing 0600 account
  storage trust model. Conflicting external changes are left in place.
- Account identity includes the Claude organization UUID. Legacy IDs are reused
  only after organization verification or an exact native token-lineage match.
  Displaced generations are retained before adoption or re-login replacement.
- Automatic Claude usage checks have a three-minute minimum interval. Manual
  Recheck can run sooner; native credential observation still runs on the
  server's ordinary refresh tick. Upstream's complete adaptive polling engine
  is not part of this port.
- The native store mechanisms are fixture-tested. They depend on Claude Code
  internal storage/lock contracts and should be rechecked when Claude changes
  them. Running-cache delay is documented, not guaranteed as instantaneous.
- Parallel-session launching, directory mappings, account export/import,
  upstream TUI/theme/launchd machinery, and proactive threshold autoswitch are
  explained above but are not folded into this default-login switch path.
  Switcher later added a narrower automatic switch (v1.11): only once usage
  shows the account at its session or weekly limit, through this same verified
  switch path, with a ten-minute cooldown per account.

## Integration verification

- `make verify` passed, including Go tests, vet, the complete race suite,
  frontend checks, Swift menu-layout tests, and Darwin/Linux arm64/amd64 builds.
- After the final reviewed recovery change, `go test ./...`, `go vet ./...`,
  and race tests for `internal/claudecode`, `internal/provider/claude`, and
  `internal/proxy` passed again.
- An optimized production Swift menu executable compiled successfully.
- Headless Chrome fixtures passed for expanded and compact cards: native
  selection differs from proxy selection, activation shows pending/success
  feedback, failed activation preserves selection and displays backup details.
- A newly built plain binary printed the complete attribution through
  `licenses` without starting the server.
- Independent review found no remaining issues in the final recovery recheck.

Credential tests use temporary profiles, fake tokens, mock Keychain operations,
and fixture profile/grant callbacks. They cover both native stores, field
ownership, external rotations, locks, rollback, commit receipts, legacy
organization verification, ambiguous grant outcomes, persistence failures,
restart recovery, re-login retirement, auth, CSRF, and local-only mutations.
No real native account switch or provider-quota request was used for these
checks. Running-client adoption still depends on Claude Code's internal storage
and cache contracts; it was not exercised with a live client session.

The active-refresh hotfix adds a proxy/native integration regression that starts
with an expired live login and stale quota, then verifies one coordinated grant
replaces that quota with a fresh response. Native recovery tests also cover
failed writes, restart, import and switch ordering, writer preflight, unrelated
config updates, and unchanged-import consumption fences. The relevant race
tests, Go suite, vet, and independent recovery review passed. A local installed
build then passed the original forced-Recheck probe against the real usage
endpoint. That probe renewed the existing native generation without changing
the selected account and returned current quota; it sent no inference request.
