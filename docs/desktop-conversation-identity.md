# Desktop conversation identity and forwarding evidence

## Observed failure

The live relay showed two request UUIDs for one saved Desktop conversation.
Both metadata records had the same exact Desktop `sessionId`, creation time,
project directory, and an overlapping bridge identity, but different
`cliSessionId` values. One request UUID had an account override and the other
used caller authentication. Matching titles alone cannot establish this link.

The existing exact scope/request-UUID binding therefore does not implement the
user's intended whole-conversation selection. Desktop's usage control plane
also continues using its original sign-in; a chat answer about usage is not
proof of the credential used for an inference request.

## Corrected contract

`sessionmeta` resolves only verified native metadata associations. A valid
`local_<UUID>` Desktop record identity, exact creation/project identity, and
unambiguous record relationships can associate multiple request UUIDs. Conflicts,
forks, titles, timestamps alone, arbitrary HTTP fields, and unknown metadata
never create an association. Resolution is read-only, bounded, and cached.

The relay accepts a trusted resolver:

```go
type ConversationResolver interface {
    Resolve(context.Context, []string) (map[string]string, error)
}
```

Returned values are canonical Desktop conversation UUIDs without the `local_`
prefix. The manager still namespaces bindings by admission scope. A verified
conversation binding is persistent and applies to later known aliases in that
scope. Unknown aliases remain caller-authorized until metadata establishes the
association. Existing exact request bindings remain usable for unmapped sessions.

`Config.Conversations` is optional. Nil preserves the original exact-ID behavior
for legacy state; grouped selection requires a resolver. Reset of an owned saved
group can use complete recorded membership when metadata is unavailable or the
resolver is nil. A persisted active group
cannot forward selected credentials without its resolver. Resolver implementations
must refresh their metadata snapshot for an alias missing from their cache.
The manager resolves every inference alias, including first-seen utility or child
requests, and checks the association again after credential preparation. It never
passes titles, prompts, models, parent IDs or body conversation IDs to this API.

Every source result must contain only requested, canonical network UUID keys and
canonical Desktop UUID values. An absent key means unknown. Empty, `local_`,
noncanonical or unsolicited entries are invalid. A missing or changed previously
verified association without current historical proof fails closed and preserves
the saved identity and selection.
`*desktoprelay.AssociationError` exposes only `error_code`, `ErrorCode()` and a
module sentinel, without retaining the resolver error or its chain:

| Code | Manager classification | Inference HTTP status |
|---|---|---|
| `conversation_association_unavailable` | `ErrUnavailable` | 503 |
| `conversation_association_invalid` | `ErrUnavailable` | 503 |
| `conversation_association_changed` | `ErrConflict` | 503 |

Management callers can classify changed association and CAS failures as 409.
Cancellation returns the context error and cannot publish a selection.

### Optional historical proof for rotated request UUIDs

Desktop can rotate its `cliSessionId` and remove the retired UUID from current
metadata while retaining the same immutable native conversation. The unchanged
base `ConversationResolver` remains strict about such missing aliases. A trusted
resolver may additionally implement:

```go
type HistoricalConversationResolver interface {
    ResolveHistorical(ctx context.Context, ids []string,
        known map[string]string) (map[string]string, error)
}
```

`Config.Conversations` keeps its existing type. When the optional method exists,
the core calls it instead of base `Resolve` for that operation. Its result is one
complete current/historical source snapshot and follows the same canonical UUID,
requested-key, cancellation and sanitized-error rules. The core does not combine
an old cached result with a separate fresh scan or fill missing aliases itself.

The core builds `known` internally from nonempty persisted
`Session.ConversationID` values in the requested scope, filtering to requested
network UUIDs. It copies these hints under the state lock and releases the lock
before source access. No public Manager API accepts arbitrary historical hints.
An unproved alias in another scope cannot borrow a peer's proof. If the same
network UUID has conflicting saved conversation IDs across scopes, the core omits
that alias's historical hint entirely. Matching proved scope records remain
independently namespaced.

The source may retain a retired alias only when `known[alias]` already records
that exact association and the same immutable native conversation is currently
valid. Any current contradictory claim, different conversation, ambiguity, veto,
truncated snapshot, missing native identity or changed immutable identity denies
historical proof. Titles, model names, body conversation IDs, group selection and
bridge overlap alone never establish history. These proof checks belong to the
trusted source implementation; the core keeps all source failures sanitized.

A legacy retired alias whose persisted conversation ID is empty therefore remains
unknown and caller-routed. History cannot retroactively attach it to a conversation
merely because a later request UUID has current proof. A previously proved retired
alias can continue selected forwarding and grouped CAS only while the optional
source supplies fresh historical proof. A fresh current alias still needs its own
current metadata proof before inheriting an active group. A source without the
optional capability keeps the earlier strict missing-association behavior.

### Read-only verification for server views

```go
func (m *Manager) VerifiedConversationAssociations(ctx context.Context) (map[string]string, error)
```

The map is keyed by exact `scopeUUID/sessionUUID`, with canonical Desktop
conversation UUID values. It is sparse: only existing observed aliases with
current or valid historical source proof are returned. A source result conflicting
with that scope's saved association is omitted. Source failures return the
sanitized association error, and cancellation returns the context error.

The helper snapshots cached observations, resolves sorted network UUID arrays
separately for each scope outside the state lock, and uses the same scoped,
unambiguous historical hints. The whole read is bounded by one management context.
It acquires no credentials, initializes no store, starts no listener and writes
no session, binding, native record or state file. Newly verified aliases can
appear in the result without being adopted into persistent observations.
Revoked/removed aliases are excluded before the result is returned.

Server views can use these scoped proof keys to mark a retired known alias
`association_verified: true` even when title metadata lookup has no current UUID
entry. Display metadata supplies no authority. Missing proof keeps saved ownership
visible with verification false and the safe recorded reset contract. Basic
`Sessions`, `Scopes`, `Status` and `ConversationBindings` reads do not resolve
metadata.

Management adds explicit grouped operations:

```go
type ConversationBinding struct {
    ScopeID        string `json:"scope_id"`
    ConversationID string `json:"conversation_id"`
    AccountID      string `json:"account_id"`
    Revision       uint64 `json:"revision"`
}

func (m *Manager) ConversationBindings() []ConversationBinding
func (m *Manager) BindConversation(ctx context.Context, scopeID, conversationID,
    accountID string, expectedRevision uint64,
    expectedMemberRevisions map[string]uint64) (ConversationBinding, error)
func (m *Manager) UnbindConversation(ctx context.Context, scopeID, conversationID string,
    expectedRevision uint64,
    expectedMemberRevisions map[string]uint64) (ConversationBinding, error)
```

The authenticated server integration owns these HTTP controls:

```text
POST /api/desktop-relay/scopes/{scope}/conversations/{conversation}/account
  {account_id, revision, member_revisions: {requestUUID: revision}}
DELETE same route
  {revision, member_revisions: {requestUUID: revision}}
```

`BindConversation` validates current trusted membership and every observed member
revision before one durable publication. Credential preparation happens outside
the mutation lock. Missing membership, a new member, changed revision, ambiguous
metadata, or cancelled operation cannot silently widen the selection.
`UnbindConversation` removes authority under complete membership and revision CAS,
using saved ownership when current metadata cannot verify an existing group.
Resetting a conversation clears its member overrides and its durable selection.
Admitted streams retain captured accounts.

`BindConversation` requires a running relay. Scope, conversation and member UUID
arguments accept case normalization. The manager resolves all observed aliases in
the scope before preparation, resolves them again before publication, then checks
runtime, scope, aggregate revision and the complete observed alias set under its
lifecycle and state locks. It conservatively rejects any new scope alias during
preparation, including an alias not yet verified. The member revision map must
exactly cover the target conversation's authoritative observed members. An
already-observed alias newly verified by metadata can join in the same explicit
selection when its current revision is included in that map. Valid changed or
missing associations belonging entirely to another conversation do not block a
target mutation. Associations into or out of the target still require validation.

The first group mutation uses expected revision zero and produces revision one.
Every later group mutation increments the aggregate revision. Member account IDs
change together in one state save, with member revision increments when account
or verified identity changes. An unchanged member account need not increment its
member revision; aggregate revision checks still fence prepared requests and 401
retries. An active group rejects exact `Bind` and `Unbind` overrides with
`ErrConflict`. Reset returns and retains an empty-account revision record so a
stale pre-reset selection cannot pass CAS. It clears all known member overrides
and stops future inheritance. Scope revocation removes all its group records.

Exact `Bind` captures the saved conversation revision before asynchronous
resolution and credential preparation. It also captures the resolved conversation
revision before preparation, then validates both under the final mutation lock.
These checks include empty-account records. First reset from revision zero and
repeated default reset both fence a delayed exact selection, even if all member
accounts were already default and no member revision changed. Management
cancellation is checked again immediately before publication.

### Safe reset while stopped or metadata is unavailable

`UnbindConversation` can reopen an existing private store with
`initializeLocked(false)` while stopped, closed or newly constructed. It never
starts a listener, changes enablement, prepares or refreshes credentials, or
activates a native account. Its context belongs to management, so stopping the
listener during metadata lookup does not cancel a removal of authority.

Recorded-only reset requires an existing `ConversationBinding` in the exact
scope/conversation namespace, including an already-default revision record.
`revision` must equal its current aggregate revision. `member_revisions` must
exactly cover the union of every saved session whose `ScopeID` and `ConversationID`
equal the target and every already-observed alias in that scope currently verified
by the resolver as belonging to the target. Every union member needs its current
revision. Reset clears their account
overrides and increments the group revision in one state save. Already-default
members need no member revision increment; clearing an override increments that
member's revision. A newly verified alias's association is persisted only by the
explicit reset, with a member revision increment. Saved conversation identities
and actual response evidence remain facts about prior observations.

When current metadata is absent, failed, ambiguous or changed for that saved
group, reset can remove only this recorded authority. It cannot create a new
association without current trusted proof, invent a group for an unknown UUID,
clear unrelated legacy overrides or omit a known member. With available metadata,
reset accepts the saved/currently-verified union even when one saved member has
lost proof and another observed alias has newly gained proof. This also works
while stopped. The additional alias must already exist in the observed scope,
must be explicitly included in complete member CAS, and must resolve to the target
in both lookups. An alias already associated with a different saved conversation
cannot be reassociated by this reset. Missing or failed source proof for an
unrecorded expected member returns `ErrConflict`; title and caller-body claims
never supply proof. Available metadata discovering an omitted union member also
returns `ErrConflict`. This authority-removing union rule does not relax grouped
selection's requirement for every target association to remain verified.
Unrelated conversations' missing associations do not block recorded removal.

Without an existing group record, initial reset at expected revision zero is
allowed only while running with current verified membership. This supports
explicitly clearing partial legacy exact selections. Stopped reset without that
saved group returns `ErrNotFound`. Unavailable metadata for initial running reset
returns the sanitized association failure. Neither path invents inactive
ownership from titles, bodies or unknown membership.

Before publishing, reset reopens state if Stop or Close released it, rechecks
scope ownership, group CAS and complete current target membership under lifecycle
and state locks, and checks cancellation at the final commit boundary. New known
members, stale member revisions, a newer group selection or revocation reject
the old reset. Available metadata also requires the complete observed alias set
to match the resolved snapshot. Failed lookups never override context cancellation
or operation deadlines. Reset success stops future verified aliases inheriting
the removed selection; already admitted responses keep their captured accounts.

The dashboard groups only exact scope/verified-conversation matches, exposes
internal request identities in details, and uses grouped controls. Conflicting
legacy selections require explicit selection. A previous single-ID override
is shown as partially applied until the user applies a whole-conversation
selection; it is not silently copied during a read-only GET.

`Session.ConversationID` persists as optional `conversation_id`. Observation
assigns it only after trusted resolution outside the state lock. Public reads
do not create bindings or migrate legacy selections. The explicit verification
helper reads source proof without persisting it. Native
aliases are the `session_id` values within one exact `scope_id/conversation_id`
group. Controls must send every alias's current `revision`, show partial legacy
selection explicitly, and retain alias, agent and parent identities in details.
An unrelated conversation with the same title and the same native UUID in another
scope remain separate. Parent identity alone never selects a child account.

When metadata lookup fails, server/UI views can retain the saved conversation ID
and mark their integration view `association_verified: false`. A saved group can
still expose its reset control while stopped or unverified. Send its current
binding revision and every revision in the saved/currently-verified observed
member union. GET may show a newly verified alias in this union without changing
its persisted association; explicit reset validates and adopts that association.
Selection controls continue
to require current trusted metadata and a running relay. The core API signatures
and persisted Session fields are unchanged by this reset policy.

The optional version-1 `conversation_bindings` state map uses
`scopeUUID/conversationUUID` keys. Loading validates UUIDs, scope ownership,
nonzero revisions, bounded account IDs, observed membership and active-group
member account consistency. State remains limited to 4 MiB and 4096 combined
session and conversation-binding records. Pre-group version-1 state still loads.

## Actual response evidence

Only received upstream responses for POST `/v1/messages` create message-route
evidence. Token counting, observations, local refusals, selection acknowledgement,
network failures, and a model's prose about quota do not prove an upstream turn.
Record only scope/session, selected-versus-caller route, selected account ID,
model, HTTP status, and response time. Never retain bearer values, credential
fingerprints, prompts, responses, or raw user metadata. New evidence must not be
replaced by an older concurrently finishing request. The UI displays the actual
model/status and says whether a selected credential was used, without claiming
independent provider-side billing verification.

The exact public record is:

```go
type ResponseEvidence struct {
    Route     string    `json:"route"`
    AccountID string    `json:"account_id,omitempty"`
    Model     string    `json:"model"`
    Status    int       `json:"status"`
    At        time.Time `json:"at"`
    Sequence  uint64    `json:"sequence,omitempty"`
}
// Session.LastResponse *ResponseEvidence `json:"last_response,omitempty"`
```

`Route` is `caller` or `selected`. Only selected evidence includes an account ID,
captured from the credential actually sent. `Model` is the unchanged request's
captured model, not a parsed response or a model's claim about its account.
`Status` is the actual upstream HTTP status. `At` is the UTC header-receipt time.
The manager assigns `Sequence` before body reading, metadata resolution and
credential preparation. It keeps the newest received Messages response per alias
by that request-start order. Evidence for a 401 retry updates within the same
sequence to the final received response. If retry dispatch fails, the received
401 remains evidence; a local 502 or 404 is never presented as upstream evidence.

Evidence is available as soon as upstream headers arrive, even while SSE continues.
An HTTP 200 records credential forwarding and response receipt, not stream
completion or independently verified billing. Selection acknowledgment can show
requested B while the last admitted response still reports captured A. The UI
should display both facts separately and keep per-alias evidence visible in group
details. A group summary can choose the highest request-start `sequence` among
its aliases' received-response records, using that record's captured route,
account, model and status. Public views return independent evidence values. Response telemetry
checkpoints with existing state saves and orderly shutdown, without a write per
response or any retained credential generation, raw metadata or content.

## Required regressions

- Two verified request UUIDs for one Desktop identity form one UI group.
- Selecting B applies to every verified observed alias and future verified
  aliases, while unrelated conversations and CLI sessions remain unchanged.
- Equal titles with different Desktop identities never share a binding.
- Membership ambiguity, stale CAS, cancellation, revocation, and a concurrent
  admitted stream preserve the appropriate prior state.
- Selected Messages use B, but usage/control-plane requests keep caller A.
- Count-tokens and blocked/failed dispatches do not claim successful inference.
- Actual response evidence is token-free, generation-safe, and shown distinctly
  from the requested account selection.
- First and repeated default resets fence delayed exact binds without relying on
  member revision changes.
- Saved groups reset while stopped or metadata is unavailable without credential
  acquisition, enablement changes, unproved associations or loss of response evidence.
- Mixed-proof reset accepts saved owners plus freshly verified observed aliases
  with complete member CAS, while refusing lost, changed or failed alias proof.
- Reset revalidates current known membership, group CAS, scope ownership and
  cancellation after read-only lookup and final-lock contention.

The package fixtures exercise these regressions through public Manager methods
and authenticated CONNECT requests. The end-to-end alias and streaming fixtures
use physical loopback HTTPS providers with dedicated fixture trust roots. Metadata
is synthetic. Checks are `go test ./internal/desktoprelay -count=1`,
`go test -race ./internal/desktoprelay -count=1` and
`go vet ./internal/desktoprelay`.
The earlier 103-test package passed all three checks after the safe-reset review
fixes. Two union-reset regressions add running/stopped mixed-proof membership,
durable adoption, complete member CAS and revalidated alias proof. The targeted
server regression is
`TestDesktopRelayIntegrationResetViewIncludesNewlyVerifiedObservedAlias`.
The current 105-test core package passed ordinary tests, race-enabled tests and
package vet. That exact server regression also passed ordinary and race-enabled
runs, using its isolated metadata, store and relay fixtures.

Historical-proof fixtures add legitimate alias rotation, group CAS continuity,
fresh-alias inheritance, unproved legacy/peer exclusion, conflicting scope hints,
source veto/removal/change/truncation, strict base-only behavior, safe read-only
proof views, cancellation and revocation. The complete current 110-test core
package passed ordinary tests, race-enabled tests and package vet.
