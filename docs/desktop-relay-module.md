# Desktop relay module

## Integration contract

`internal/desktoprelay` owns its dedicated `Config.DataRoot` directory. Its
parent must already exist, and all path components must be real directories,
not symlinks. Production supplies port `8789`, a verified Claude
`CredentialSource`, and leaves the fixture transport, dial and DNS hooks nil.
Port zero selects a loopback ephemeral port for fixtures.

```go
type Credential struct {
    AccountID string `json:"account_id"`
    AccessToken string `json:"-"`
}
type CredentialSource interface {
    Prepare(context.Context, string) (Credential, error)
    RefreshRejected(context.Context, string, string) (Credential, error)
}
```

`New(Config)` is lazy and disabled. `Start(ctx)` explicitly enables and persists
the preference. `Resume(ctx)` reads an existing store and starts only a previously
enabled relay. `Stop(ctx)` persists disablement. `Close(ctx)` shuts down while
retaining the enablement preference. None of these operations changes native
Claude credentials or another listener.

Automatic setup adds `Configure(ctx, settingsPath)`, read-only
`SetupStatus(settingsPath)`, and `RestoreSetup(ctx, settingsPath)`.
Configure starts the listener, creates/reuses one owned Desktop scope, saves a
private exact backup, and merges only the proxy/CA environment fields. A durable
setup journal records the original field values and mutation phase. Restore
checks ownership, restores those fields, preserves unrelated edits, and retains
the backup and admitted streams. Pending restore recovery completes its file
and directory durability barrier even when restored bytes already match.

Setup paths are explicit and absolute; this module never resolves HOME. The
server supplies the user settings path. Status reports only the condition,
settings path, scope ID, backup path, and required-restart flag. Configure,
Restore, and status never restart applications. A separate authenticated route
can restart Desktop only after explicit confirmation and owned setup evidence.
Managed setup blocks Stop/profile revocation until it is restored. The optional
version-1 `setups` record remains compatible with pre-setup relay state.

The remaining public methods are `Status() Status`, `CreateScope(label)
(ScopeSetup, error)`, `DeleteScope(id) error`, `Scopes() []Scope`, `Sessions()
[]Session`, `Bind(ctx, scopeID, sessionID, accountID, revision) (Session, error)`
and `Unbind(scopeID, sessionID, revision) (Session, error)`. Grouped methods are
`ConversationBindings() []ConversationBinding`,
`BindConversation(ctx, scopeID, conversationID, accountID, expectedRevision,
expectedMemberRevisions) (ConversationBinding, error)` and
`UnbindConversation(ctx, scopeID, conversationID, expectedRevision,
expectedMemberRevisions) (ConversationBinding, error)`. The member map type is
`map[string]uint64`. Errors include
`ErrConflict`, `ErrNotFound`, `ErrUnavailable`, `ErrBusy` and `ErrCredentialBusy`.
`CreateScope` and `Bind` require a running relay. Unbind and revocation can
also modify an existing disabled store without starting a listener.

`Status` serializes `enabled`, `listening`, `address`, `ca_path`, `condition`
and `validation`. `ReplyStatus` aliases `Status`. Validation is `fixture_tested`.
`in_flight` counts runtime request admissions independently of session records,
so scope revocation cannot hide streams that are still draining.
`Scope` contains only `id`
and `label`. `ScopeSetup` embeds `Scope` and adds `proxy_url`, `ca_path` and
`env`, with `HTTPS_PROXY` and `NODE_EXTRA_CA_CERTS`. Only `CreateScope` returns
the admission capability. Scope rotation is delete followed by create.
Revocation removes the scope's tasks and bindings. Existing intercepted tunnels
recheck scope admission for every HTTP request; existing opaque tunnels close.

`Session` serializes `scope_id`, `session_id`, `account_id`, `revision`,
`last_seen`, `requests`, `in_flight` and optional `agent_id`,
`parent_session_id`, `model`, `conversation_id` and `last_response`.
A new observed task starts at revision one.
Bindings are compare-and-swap operations. Preparation occurs outside the
binding lock; a revision change during preparation returns `ErrConflict`.
Bind checks management cancellation again under the final mutation lock,
immediately before changing the account or revision. Cancellation while waiting
behind a durable write cannot commit a binding.
Exact Bind also captures the relevant conversation revisions before asynchronous
resolution/preparation and checks them at publication, including empty-account
reset records. A first or repeated default group reset therefore fences a delayed
exact bind even when the member revision did not change.
Already admitted requests retain their account snapshot. Parent identity is
observational and never causes implicit child binding.

### Verified conversation selection and response evidence

`Config.Conversations ConversationResolver` optionally supplies read-only trusted
native associations through `Resolve(context.Context, []string)
(map[string]string, error)`. Inputs are network request UUIDs; returned values are
canonical Desktop UUIDs without `local_`. Unknown aliases remain caller-routed
unless they have an explicit legacy exact binding. The resolver must refresh for
aliases missing from its cache. The manager performs resolution outside its state
lock and rejects changed known associations, invalid results and source failures
through sanitized `*AssociationError` codes. Titles, models, parent identities and
body conversation IDs never establish membership. Basic state reads perform no
lookup or legacy selection migration.

An optional `HistoricalConversationResolver` adds
`ResolveHistorical(ctx context.Context, ids []string, known map[string]string)
(map[string]string, error)` without changing `Config.Conversations` or base
`Resolve`. The core chooses the optional method when present, supplying only
requested, core-persisted nonempty associations from that exact scope. It builds
the hints under its state lock and calls the source outside the lock. Conflicting
saved conversation IDs for the same network UUID across scopes omit that alias's
hint. An unproved peer cannot borrow another scope's historical association.

The method returns one current/historical proof snapshot. A retired alias may be
retained only if it was already proved by the core and the same immutable native
conversation remains currently valid, with no contradictory current claim,
different conversation, ambiguity, veto, truncation or changed identity. Missing
native identity and bridge/title/model/body hints cannot retain history. The core
does no historical filling itself. Without this capability, missing known aliases
still fail closed. Unproved legacy retired aliases stay caller-routed.

`VerifiedConversationAssociations(ctx context.Context) (map[string]string, error)`
returns sparse current/historical source proof keyed by `scopeUUID/sessionUUID`,
with canonical native Desktop conversation UUID values. It resolves per-scope
observed UUID arrays, filters conflicting saved associations, drops revoked
aliases and sanitizes source errors. It does not initialize a store, persist
associations, prepare credentials or start a listener. Server views may use it
to mark historical verification even when current title metadata lacks the
retired UUID. A fresh proof returned for an unproved observation is read-only.
See the identity contract for the source's negative-proof requirements.

`ConversationBinding` has JSON fields `scope_id`, `conversation_id`, `account_id`
and uint64 `revision`. Group controls require the current aggregate revision and
every authoritative observed member's revision. BindConversation requires a
running relay and current trusted associations. All scope aliases are resolved
before preparation and revalidated before one durable publication under lifecycle
and state locks. A changed member set, scope, runtime, aggregate revision, member
revision or cancelled context prevents selection publication. Only associations
relevant to the target must remain verified; another conversation's valid changed
or missing association does not block the target. Explicit consent can include a
newly verified, already-observed alias by supplying its revision. The first
aggregate revision is one; reset
retains an empty-account revision record to prevent stale CAS and future
inheritance. Member account fields change atomically, with revision increments
when account or association changes. Active groups reject legacy exact Bind and
Unbind overrides. Scope revocation removes grouped state while retaining managed
setup ownership restrictions.

UnbindConversation supports stopped, closed and freshly constructed managers by
loading the existing private state with `initializeLocked(false)`. For an existing
saved group it can clear authority using complete recorded membership and CAS
when metadata is nil, failed or cannot verify the target. Every recorded target
member must have that exact scope/conversation ID, and the member revision map
must cover all of them with current revisions. With current metadata, membership
is the union of recorded owners and resolver-verified already-observed target
aliases. Mixed proof is allowed: one saved member may have lost metadata while
another observed alias has gained trusted proof. Complete union CAS is required
while running or stopped. Additional aliases require proof in both lookups and
cannot have a conflicting saved association. Their verified identity is adopted
only during explicit reset. Failed/missing proof for an unrecorded expected alias,
an omitted union member or a stale member revision returns `ErrConflict`. Titles
and caller-body identities never authorize inclusion. Grouped Bind still requires
current verification for every target association.

Reset never prepares/refreshes credentials, activates accounts, starts listeners
or changes enablement. Its management context survives listener Stop while
lookup is pending, but cancellation or deadline expiry still prevents final
publication. The final lifecycle/state lock rechecks the saved group epoch,
complete current target membership and scope ownership, reopening existing state
if shutdown released the store. A new known member, stale revision, newer
selection or revoked scope rejects the pending reset. Existing response evidence
and admitted account snapshots remain intact.

Initial reset at expected revision zero without a saved group requires a running
relay and verified metadata membership. A stopped unknown/unsaved group returns
`ErrNotFound`; unavailable metadata cannot invent an inactive record. Server/UI
views may retain saved conversation IDs with `association_verified: false` and
offer reset for known saved groups while stopped or metadata is unavailable.
Send the saved binding revision and every saved/currently-verified observed union
member's revision. GET can expose this union without persisting new associations;
explicit reset revalidates it before publication. The public
method signatures and serialized core fields are unchanged.

`Session.LastResponse *ResponseEvidence` records only actual received upstream
Messages headers. Its JSON fields are `route`, optional selected `account_id`,
captured `model`, `status`, UTC `at` and optional uint64 `sequence`. Route is
`caller` or `selected`, based on the sent credential snapshot. Request-start
sequence ordering prevents older concurrent responses from replacing newer
evidence. A same-account 401 retry records its final received response; a failed
retry leaves the actually received 401. Count-tokens, control traffic, local
refusals, observations, network failures without responses and selection
acknowledgments produce no evidence. Header receipt does not establish stream
completion or independently verified billing. Public evidence values are copied
and telemetry checkpoints through existing saves and orderly shutdown.

See `desktop-conversation-identity.md` for exact Go types, grouped controls,
association error codes, member CAS semantics and UI alias/evidence guidance.

The caller must authenticate management routes independently of proxy admission.
This module exposes no management HTTP listener. It accepts only CONNECT on its
proxy listener, so a scope capability cannot select an account.

## Wire policy

The proxy accepts scope-specific Basic admission on IPv4 loopback. It intercepts
TLS only for the exact authority `api.anthropic.com:443`, checks exact SNI,
and fences decrypted Host and origin-form paths. Selected credentials reach
only the fixed, verified HTTPS Anthropic origin. The transport does not read
proxy environment variables or follow redirects. Blind public HTTPS tunnels
use validated numeric IPs after checking every DNS answer. The fixture
`LookupIP` hook lets tests exercise rebinding without external DNS; fixture
transports disable blind egress unless a fixture dialer is also supplied.
Fixture dialers also require injected DNS for hostname targets. The optional
`SyncDirectory func(*os.File) error` hook injects fixture durability failures.
Production leaves all fixture hooks nil.
`FixtureTLSRoots *x509.CertPool` gives isolated fixture TLS tests their own root
pool. Production leaves it nil. The module clones a supplied fixture pool and
always verifies upstream TLS; the hook cannot disable hostname verification.
`FixtureReadTimeout time.Duration` shortens request-body and rejection-drain
deadlines in isolated fixtures. Production leaves it zero.

### Credential failures for integration

`Bind` sanitizes `CredentialSource.Prepare` failures into `*CredentialError`.
Its public `Code` field serializes as `error_code`; `ErrorCode()` returns the
same value. `errors.As` can recover this type. Its message and unwrap chain
contain only module-owned text and sentinels, never the source error or tokens.

| Source result | Manager classification | Recommended management HTTP status | Inference `error.details.error_code` |
|---|---|---|---|
| `errors.Is(err, ErrNotFound)` | `errors.Is(err, ErrNotFound)` | 404 | `account_not_found` |
| `errors.Is(err, ErrBusy)` or `ErrCredentialBusy` | `ErrCredentialBusy` and `ErrUnavailable`, never `ErrBusy` | 503 | `credential_busy` |
| Other source failure or unusable credential | `ErrUnavailable` | 503 | `credential_unavailable` |

The credential adapter should wrap `desktoprelay.ErrNotFound` when an account
disappears after an API existence check and before or during preparation.
Bind preserves the old binding and revision in that race. Selected inference
returns a local 404 `not_found_error` with `account_not_found` before any
upstream send; it never falls back to caller credentials. Lifecycle/drain
`ErrBusy` remains a distinct 409 condition for management integrations.

Only exact POST Messages and count-tokens paths can use a binding. Models,
usage and other control traffic retain caller credentials. Missing and unbound
session IDs also retain caller credentials. A supplied identity must be a
nonzero UUID. Duplicate, conflicting and unsupported identities get a local
400 `session_identity_invalid`. This stricter rejection follows the explicit
implementation request, rather than the plan's earlier passthrough wording.

Identity comes from `X-Claude-Code-Session-Id` and the JSON object encoded in
the string `metadata.user_id`. Both must agree. UUID case is normalized in the
identity view only. The original request bytes remain untouched. The view
retains only session, parent, agent and model fields, never prompts or raw
metadata. Scope admission identifies a configured profile. Cooperative UUID
routing is not an OS isolation boundary against a process holding the same
scope capability.

Selected requests require the caller's exact `oauth-2025-04-20` Anthropic beta
token. The module preserves beta header values and rejects incompatible clients
with 400 `oauth_client_incompatible`; it never silently merges beta policies.
It strips hop-by-hop, proxy and Connection-nominated headers, replaces bearer
authorization and removes the caller API key only on selected inference.

Any top-level `thread` field on selected inference or count-tokens receives
the specified 400 `invalid_request_error` with
`details.error_code = "thread_unsupported_request"` before dispatch. The client
must submit full history. The module never deletes a thread delta or rewrites
models, signed thinking, tools, images, cache markers or unknown extensions.

A real selected-account 401 permits one same-account retry, before downstream
output, through `RefreshRejected` with the rejected token generation. The
credential owner must serialize refresh and adopt an existing successor. Retry
admission rechecks the binding revision. Ordinary refresh failure preserves the
first 401. An account removed during `RefreshRejected` instead receives the
sanitized 404 `account_not_found` failure, without a retry or binding mutation.
There is no 403/429, account, model, caller-token or network-error fallback.

## Limits and persistence

Request headers are bounded, raw and gzip-decoded bodies are limited to 16 MiB,
and only identity fields are interpreted. Header, TLS, body-read, preparation,
upstream-header, stream-idle and downstream-stall deadlines are separate.
Streaming has no server-wide WriteTimeout. SSE bytes pass through with flushes
and natural write backpressure. Cancellation closes the active upstream request;
shutdown closes owned sockets and has a bounded wait.
The listener admits at most 128 connections, and at most 32 intercepted HTTP
requests can run concurrently. Excess requests receive a local 503 rather than
buffering more bodies. Scope count is bounded at 128. Sessions and conversation
bindings share a 4096-record bound, and state is limited to 4 MiB.
Header and TLS deadlines are 15 seconds; request body and credential acquisition
deadlines are 30 seconds. Upstream headers, upstream stream inactivity and
downstream write stalls have separate two-minute limits. A request context has
a one-hour maximum lifetime. Write-stall deadlines reset with progress and are
cleared after each response. Blind tunnels preserve TCP half-close semantics.
Before every outer admission or inner HTTP rejection, the handler arms a
one-second read deadline and retains it through explicit `Body.Close` and
net/http cleanup. A successful, fully read body is closed before clearing that
deadline for streaming. Read failures keep their expired deadline; oversized
chunked bodies get a bounded remaining drain. The TCP RST-avoidance wait may add
another half second before an outer connection's admission slot is released.

The Unix private store uses directory mode 0700 and file mode 0600, descriptor-
relative operations, no-follow opens on every directory and file component,
exclusive random temporary files, file fsync,
atomic rename and directory fsync. `state.json` is a single durable record for
the CA key, certificate, scope capabilities, observed tasks and bindings.
Optional version-1 `conversation_bindings`, `conversation_id` and `last_response`
fields preserve legacy state compatibility. Loading validates group namespace,
membership, active member-account consistency and safe response-evidence fields.
`ca.pem` is the process-local trust export. OAuth credentials acquired from
`CredentialSource` are never persisted by this module. Exact settings backups
may retain secret-bearing values that already existed in the user's settings.
Those backups are private and are not included in JSON views or logs.
Corrupt state or unsafe paths fail closed.
The canonical state is durable before publishing the repairable CA export.
An exclusive directory lock prevents concurrent managers from overwriting the
same store. Close releases that lock. Telemetry checkpoints on orderly shutdown;
new observations and binding changes persist immediately.

A post-rename fsync failure is a commit-uncertain error. The module retains the
committed revision in memory, reports `ErrUnavailable` and `store_error`, and
suspends admissions. It does not pretend that disk rolled back. The management
caller must read status and session revisions again after close/reload before
retrying a change. A timed-out shutdown fences a new Start with `ErrBusy` until
the old runtime has drained. Start or Resume then reclaims the finished runtime
under the lifecycle lock, checkpoints telemetry and releases old store/transport
resources. No second Close is required. Resume still honors a persisted Stop
preference and does not enable a disabled relay. Source implementations must honor cancellation;
the relay cannot terminate arbitrary code inside a credential adapter.

## References and licensing

The implementation is independent Go code based on the public contracts and
the pinned MIT source concepts in `desktop-reference-map.md`:

| Reference | Concept used or compared |
|---|---|
| opencodex `ef0297f86c4540c7d757c8595170d66f9c584aec` | Host-selective CONNECT, process-local CA setup, request-time selection and explicit thread full-history negotiation |
| CLIProxyAPI `6fecc6e5567912661654a4eaf9b8f5436facd1c2` | Credential-owner generation adoption and recovery before exposing a response |
| CCR `f2e01bfe0c01e0c7ea7a37077747473f69869048` | Comparison for explicit session identity and stream termination; its broader interception and beta mutation were not adopted |
| claude-swap `3a4e5c14873eb5b32f182d55c68da98ac8c0db45` | Account-owned credentials and refresh ownership; native filesystem mutation remains with the credential adapter |

No source code, comments, tests or line-by-line translations were copied.
The required thread error string and code are protocol contract values.
The pinned root licenses are MIT. Their original notices would be required if
code were copied in a future change; this implementation adds no such copy.

## Verification and parallel review

The conversation/evidence regressions add real loopback HTTPS alias routing,
legacy partial selection, first-seen verified children, separate same-title
conversations, authoritative membership CAS, aggregate CAS with unchanged member
accounts, cancellation behind durable writes, revocation, restart, sanitized
metadata failures, changed-association request admission, atomic commit uncertainty,
state bounds and invalid schema rejection. Evidence fixtures cover selected
Messages versus caller usage, count-tokens and local/network blockers, final 401
retry responses, captured account selection, SSE continuity and concurrent
request-start ordering. Run the complete current package with:

```sh
go test ./internal/desktoprelay -count=1
go test -race ./internal/desktoprelay -count=1
go vet ./internal/desktoprelay
```

The current suite has 110 named tests. Eight safe-reset review tests add delayed
exact-bind epoch fencing, stopped/reloaded reset with missing metadata, unrelated
association isolation, complete ownership/member CAS, Stop during lookup,
final-lock cancellation, concurrent membership/selection/revocation, and initial
reset verification rules. The earlier 95 tests include the 17 conversation/evidence
tests and all earlier relay and setup fixtures.
Two union-reset tests add mixed saved/current proof while running or stopped,
durable alias adoption, complete member CAS and rejection of missing, failed,
changed or lost current proof for additional aliases. The complete 105-test
package passed ordinary Go tests, race-enabled tests and package vet.
`TestDesktopRelayIntegrationResetViewIncludesNewlyVerifiedObservedAlias` also
passed ordinary and race-enabled server runs with isolated fixtures.

Five historical-verification tests cover rotated previously proved aliases,
current fresh aliases, unproved legacy and peer identities, cross-scope ambiguity,
negative native/source proof, strict base-only sources, read-only scoped maps,
unchanged disk/credentials, cancellation and revocation. All 110 current named
core tests passed ordinary and race-enabled Go tests, followed by package vet.

The earlier relay verification covered 52 named public-interface fixture tests,
including the original 43.
Additional blocker regressions cover incomplete rejected-body cleanup, all 128 admission
slots, startup after a timed-out shutdown, cancellation during the final bind
lock wait, and sanitized removed-account/contention failures.
They cover authenticated CONNECT, raw Host conflicts and rejected-socket reuse,
SNI and path fencing, pipelined ClientHello, reused TLS tunnels, DNS pinning and
rebinding, private/reserved destinations, opaque half-close and revocation,
selected upstream TLS verification, byte-preserving bodies and errors, gzip
bounds, peer/scope/child isolation, thread negotiation, beta compatibility,
generation-aware 401 recovery, CAS races, in-flight snapshots, cancellation,
backpressure, concurrency limits, restart fencing and private durable storage.
The no-network-retry fixture uses a real TLS `http.Transport` and forces a
failure on its reused connection, including an empty GET with an idempotency
key. TLS fixtures use dedicated root pools and isolated injected egress.

Targeted package checks on Darwin arm64:

```sh
go test -race ./internal/desktoprelay -run 'TestIncompleteRejected|TestFinishedTimedOut|TestBindCancellationDuringFinal|TestAccountRemoved|TestBindCredentialFailures|TestForwardCredentialFailures|TestRemovedAccountDuring401' -count=3
go test -race ./internal/desktoprelay -count=1
go vet ./internal/desktoprelay
```

All three commands passed in that earlier verification. The blocker regressions completed three race-enabled
runs, followed by all 52 tests with the race detector and package vet. These
checks exercised only this package, using
ephemeral loopback listeners, temporary stores and fixture-controlled egress.

Two read-only agents reviewed security and correctness in parallel. Their
actionable findings led to descriptor-relative no-follow traversal, explicit
post-rename uncertainty handling, canonical-state-first CA export, canonical
CONNECT authority syntax, preserved half-close, cleared write deadlines,
one outer admission attempt per socket and suppression of empty-body transport
retries. Each reproduced wire or persistence failure has regression coverage.
No review agent changed repository files. Subsequent independent reviews found
the rejected-body drain, finished-runtime reclamation, final-lock cancellation
and credential-classification blockers. Each fix has a red regression at the
public fixture interfaces, including durable-write contention and account
removal between checking and preparation.

## Real Desktop compatibility gates

Fixture success does not establish live Desktop support. A disposable future
environment must prove that the embedded runtime adopts authenticated
`HTTPS_PROXY` and `NODE_EXTRA_CA_CERTS`, emits the supported session identity,
retains the OAuth beta, and performs automatic full-history thread recovery.
It must also prove A-to-B subscription attribution on the same worker, intact
tool continuation and peer isolation. Account-bound signed thinking, including
the documented Sonnet 5.5 restriction, remains a provider-side compatibility
gate. No real Desktop task, provider or quota endpoint is part of these tests.
