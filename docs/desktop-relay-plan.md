# Desktop task relay implementation

## Scope

Implement an opt-in, authenticated loopback CONNECT proxy for future local
Claude Desktop Code tasks. Intercept TLS only for `api.anthropic.com:443`.
Other approved public HTTPS destinations use blind tunnels. Keep Desktop's
sign-in, control-plane requests, project files, transcripts, and native CLI
selection in place. Source references are pinned in `desktop-reference-map.md`.

Current running sessions are outside verification. Startup and status queries
never edit Claude settings, quit Desktop, or change the machine's native login.
The user's follow-up requirement replaces the original manual setup flow with
explicit one-click Configure, a private backup, owned two-key environment merge,
and selective Restore. A separately confirmed Restart action applies settings
when current work is finished. No OS trust root is installed. Manual profiles
and environment instructions remain under Advanced.

## Agreed test seams

- Authenticated CONNECT and TLS: fixture clients, locally generated CA,
  authority/SNI/Host fencing, reused tunnels, cancellation, private-address
  refusal, and shutdown.
- Native inference forwarding: two fixture OAuth accounts, byte-identical
  Messages/count-tokens bodies and end-to-end headers, unchanged tools and
  signatures, complete SSE/errors, one same-account 401 retry, and thread
  full-history fallback without an inference send.
- Task control: observed session identities, explicit authenticated binding,
  atomic revisions, in-flight account snapshots, scope isolation, unbinding,
  and unidentified-child caller passthrough.
- Local management and setup: loopback plus independent control authority,
  cookie CSRF where applicable, no OAuth serialization, disabled-by-default
  lifecycle, and fixture-only UI actions.

Tests use isolated stores and injected transports. Success with a fake Desktop
client establishes the proxy contracts, not live Desktop billing acceptance.

## Module interface and management contract

The new module is `internal/desktoprelay`. It accepts a credential adapter with
`Prepare(context.Context, accountID)` and `RefreshRejected(context.Context,
accountID, rejectedAccessToken)`, returning an access-only credential with an
account ID. The production adapter delegates to Switcher's serialized account
preparation, checks the Claude provider, and never activates an account.

`New(Config)` constructs a disabled manager. `Config` contains `DataRoot`,
`Port`, `Source`, and optional fixture `Transport`/`DialContext` hooks. The
public interface is `Start`, `Resume`, `Stop`, `Close`, `Status`, `CreateScope`,
`DeleteScope`, `Sessions`, `Bind`, and `Unbind`. `Resume` starts only when an
explicit prior enablement was saved; `Close` shuts down without changing that
preference. Runtime state and persistence stay private to this module.

Management routes are local-only under `/api/desktop-relay`:

| Route | Result |
|---|---|
| `GET /api/desktop-relay` | `{status, scopes, sessions}` without tokens |
| `POST /api/desktop-relay/start` | Start/save explicit enablement |
| `POST /api/desktop-relay/stop` | Stop/save explicit disablement |
| `POST /api/desktop-relay/scopes` with `{label}` | New scope and one-time `{proxy_url, ca_path, env}` setup |
| `DELETE /api/desktop-relay/scopes/{scope}` | Revoke that admission scope and its bindings |
| `POST /api/desktop-relay/scopes/{scope}/sessions/{session}/account` with `{account_id, revision}` | Prepare and atomically bind an observed task |
| `DELETE /api/desktop-relay/scopes/{scope}/sessions/{session}/account` with `{revision}` | Restore caller-authorized inference for that task |

Status has `enabled`, `listening`, `address`, `ca_path`, `condition`, and
`validation` fields. Validation begins as `fixture_tested`, not live verified.
Scope views contain only `id` and `label`. Session views contain `scope_id`,
`session_id`, `account_id`, `revision`, `last_seen`, `requests`, and `in_flight`,
with optional observed parent/agent/model fields. No prompts, raw user metadata,
OAuth tokens, or response bodies appear in views.

Controls require loopback socket/Host checks and an explicit independent
management authority even when dashboard password authentication is disabled.
Authenticated browser sessions carry CSRF; device authentication or the local
management key can authorize controls without ambient cookies. Setup admission
credentials never authorize account selection. The web UI reads status, starts
explicitly, copies new-scope instructions, and switches only observed tasks.

## Native forwarding contract

Only exact `POST /v1/messages` and `POST /v1/messages/count_tokens` paths are
eligible for selected credentials. Preserve raw query and body bytes, model,
unknown fields, thinking signatures, deferred tools, tool references, images,
and end-to-end header values. Strip hop-by-hop and proxy credentials. Selected
OAuth reaches only the fixed verified HTTPS Anthropic origin, without redirects
or implicit fallback to another account, model, or API key. (Later, v1.11:
with "Switch Claude automatically" on, a request refused because its account
ran out of usage moves its conversation to another Switcher account; see the
module's "Out of usage".)

Resolve session identity from `X-Claude-Code-Session-Id` and the JSON string in
`metadata.user_id`. Require matching, nonzero UUIDs when both are present.
Reject ambiguous identity keys/headers. Missing, conflicting, unsupported,
unbound, or unidentified-child requests retain caller credentials.

Proxy admission identifies a configured client scope. Explicit management
binds an observed scope/session to an account. Metadata is an identifier, never
an instruction to select credentials. Shared-scope UUID routing assumes
cooperative local clients; it is not an OS isolation boundary against another
process that already holds that scope's admission secret.

Credential preparation occurs outside the selection lock. Recheck the binding
revision before admitting a request. An acknowledged switch controls later
admissions; already admitted streams retain their captured account. No native
account activation or global proxy selection occurs.

Selected thread requests are rejected before upstream submission with HTTP 400:

```json
{"type":"error","error":{"type":"invalid_request_error","message":"message threads are not supported on selected-account routes","details":{"error_code":"thread_unsupported_request"}}}
```

The real client must retry with full history. Never delete `thread` from a delta
and forward the remaining bytes as if they contained complete context.

## Setup and support status

Persist CA keys, admission secrets, and scope/binding state with private,
atomic writes. The CA is for the Anthropic API host only and is trusted through
`NODE_EXTRA_CA_CERTS`, not system trust. `HTTPS_PROXY` includes a dedicated
admission credential. Unknown traffic never receives stored OAuth.

Expose task selection separately from native Claude Code activation. Report
the relay as fixture-tested until a disposable live Desktop task proves A-to-B
subscription attribution, unchanged worker/session, intact tool continuation,
automatic thread recovery, and peer isolation. The Sonnet 5.5 account-bound
thinking restriction remains a server constraint and is accepted by the user.
