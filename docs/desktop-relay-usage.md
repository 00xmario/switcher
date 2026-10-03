# Claude Desktop task relay

## Current status

Real Claude Desktop Code traffic through the relay has been observed. The
implementation also has isolated protocol and account-selection fixtures.
Complete target-account billing, task continuity, and bundled-client
compatibility have not been verified together. This is a network intermediary,
not a zero-impact credential toggle. Routine tests never reconfigure or restart
current Desktop or CLI workers.

Version 0.5.12 fixes a local association-check failure that blocked ordinary
requests in earlier previews. With no active conversation-group selection in
the scope, caller and exact-session forwarding use cached discovery only and
do not depend on metadata scans. Selected conversation-group routing still
requires proof before substituting another account's credential.

Native Claude Code switching and this relay are separate controls. The native
account action changes the machine login. The relay changes only the credential
used for subsequent inference requests belonging to an explicitly selected
observed task. Desktop sign-in and its conversation list stay on the original
account. Nothing automatically selects a global account for relay tasks.

## Prepare a future test environment

Use the installed local preview or the reviewed source build. The Desktop relay
is not part of the public v0.5.6 release. Configure changes future worker setup;
restart Desktop only when current tasks are finished, or use a separate
disposable macOS environment for testing.

1. Open the loopback Switcher dashboard and select **Settings**.
2. Click **Configure Claude Desktop**. One operation starts the relay, creates
   or reuses an owned profile, saves a private backup, and merges only
   `HTTPS_PROXY` and `NODE_EXTRA_CA_CERTS` into the user settings `env` object.
   There is no JSON to copy or merge manually. The settings path and backup
   location are available in Setup details. The default relay address is
   `127.0.0.1:8789`; `--desktop-relay-port` can change it.
3. When existing Desktop tasks are finished, click **Restart Claude Desktop**
   and confirm. Cancel is selected by default because restart closes Desktop
   windows and can interrupt tasks. Configure and Remove setup never restart
   Desktop automatically. No Keychain or system trust root is installed.
4. Start one disposable **Local Code** task. Refresh the relay card and locate
   its profile and session UUID. If the task never appears, do not guess which
   worker is using the relay; inspect the test environment's proxy adoption.
5. Select a stored Claude account and choose **Switch conversation**. A completed
   switch governs subsequent admitted requests. Existing streams finish with
   their captured account. Another task using the same model is not selected.

## Identify a conversation

The Settings card is titled **Claude Desktop**, with **Account switching**
underneath. It shows requests in progress separately from a collapsed
**Recent conversations** list. Remembered sessions can be idle or from earlier
work; zero requests in progress does not prove every worker process is closed.

Saved chat titles and project basenames are matched by exact session UUID.
Desktop's `local_*.json` metadata supplies `cliSessionId`, `title`, and `cwd`.
A saved CLI `sessions-index.json` title is a fallback. Neither transcript JSONL
nor prompt text is scanned for this feature. Malformed, ambiguous, redirected,
or oversized metadata is skipped. Unknown sessions retain distinct short IDs.
Colliding titles also show a profile or session disambiguator. Client labels
come from the matched metadata, not merely the shared relay profile's name.

Conversations keep their original credentials by default. Select another
account and click **Switch conversation** only for a conversation you want to
change. **Use Claude sign-in** removes its override. Newly observed sessions do
not automatically inherit another conversation's selection. Changing accounts
requires no Desktop restart; restart is only for applying setup changes.

Title and project metadata is added only to the authenticated local response.
These display fields are not persisted into relay state and cannot choose an
account. The lookup is bounded and briefly cached; a newly saved or renamed
title may take a few seconds to appear after Refresh status.

## Verify what was actually switched

Desktop can store several internal CLI/request UUIDs for one native conversation
identity, especially across saved account copies. Matching titles alone is not
enough to associate them. The relay now verifies the exact saved Desktop identity,
creation/project evidence, and compatible bridge history before grouping them.
One explicit conversation selection applies to those verified aliases in its
admission scope, including future aliases whose metadata becomes verifiable.
Unmapped identities and different conversations remain separate.

Older exact-session selections may show **Partially applied**. Select the desired
account and apply **Switch conversation** once to create the whole-conversation
binding. The action checks the group revision and every observed member revision
before publication. **Internal request sessions** shows the underlying UUIDs,
models, agent/parent metadata, individual selections, and response evidence.

**Last upstream response** is recorded only after an actual upstream Messages
response. It identifies whether the request used caller credentials or the
selected account credential, the model, HTTP status, and time. Token counting,
local refusals, observations, and response-free transport failures are not
successful inference evidence. Request-start ordering prevents an older request
finishing later from replacing newer evidence. HTTP 200 establishes forwarding
and response receipt, not independent provider-side invoice attribution.

Desktop remains signed into its original account. Usage/control-plane traffic
retains that sign-in, and a model can also repeat usage information from prior
context. Asking it how much quota remains therefore does not verify the relay's
inference credential. Use the recorded upstream evidence for routing diagnosis
and separately corroborate the target account's subscription usage for billing.

When metadata is unavailable, saved ownership remains visible, but new grouped
account selections are blocked. A saved group can be reset to Claude sign-in
while stopped using its current group/member revisions, without acquiring
credentials or starting a listener. An initial unsaved group reset requires a
listening relay and current verified membership. Equal titles never establish
a binding. See [identity and evidence](desktop-conversation-identity.md).

Management requires a local socket and local Host plus a browser session with
CSRF, device authentication, or Switcher's independent local control key.
Proxy admission cannot authorize management. Admission capabilities and the CA
private key are stored in private files under `~/.switcher/desktop-relay/`.
Credentials obtained from the relay's credential source are not persisted by
the relay. Exact private settings backups can contain pre-existing secrets
already present in the user's settings; their paths and contents are not
returned as setup environment values or written to logs.

The default target is `~/.claude/settings.json`; an absolute
`CLAUDE_CONFIG_DIR` in the Switcher server environment selects its settings
file instead. Unrelated settings and environment keys are retained. Relative,
redirected, malformed, duplicate-key, or unsafe configurations fail closed.
Shared user environment settings also affect newly launched standalone CLI
processes. Manual profiles and environment copying remain under **Advanced**.

## Forwarding and isolation

Only exact POST `/v1/messages` and `/v1/messages/count_tokens` requests can use
task bindings. Other API requests retain the caller's credentials. Other
approved public HTTPS destinations are opaque tunnels. Selected credentials
go only to verified HTTPS `api.anthropic.com:443`; redirects are not followed.

Session identity is read from `X-Claude-Code-Session-Id` and the JSON string
inside `metadata.user_id`. Matching valid UUIDs are required when both exist.
Malformed or conflicting identities return a local error. Missing and unbound
identities retain caller authorization. Parent/agent metadata is observational:
an unidentified child does not automatically inherit its parent's account.

Profiles provide separate admission scopes. Within a shared scope, session
metadata assumes cooperative local clients. It is not an OS isolation boundary
against another process already holding the same admission capability.

Request body bytes, query strings, model names, signatures, deferred tools,
tool references, images, and end-to-end headers are retained. Transport and
conflicting authentication headers are handled separately. Selected requests
require the caller's OAuth beta. Body identity metadata is not rewritten to
match the selected account; real upstream acceptance of that combination is
one of the live compatibility gates.

Selected thread creation and continuation receive HTTP 400 with
`thread_unsupported_request` before upstream dispatch. The client must resend
full conversation history. Removing the thread field from a delta would lose
context, so the relay does not do that. The fixture submits the replacement
history explicitly; automatic real Desktop retry remains unverified.

An upstream 401 permits at most one same-account rejected-generation recovery
before downstream output, using Switcher's existing credential owner. Network
failures, 403s, and 429s are not retried with another account or model. A removed
selected account returns a typed error rather than reverting to caller billing.

## Stop and restore

Selecting **Caller auth** and applying it removes a task binding. This remains
available for persisted tasks while the relay is stopped. Revoke removes a
profile, its bindings, and its admission capability. Stop closes the relay's
owned connections, including streams still in flight. The UI shows the active
count before an explicit Stop. It does not stop Desktop or CLI processes.

Click **Remove Desktop setup** to restore the prior proxy/CA fields, retaining
unrelated edits and the original backup. This works even when the listener is
unavailable. Existing owned fields are checked before restoration so a foreign
replacement is not overwritten. Explicit restart is available after restoration.

Managed setup must be removed before stopping its relay or revoking its profile.
Manual Advanced profiles still require their own environment restoration.
There is no silent direct-network fallback. Admission counts remain visible
while revoked tasks' already admitted requests finish.

## Required live acceptance

Run these gates on one disposable future Local Code task, recording Desktop,
embedded Code, macOS, and model versions. Do not use current active work as the
test fixture.

| Gate | Pass condition |
|---|---|
| Ingress | The real Code worker adopts proxy/CA settings and emits a supported session identity and OAuth beta |
| Subscription attribution | Physical upstream authorization changes A to B, and isolated subscription observations corroborate B, without Console/API billing or fallback to A |
| Worker continuity | The worker start identity and Code session remain unchanged after switching |
| Tool continuation | A tool called under A executes once; its complete result and prior history reach B in the same task |
| Threads | Real Desktop automatically retries with complete history after `thread_unsupported_request` |
| Rich context | Deferred tools, nested tool references, compaction, and subagent behavior are verified |
| Peer isolation | Same-model peer tasks and CLI workers retain their own credentials and contexts |
| Lifecycle | Token rotation, original connector identity, cancellation, and saved resume behave correctly |

Sonnet 5.5 account-bound thinking can be dropped by Anthropic across unlinked
accounts even when inference succeeds. The user accepts that restriction;
testing should start with another available model. Other model and credential-
scoped resource restrictions must still be recorded honestly.

## Verification and reference comparison

The full `make verify` passed with isolated HOME/TMPDIR and downloads disabled.
It includes all Go tests and race checks, vet, Node regressions, Swift layout
fixtures, and Darwin/Linux arm64/amd64 builds. An optimized production Swift
menu executable compiled. Independent security and correctness reviews found
no remaining actionable findings after their reported blockers were fixed.
Mocked headless Chrome checks at desktop and narrow widths also passed the
Settings start/profile/setup/task-selection flow, peer isolation, ephemeral
setup cleanup, and caller-auth restoration while stopped, with no console errors.

The combined HTTP/TLS integration fixture proves same-tunnel caller A to
selected B, exact body/query/header preservation, complete SSE/tool content,
old-stream ownership during a switch, peer isolation, stopped unbinding,
persisted restart, scope revocation, typed failures, and same-account 401
recovery. It uses synthetic credentials and mocked upstream I/O, not actual
Desktop inference.

Durable source clones are retained beside Switcher in `switcher-references/`.
The [source map](desktop-reference-map.md) pins commits, files, functions,
licenses, and differences across CC Switch, CLIProxyAPI, Claude Code Router,
OpenCodex, and claude-swap. The [module contract](desktop-relay-module.md)
describes the implemented behavior and its fixture coverage.
