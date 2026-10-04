# Desktop implementation reference map

Source review date: 2026-10-02. The five repositories below are durable, source-only references for Switcher's Claude Desktop implementation. All findings refer to the exact pinned commits, not current upstream branches.

The best implementation candidate combines opencodex's narrow first-party CONNECT ingress, CC Switch's owned third-party profile writer, CLIProxyAPI's request selection and pre-output stream recovery, and claude-swap's native refresh ownership. CCR adds a useful comparison for Desktop gateway discovery and protocol-aware session identity. Account selection must happen for each decrypted HTTP request, not when its CONNECT tunnel opens.

This is static source analysis. Upstream tests cited below were read, not run. Desktop adoption, certificate trust, native refresh reactions, and client full-history retries were not exercised against a live app or provider.

## Durable sources and clone verification

Reference root: `/Users/mariomakdis/Documents/Makdis/Projects/switcher-references/`.

| Project | Verified GitHub origin | Verified `git HEAD` | Durable local path |
|---|---|---|---|
| CC Switch | `https://github.com/farion1231/cc-switch.git` | `b9e9620265a76b2c074a83644ae1ad5ec57c3c98` | `/Users/mariomakdis/Documents/Makdis/Projects/switcher-references/cc-switch` |
| CLIProxyAPI | `https://github.com/router-for-me/CLIProxyAPI.git` | `6fecc6e5567912661654a4eaf9b8f5436facd1c2` | `/Users/mariomakdis/Documents/Makdis/Projects/switcher-references/CLIProxyAPI` |
| Claude Code Router | `https://github.com/musistudio/claude-code-router.git` | `f2e01bfe0c01e0c7ea7a37077747473f69869048` | `/Users/mariomakdis/Documents/Makdis/Projects/switcher-references/claude-code-router` |
| opencodex | `https://github.com/lidge-jun/opencodex.git` | `ef0297f86c4540c7d757c8595170d66f9c584aec` | `/Users/mariomakdis/Documents/Makdis/Projects/switcher-references/opencodex` |
| claude-swap | `https://github.com/realiti4/claude-swap.git` | `3a4e5c14873eb5b32f182d55c68da98ac8c0db45` | `/Users/mariomakdis/Documents/Makdis/Projects/switcher-references/claude-swap` |

Verification commands for each durable clone were `git rev-parse HEAD`, `git remote get-url origin`, `git rev-parse --is-shallow-repository`, `git status --short`, `git count-objects -v`, and `git fsck --connectivity-only`. All pins and origins matched. All working trees were clean, all clones were non-shallow, no alternates were reported, and all connectivity checks passed.

Only the existing claude-swap research clone was a suitable full local source. It was reused with `git clone --no-hardlinks --no-checkout`, and the new copy's origin was reset to its GitHub URL. The other four existing research clones were shallow. The old opencodex clone also used `blob:none`. Those four durable copies were cloned fully from public GitHub. All requested commits were available without an additional fetch. The earlier research clones were only inspected.

The companion mapping is [`../../switcher-references/README.md`](../../switcher-references/README.md). Paths in the source tables below are relative to the named durable clone. Links include the pinned SHA and line range.

## Implementation comparison

| Concern | CC Switch | CLIProxyAPI | CCR | opencodex | claude-swap |
|---|---|---|---|---|---|
| First-party Desktop CONNECT | Reviewed Desktop path is a gateway profile, not a first-party CONNECT integration | Compatible HTTP APIs and provider executors, not a Desktop profile or CONNECT implementation in the reviewed path | General CONNECT TLS termination with host/path gateway routing | Dedicated CONNECT plus TLS listener for Desktop's embedded Code runtime; optional separate browser picker interception | Native Code credential switching, no HTTP proxy |
| Third-party Desktop profile | Separate Desktop app scope; direct or local gateway; `deploymentMode`, library entry and `appliedId` | Can supply a compatible gateway behind another project's profile writer | Gateway profile with static local key, pinned model list and discovery enabled | Gateway profile plus static, hybrid or discovery-only model modes; separate first-party mode | No Desktop gateway profile |
| Per-request OAuth selection | Managed Codex, Copilot and xAI credentials resolved during forwarding | Selector picks an auth record, executor installs that record's credential | Claude hook re-reads current Code access token per request; no multi-account Claude selection in that hook | Managed OAuth uses account and generation snapshots with selection revision checks | Changes native stored login; does not choose credentials for HTTP requests |
| Session affinity | Session propagation and provider routing; reviewed Desktop profile does not establish a Claude OAuth session pool | Explicit IDs, provider/model namespaces, parent aliases, fallback history-prefix matching | Copies Code identity into compatible Responses bodies; this is upstream channel affinity | Anthropic pool affinity uses real thread/session IDs; shared Desktop cache cohorts are excluded | Isolated native config profiles and optional transcript sharing |
| Streaming | Native byte-stream response path plus separate protocol transforms | Complete Anthropic SSE events; bootstrap before exposing stream; terminal errors do not trigger a replacement stream | Direct proxy pipes upstream responses; gateway core is an external package | Native passthrough and managed native Messages are separate from translation | No inference streaming |
| 401 and refresh owner | Managed OAuth managers own their credentials; no force-401 Desktop switching mechanism found | One refresh of selected local auth after a real 401; Home-owned auth only adopts newer Home snapshot | Claude hook reads access tokens; it does not refresh Claude grants | Caller-native passthrough preserves upstream status; stored OAuth has its own refresh owner | Compatible native locks, lineage checks and successor retention |
| Thread full-history retry | No explicit unsupported-thread response found in reviewed Desktop handlers | Native beta pass-through exists; no equivalent `thread_unsupported_request` response found | No equivalent unsupported-thread response found in reviewed routing paths | Explicit `400` unsupported-thread response before translated inference or token counting | Filesystem transcript sharing is unrelated to wire thread retry |

These are boundaries of the reviewed paths, not claims that every feature in these large repositories was audited.

## CC Switch: strongest third-party profile reference

### Mechanism and boundaries

CC Switch models Claude Code and Claude Desktop as independent app scopes. Its Desktop integration writes the `Claude` and `Claude-3p` root configuration files and an owned `configLibrary` profile. Local gateway mode writes a generated gateway token into that profile, not the upstream provider token. Direct mode writes an upstream bearer credential and requires native Anthropic Messages compatibility.

The profile contains `inferenceProvider: "gateway"`, `inferenceGatewayBaseUrl`, `inferenceGatewayAuthScheme: "bearer"`, `inferenceGatewayApiKey`, and optional `inferenceModels`. `_meta.json` records the owned entry and sets `appliedId`. Apply uses a multi-file operation with write intent and recovery. Returning to official mode sets `deploymentMode: "1p"`, removes the owned metadata entry, and clears owned gateway fields while retaining other profile preferences.

Model mapping exposes Desktop-compatible role IDs and keeps actual upstream names inside CC Switch. `labelOverride` changes display names, and `supports1m` expresses context capability. The pinned source accepts Sonnet, Opus, Haiku and Fable families. The English manual still describes only three role families. Treat the source as the pin's behavior and revalidate actual Desktop schema when implementing.

Desktop Messages and model-list routes authenticate the local gateway token and then use the ordinary forwarding machinery. Requests are buffered before retry. Each provider attempt clones the original parsed body. Streaming uses a response byte stream and holds the connection guard for its lifetime. Managed OAuth credentials are resolved during forwarding, with a provider-bound account ID preferred over a default account. This is useful for third-party routes to Codex, Copilot or xAI. It is not evidence of native Claude Desktop sign-in replacement.

### Source map

| File and lines | Key functions or types | Relevance |
|---|---|---|
| [`src-tauri/src/services/profile.rs:28-80`](https://github.com/farion1231/cc-switch/blob/b9e9620265a76b2c074a83644ae1ad5ec57c3c98/src-tauri/src/services/profile.rs#L28-L80) | `ProfileScope`, `apps` | Independent Claude Code and Desktop selection scopes |
| [`src-tauri/src/claude_desktop_config.rs:237-396`](https://github.com/farion1231/cc-switch/blob/b9e9620265a76b2c074a83644ae1ad5ec57c3c98/src-tauri/src/claude_desktop_config.rs#L237-L396) | `is_claude_safe_model_id`, `get_or_create_gateway_token`, `validate_direct_provider` | Desktop role validation, local admission token and direct-protocol restrictions |
| [`src-tauri/src/claude_desktop_config.rs:561-756`](https://github.com/farion1231/cc-switch/blob/b9e9620265a76b2c074a83644ae1ad5ec57c3c98/src-tauri/src/claude_desktop_config.rs#L561-L756) | `proxy_model_routes`, `model_list_response`, `map_proxy_request_model` | Separate menu route IDs from upstream model IDs |
| [`src-tauri/src/claude_desktop_config.rs:937-1067`](https://github.com/farion1231/cc-switch/blob/b9e9620265a76b2c074a83644ae1ad5ec57c3c98/src-tauri/src/claude_desktop_config.rs#L937-L1067) | `apply_provider_to_paths`, `restore_official_at_paths`, `write_desktop_files` | Owned multi-file apply and recovery |
| [`src-tauri/src/claude_desktop_config.rs:1099-1204`](https://github.com/farion1231/cc-switch/blob/b9e9620265a76b2c074a83644ae1ad5ec57c3c98/src-tauri/src/claude_desktop_config.rs#L1099-L1204) | `gateway_profile_patch`, `MetaPatch::apply`, `build_gateway_profile` | Field ownership, metadata registration and gateway JSON shape |
| [`src-tauri/src/proxy/handlers.rs:251-424`](https://github.com/farion1231/cc-switch/blob/b9e9620265a76b2c074a83644ae1ad5ec57c3c98/src-tauri/src/proxy/handlers.rs#L251-L424) | `handle_claude_desktop_messages`, `handle_claude_desktop_models`, `validate_claude_desktop_gateway_auth` | Authenticated Desktop gateway entry points |
| [`src-tauri/src/proxy/forwarder.rs:599-712`](https://github.com/farion1231/cc-switch/blob/b9e9620265a76b2c074a83644ae1ad5ec57c3c98/src-tauri/src/proxy/forwarder.rs#L599-L712) | `forward_with_retry_inner` | Bounded provider attempts with per-attempt body copies |
| [`src-tauri/src/proxy/forwarder.rs:1808-1871`](https://github.com/farion1231/cc-switch/blob/b9e9620265a76b2c074a83644ae1ad5ec57c3c98/src-tauri/src/proxy/forwarder.rs#L1808-L1871) | Managed Codex OAuth branch | Resolve selected token and corresponding upstream workspace together |
| [`src-tauri/src/proxy/response_processor.rs:150-210`](https://github.com/farion1231/cc-switch/blob/b9e9620265a76b2c074a83644ae1ad5ec57c3c98/src-tauri/src/proxy/response_processor.rs#L150-L210) | `handle_streaming` | Stream pass-through, header filtering and connection lifetime |
| [`docs/user-manual/en/2-providers/2.6-claude-desktop.md:16-25`](https://github.com/farion1231/cc-switch/blob/b9e9620265a76b2c074a83644ae1ad5ec57c3c98/docs/user-manual/en/2-providers/2.6-claude-desktop.md#L16-L25) | Support scope | Upstream documents restart-based profile adoption |

Switcher candidate: an owned Desktop gateway profile writer with a Switcher-specific UUID, transactional apply/restore, a local admission token, and an Anthropic-native route first. Add model conversion only as a separately specified feature. Do not copy CC Switch's role names or broad egress policy as assumptions about current Desktop.

## CLIProxyAPI: strongest Go selection and stream-bootstrap reference

### Mechanism and boundaries

CLIProxyAPI separates account selection from provider execution. `SessionAffinitySelector.Pick` namespaces bindings by provider, session and canonical model. Explicit client identities take precedence over inferred history-prefix matches. A still-usable bound credential wins over a recovered higher-priority credential. `OnResult` retains bindings on success and conditionally removes only bindings belonging to the failed credential. Request-scoped failures do not poison credential health.

`ClaudeExecutor` receives the selected auth record. `PrepareRequest` removes conflicting credential headers and installs either the selected API key or bearer. Its streaming path preserves complete SSE events for an Anthropic response target and uses translation only for a different target format. This is native-protocol forwarding with deliberate identity and header processing, not guaranteed byte-identical forwarding.

The manager reads stream bootstrap before returning a stream to the HTTP layer. A real pre-output 401 can refresh the same local auth once and retry. Once the result is exposed, the wrapper forwards errors and records failure instead of opening a new response stream. Per-auth refresh locks make concurrent rejected-token requests adopt an already-refreshed token. Registration epochs prevent a stale refresh from updating a replaced account record.

Home-owned credentials have a different owner. `RefreshHomeSelectionAfterUnauthorized` only adopts a newer snapshot already installed by Home. It does not consume Home's refresh grant. That ownership split is worth retaining in Switcher's native-store versus proxy-owned credentials.

The Claude beta policy has fixtures preserving the caller's `message-threads-2026-08-12` value. No explicit unsupported-thread/full-history negotiation was located in the reviewed executor or selector paths. Beta preservation alone does not prove an account change can continue a thread stored under another credential.

### Source map

| File and lines | Key functions or types | Relevance |
|---|---|---|
| [`sdk/cliproxy/auth/selector.go:909-1112`](https://github.com/router-for-me/CLIProxyAPI/blob/6fecc6e5567912661654a4eaf9b8f5436facd1c2/sdk/cliproxy/auth/selector.go#L909-L1112) | `SessionAffinitySelector`, `Pick` | Explicit IDs, reusable account bindings, provider/model isolation |
| [`sdk/cliproxy/auth/selector.go:1457-1605`](https://github.com/router-for-me/CLIProxyAPI/blob/6fecc6e5567912661654a4eaf9b8f5436facd1c2/sdk/cliproxy/auth/selector.go#L1457-L1605) | `OnResult`, `CanonicalSessionID`, `ExtractSessionID` | Conditional invalidation and session identity precedence |
| [`internal/runtime/executor/claude_executor.go:213-239`](https://github.com/router-for-me/CLIProxyAPI/blob/6fecc6e5567912661654a4eaf9b8f5436facd1c2/internal/runtime/executor/claude_executor.go#L213-L239) | `PrepareRequest` | Install the selected credential, remove conflicting auth |
| [`internal/runtime/executor/claude_executor_stream.go:404-549`](https://github.com/router-for-me/CLIProxyAPI/blob/6fecc6e5567912661654a4eaf9b8f5436facd1c2/internal/runtime/executor/claude_executor_stream.go#L404-L549) | `ExecuteStream` | Complete native SSE events, tool-name restoration, separate translation path |
| [`sdk/cliproxy/auth/conductor_stream.go:91-205`](https://github.com/router-for-me/CLIProxyAPI/blob/6fecc6e5567912661654a4eaf9b8f5436facd1c2/sdk/cliproxy/auth/conductor_stream.go#L91-L205) | `readStreamBootstrap`, `wrapStreamResult` | Pre-output failure detection and terminal stream ownership |
| [`sdk/cliproxy/auth/conductor_stream.go:208-367`](https://github.com/router-for-me/CLIProxyAPI/blob/6fecc6e5567912661654a4eaf9b8f5436facd1c2/sdk/cliproxy/auth/conductor_stream.go#L208-L367) | `executeStreamWithModelPool` | One same-auth 401 recovery before returning stream |
| [`sdk/cliproxy/auth/conductor_refresh.go:492-607`](https://github.com/router-for-me/CLIProxyAPI/blob/6fecc6e5567912661654a4eaf9b8f5436facd1c2/sdk/cliproxy/auth/conductor_refresh.go#L492-L607) | `RefreshHomeSelectionAfterUnauthorized`, `tryRefreshAfterUnauthorized`, `refreshAuthForRequestAtEpoch` | Owner-aware refresh, token re-read and serialized grant consumption |
| [`internal/runtime/executor/claude_executor_auth.go:149-182`](https://github.com/router-for-me/CLIProxyAPI/blob/6fecc6e5567912661654a4eaf9b8f5436facd1c2/internal/runtime/executor/claude_executor_auth.go#L149-L182) | `Refresh` | Local auth manager updates its stored OAuth token metadata |
| [`internal/runtime/executor/claude_executor_beta_passthrough_test.go:24-45`](https://github.com/router-for-me/CLIProxyAPI/blob/6fecc6e5567912661654a4eaf9b8f5436facd1c2/internal/runtime/executor/claude_executor_beta_passthrough_test.go#L24-L45) | Caller beta fixture | Thread beta preservation, not cross-account thread portability |

Switcher candidate: keep the selected-active-account policy, add an explicit request credential snapshot, and adopt bootstrap-aware stream delivery and conditional binding invalidation. CLIProxyAPI's round-robin fleet policy and history-prefix inference are not required for Switcher's active-account-first behavior.

## Claude Code Router: gateway discovery and channel affinity comparison

### Mechanism and boundaries

CCR writes an owned Desktop gateway library entry using `inferenceCredentialKind: "static"`, `inferenceGatewayAuthScheme: "x-api-key"`, `bootstrapEnabled: false`, a static `inferenceModels` list, and `modelDiscoveryEnabled: true`. It sets `deploymentMode: "3p"` and registers `appliedId`. Restore checks whether the currently applied root and metadata values still match CCR before restoring those keys. The inactive entry is retained for Desktop preferences.

Its general proxy handles CONNECT by creating a host-specific TLS server. Decrypted requests are routed to the gateway only when configured host/path targets match; other requests are forwarded to their destination. This is broader than opencodex's host-selective blind-tunnel design. The direct forwarding layer pipes streams and destroys an already-started response on upstream error.

`authenticateClaudeCode` calls `readClaudeCodeOauth` at request time, uses that live access token with a configured bearer fallback, removes `x-api-key`, and merges OAuth beta values. The reviewed hook contains no Claude refresh-grant request and no multi-account Claude selection. The scanner's broad native-store discovery is useful context, but Switcher's existing verified native ownership layer is the better integration point.

`applyResponsesSessionAffinity` propagates a Code session header or `metadata.user_id` into compatible Responses `prompt_cache_key` fields so channel-bound encrypted continuations remain on a consistent upstream channel. It explicitly skips Codex backend URLs, which reject these added fields. This is channel affinity, not a stored OAuth account binding.

The pinned CCR core delegates gateway execution to external `@the-next-ai/ai-gateway`. Its local retry classifier treats ordinary 401 as a client error, while explicit `model-chain` fallback accepts any status at least 400. Do not infer complete gateway streaming or token-refresh behavior from this repository's wrapper alone.

### Source map

| File and lines | Key functions or types | Relevance |
|---|---|---|
| [`packages/core/src/agents/claude-app/gateway-service.ts:143-194`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/packages/core/src/agents/claude-app/gateway-service.ts#L143-L194) | `applyClaudeAppGatewayConfig` | Static local key, model list, discovery and restart contract |
| [`packages/core/src/agents/claude-app/gateway-service.ts:211-222`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/packages/core/src/agents/claude-app/gateway-service.ts#L211-L222) | `restoreClaudeAppGatewayConfig` | Restore only owned selection fields |
| [`packages/core/src/agents/claude-app/gateway-service.ts:438-517`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/packages/core/src/agents/claude-app/gateway-service.ts#L438-L517) | `restoreClaudeAppOwnedKey`, `applyClaudeAppGatewayLibraryConfig`, `applyClaudeAppConfigMeta` | Field-scoped restoration and profile registration |
| [`packages/core/src/agents/claude-app/gateway-routes.ts:46-123`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/packages/core/src/agents/claude-app/gateway-routes.ts#L46-L123) | `buildClaudeAppGatewayModelRoutes`, `resolveClaudeAppGatewayRouteModel`, `buildClaudeAppGatewayInferenceModels` | Display/model separation and legacy route decoding |
| [`packages/core/src/proxy/service.ts:630-743`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/packages/core/src/proxy/service.ts#L630-L743) | `handleConnect`, `createMitmServer`, `handleProxyRequest` | General CONNECT TLS termination and target routing |
| [`packages/core/src/proxy/service.ts:909-979`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/packages/core/src/proxy/service.ts#L909-L979) | `forwardDirectRequest` | Response streaming and headers-sent error boundary |
| [`packages/core/src/gateway/core-runtime/local-agent-auth-provider-hook.ts:150-171`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/packages/core/src/gateway/core-runtime/local-agent-auth-provider-hook.ts#L150-L171) | `authenticateClaudeCode` | Per-request live access-token read, not grant refresh |
| [`packages/core/src/agents/local-providers/claude-code.ts:186-219`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/packages/core/src/agents/local-providers/claude-code.ts#L186-L219) | `readClaudeCodeOauth`, `scanClaudeCodeLogin` | Access-token discovery and native-store shapes |
| [`packages/core/src/gateway/core-runtime/responses-session-affinity.ts:29-104`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/packages/core/src/gateway/core-runtime/responses-session-affinity.ts#L29-L104) | `applyResponsesSessionAffinity`, `resolveResponsesSessionKey`, `isCodexResponsesUpstream` | Protocol-specific channel identity |
| [`packages/core/src/routing/failure-classifier.ts:10-31`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/packages/core/src/routing/failure-classifier.ts#L10-L31) | `classifyRouteFailure` | Distinguish normal status recovery from explicit model-chain fallback |
| [`packages/core/package.json:18-25`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/packages/core/package.json#L18-L25) | Core dependencies | Gateway core is outside this pinned source tree |

Switcher candidate: borrow the gateway field comparison and channel-identity rules when adding Responses translation. Prefer the narrower opencodex ingress for first-party Code traffic and Switcher's existing ownership checks for Claude credentials.

## opencodex: strongest first-party CONNECT and thread fallback reference

### First-party versus gateway mode

First-party Desktop mode leaves the app's ordinary claude.ai identity in place and redirects the embedded Code runtime through `HTTPS_PROXY` and `NODE_EXTRA_CA_CERTS`. The CONNECT proxy terminates `api.anthropic.com:443` at its local TLS listener; other destinations are blind tunnels. Only POST `/v1/messages` and `/v1/messages/count_tokens` enter the router. Other API paths relay upstream with caller credentials. This is a Code-tab inference integration, not Desktop Chat account switching.

The optional picker adds a separate Desktop egress profile containing only `egressProxyUrl`. Its CONNECT selector distinguishes browser User-Agent traffic from embedded Code traffic and can rewrite browser model discovery through a separate claude.ai terminator. The source explicitly treats User-Agent as a routing hint, not authorization. Desktop's egress profile cannot carry proxy credentials in this implementation, while the Code CONNECT listener requires a per-install credential. The browser picker is a separate feature with separate trust and lifecycle requirements.

Gateway mode instead uses Desktop's third-party profile. The source supports static, hybrid and discovery-only lists, with static as the default. Model aliases have an explicit registry and collision checks. Real Anthropic model IDs stay out of that registry so native caller passthrough remains possible.

### Credential selection and ownership

There are two distinct inference credential paths:

1. Genuine, unmapped Claude models with an accepted caller-native credential take `anthropicNativePassthrough`. This branch forwards the caller's credential, not opencodex's selected stored OAuth account. It keeps native protocol fields and returns upstream error statuses. It also performs image/tool-ID normalization, so it is not strictly byte-identical despite the source's high-level passthrough comments.
2. Managed routes resolve an `OAuthAccessSnapshot` with account ID and credential generation, then compare-and-swap the captured account-selection revision. A concurrent manual selection supersedes a stale proposal. The native managed OAuth Messages lane is behind `protocols.rollout.managedMessagesNativeOAuth` and repeats this validation immediately before sending. It declines pooled OAuth because that lane does not implement the pool's rotation and affinity; pooled requests use the bridge path instead. Its OAuth tool-name rewrite has a matching response and SSE restoration step, so native protocol compatibility still needs a defined transformation boundary.

For Switcher to change the account serving a genuine native Claude request, it must choose the managed-native credential mode explicitly. Copying opencodex's default caller passthrough would leave the client's original account serving those requests.

Anthropic pool affinity uses real client thread/session IDs. A shared Desktop `prompt_cache_key` cohort must not bind multiple conversations to one account by accident. Manual selection clears prior affinity. Background `local-cli` account slots cannot adopt or refresh through the global Code login. The stored Anthropic refresh path uses account locks, generation checks and durable refresh intent so an ambiguous grant outcome blocks blind replay. These are opencodex store locks, not proof of coordination with Claude Code's external refresh locks. Its local-CLI adoption helper must not replace Switcher's stronger native identity and ownership gate.

### Thread full-history retry and force-401

A `thread.type: "continue"` request can contain only messages after `previous_message_id`, omitting `system` and `tools`. A translated upstream cannot reconstruct that history. Opencodex rejects a thread-bearing translated request before dispatch with:

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "message": "message threads are not supported on translated routes",
    "details": { "error_code": "thread_unsupported_request" }
  }
}
```

The status is `400`. The source documents that Claude Code resends the full conversation and disables threads for that model for the rest of the session. Its fixture checks no translated inference occurs before the refusal, token counting refuses the same delta, a full-history replacement can be sent, and native Anthropic passthrough preserves the thread object. Those fixtures do not constitute a live-client retry test.

No deliberate force-401 account-switching mechanism was located in the reviewed paths across these five projects. Opencodex's native branch relays genuine upstream 401s; stored-credential paths have their own refresh/rejection handling. A 401 cannot stand in for the explicit thread-unsupported negotiation and does not prove a client's cached login has changed.

### Source map

| File and lines | Key functions or types | Relevance |
|---|---|---|
| [`src/claude/desktop-first-party.ts:1-43`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/claude/desktop-first-party.ts#L1-L43) | `ClaudeDesktopMode` | First-party Code interception versus whole-app gateway mode |
| [`src/claude/intercept/settings.ts:6-35`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/claude/intercept/settings.ts#L6-L35) | `buildClaudeInterceptEnv` | Code runtime proxy and extra-CA environment |
| [`src/claude/intercept/settings.ts:124-170`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/claude/intercept/settings.ts#L124-L170) | `captureClaudeInterceptSettingsRollback`, `applyClaudeInterceptSettings` | Value-based ownership and field-scoped rollback |
| [`src/claude/intercept/connect-proxy.ts:24-64`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/claude/intercept/connect-proxy.ts#L24-L64) | `ConnectProxyOptions`, `isBrowserConnect` | Tunnel selection is distinct from request account selection |
| [`src/claude/intercept/connect-proxy.ts:153-225`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/claude/intercept/connect-proxy.ts#L153-L225) | `handleConnection` | CONNECT authentication, target policy, pending bytes and bidirectional splice |
| [`src/claude/intercept/listener.ts:31-127`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/claude/intercept/listener.ts#L31-L127) | `rewriteInterceptedRequest`, `relayToUpstream`, `startClaudeInterceptListener` | Only inference and token counting enter the router |
| [`src/claude/intercept/runtime.ts:241-263`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/claude/intercept/runtime.ts#L241-L263) | Picker `selectTunnel` | Browser versus Code trust and interception split |
| [`src/claude/desktop-picker-profile.ts:1-8`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/claude/desktop-picker-profile.ts#L1-L8) | Owned egress profile contract | First-party picker profile is not an inference gateway profile |
| [`src/claude/desktop-3p.ts:42-48`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/claude/desktop-3p.ts#L42-L48) | `Desktop3pConfigMode` | Static list versus hybrid/discovery behavior |
| [`src/claude/desktop-3p.ts:249-317`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/claude/desktop-3p.ts#L249-L317) | `collectDesktop3pModels`, `buildDesktop3pRegistry` | Alias collisions and native model bypass |
| [`src/server/claude-messages.ts:199-261`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/server/claude-messages.ts#L199-L261) | `wantsNativePassthrough` | Caller-native credentials bypass managed account selection |
| [`src/server/claude-messages.ts:545-651`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/server/claude-messages.ts#L545-L651) | `anthropicNativePassthrough` | Native body forwarding, streaming and real status propagation |
| [`src/server/messages-native-oauth.ts:1-24`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/server/messages-native-oauth.ts#L1-L24) and [`77-148`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/server/messages-native-oauth.ts#L77-L148) | `resolveNativeOAuthBinding`, `nativeOAuthBindingIsCurrent`, `restoreOAuthToolNamesInSse` | Rollout-gated native lane, request-time generation checks and bounded tool-name rewrite |
| [`src/server/responses/request-transport.ts:158-218`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/server/responses/request-transport.ts#L158-L218) | `commitResolvedOAuthSelection`, `refreshResolvedOAuthSelection` | Manual selection wins stale asynchronous proposals |
| [`src/oauth/anthropic-routing.ts:1030-1134`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/oauth/anthropic-routing.ts#L1030-L1134) | `resetAnthropicRoutingForManualSelection`, `canRefreshAnthropicPoolAccount`, `anthropicSessionKeyFromParts` | Manual-switch policy, local-CLI ownership and real session IDs |
| [`src/oauth/index.ts:869-874`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/oauth/index.ts#L869-L874) and [`917-1038`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/oauth/index.ts#L917-L1038) | `newerClaudeCredential`, `refreshAnthropicAccountWithLock` | Local token adoption and durable ambiguous-refresh guard |
| [`src/claude/message-threads.ts:1-28`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/claude/message-threads.ts#L1-L28) | `carriesMessageThread`, `messageThreadUnsupportedResponse` | Exact full-history retry response |
| [`src/server/claude-messages.ts:922-932`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/src/server/claude-messages.ts#L922-L932) | Native branch followed by thread refusal | Ordering preserves native threads and blocks incomplete translation |
| [`tests/claude-integration/claude-messages-thread.test.ts:83-171`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/tests/claude-integration/claude-messages-thread.test.ts#L83-L171) | Thread fixtures | Static evidence for no premature inference and native thread preservation |

Switcher candidate: a host-selective CONNECT adapter into the existing native Messages route, with managed-account forwarding chosen explicitly. Keep the optional browser picker out of the initial implementation. Preserve the exact unsupported-thread contract for routes that cannot honor server-held history.

## claude-swap: strongest native refresh and credential ownership reference

### Mechanism and boundaries

Claude-swap changes Claude Code's native credential and `oauthAccount` configuration. It does not switch Claude Desktop's browser sign-in, implement a CONNECT proxy, or select a credential per inference request.

`_perform_switch` checks session-profile drift before activation, resolves live identity before taking mutation locks, then re-reads state while holding the account-store lock, Claude Code's two refresh locks, and its config lock. Target login fields remain account-owned. Only an explicit machine-shared allowlist is composed from the live credential. Presence and absence are authoritative, so a deleted shared field is not resurrected from an old backup.

The native refresh locks use the proper-lockfile directory protocol. The primary `.oauth_refresh.lock` precedes the legacy config-home lock; both have 60-second staleness. The config-file lock has 10-second staleness. The source records these as Claude Code internal contracts, so they remain version-sensitive.

The backup consume gate re-reads the current slot, avoids consuming an unreadable or removed slot's copied grant, adopts a retained successor, and fences writes to the consumed generation. The active usage refresh path follows the native lock protocol through re-read, refresh and persistence. It treats a real usage 401 as a possible stale credential, not account quota exhaustion. A rotated successor must survive even when a later native-store write fails.

Session mode uses separate `CLAUDE_CONFIG_DIR` profiles. Optional history sharing merges and links transcript files. That solves filesystem discoverability and isolated native sessions, not HTTP `thread.continue` reconstruction or Desktop account sign-in.

### Source map

| File and lines | Key functions or types | Relevance |
|---|---|---|
| [`src/claude_swap/switcher.py:6692-6790`](https://github.com/realiti4/claude-swap/blob/3a4e5c14873eb5b32f182d55c68da98ac8c0db45/src/claude_swap/switcher.py#L6692-L6790) | `_perform_switch` | Session drift, provenance and mutation lock order |
| [`src/claude_swap/credentials.py:191-267`](https://github.com/realiti4/claude-swap/blob/3a4e5c14873eb5b32f182d55c68da98ac8c0db45/src/claude_swap/credentials.py#L191-L267) | `SHARED_CREDENTIAL_KEYS`, `merge_shared_credential_fields` | Machine-shared versus account-owned token data |
| [`src/claude_swap/claude_locks.py:1-61`](https://github.com/realiti4/claude-swap/blob/3a4e5c14873eb5b32f182d55c68da98ac8c0db45/src/claude_swap/claude_locks.py#L1-L61) and [`158-187`](https://github.com/realiti4/claude-swap/blob/3a4e5c14873eb5b32f182d55c68da98ac8c0db45/src/claude_swap/claude_locks.py#L158-L187) | `claude_credentials_lock`, `claude_config_lock` | External native refresh coordination |
| [`src/claude_swap/switcher.py:2097-2173`](https://github.com/realiti4/claude-swap/blob/3a4e5c14873eb5b32f182d55c68da98ac8c0db45/src/claude_swap/switcher.py#L2097-L2173) | `_consume_backup_grant_locked` | Slot re-read and retained successor adoption before grant consumption |
| [`src/claude_swap/switcher.py:2257-2289`](https://github.com/realiti4/claude-swap/blob/3a4e5c14873eb5b32f182d55c68da98ac8c0db45/src/claude_swap/switcher.py#L2257-L2289) | Backup grant recovery | Reuse a fresh generation and retain issued successors |
| [`src/claude_swap/switcher.py:4072-4111`](https://github.com/realiti4/claude-swap/blob/3a4e5c14873eb5b32f182d55c68da98ac8c0db45/src/claude_swap/switcher.py#L4072-L4111) and [`4195-4281`](https://github.com/realiti4/claude-swap/blob/3a4e5c14873eb5b32f182d55c68da98ac8c0db45/src/claude_swap/switcher.py#L4195-L4281) | `_fetch_active_usage` | Native ownership, unreadable-store refusal and coordinated refresh |
| [`src/claude_swap/session.py:1-30`](https://github.com/realiti4/claude-swap/blob/3a4e5c14873eb5b32f182d55c68da98ac8c0db45/src/claude_swap/session.py#L1-L30) | Session profile contract | Separate native profiles and opt-in transcript sharing |

Switcher candidate: reuse the existing `internal/claudecode` integration as the native credential owner. A new Desktop transport should request a verified credential snapshot from that owner rather than introducing another refresh loop or reading arbitrary native stores itself. The existing [claude-swap analysis](claude-swap-analysis.md) contains the broader Switcher port history.

## Proposed Switcher implementation

The following records the original design candidate. Its initial implementation
is now in `internal/desktoprelay`; the table below distinguishes implemented
behavior from reference options that were not adopted. It remains fixture-tested,
with live Desktop validation deferred until current tasks finish.

### Implemented comparison

| Concern | Reference logic | Switcher implementation and difference |
|---|---|---|
| First-party ingress | OpenCodex's host-selective CONNECT and extra-CA environment | `ingress.go`, `network.go`, and `ca.go` use authenticated loopback CONNECT, an Anthropic-only CA, and opaque tunnels for every other destination |
| Account selection | OpenCodex provider-wide selector and CLIProxyAPI affinity | `control.go` and `conversation.go` use explicit session or conversation selections; no model-wide selection, inferred prompt cohort, or automatic peer inheritance |
| Native payloads | OpenCodex and CLIProxyAPI normalize some fields or translate selected routes | `forward.go` retains raw body/query bytes, model, signatures, deferred tools, unknown fields, and complete native response/SSE content |
| Refresh ownership | CLIProxyAPI selected-generation recovery and claude-swap native ownership | `proxy/desktop_credentials.go` delegates to Switcher's serialized credential owner; the relay stores no OAuth tokens and never activates the native login |
| Thread continuation | OpenCodex refuses translated thread deltas before dispatch | Adopted narrowly: only a `continue` thread whose session changed account since its last thread request gets the same 400; everything else is passed through |
| Setup and trust | CC Switch/CCR owned 3P profiles and OpenCodex first-party settings writer | One-click first-party Configure backs up settings and merges only owned proxy/CA fields, with durable recovery and selective restore. Manual profiles remain Advanced; no OS trust or browser-picker interception, and Desktop restart requires a separate explicit confirmation |
| Stream ownership | CLIProxyAPI pre-output recovery and OpenCodex request snapshots | One same-account 401 recovery before output; selection changes apply to later requests, and running streams keep their account |
| Control authority | Local proxy tokens and manager authentication in the references | Independent local management authority is separate from scope admission; cookie controls require CSRF, and LAN requests cannot change relay selections |

Implementation code is independently written Go. No source code, comments,
tests, or line-by-line translations were copied from the Desktop reference
implementations. The existing claude-swap native port retains its attribution
in `THIRD_PARTY_NOTICES.md`.

The [module contract](desktop-relay-module.md) records implementation limits,
wire policies, persistence, and the reviewed regressions. The
[usage and live validation guide](desktop-relay-usage.md) records the remaining
real-client acceptance gates.

### First-party CONNECT request path

```text
Desktop embedded Code runtime
  -> CONNECT api.anthropic.com:443
  -> Switcher TLS listener
  -> POST /v1/messages or /v1/messages/count_tokens
  -> request account selection and credential-generation snapshot
  -> existing Claude native-protocol forwarding
  -> native Anthropic SSE or native error response
```

Other destinations use opaque tunnels. Other intercepted API paths retain a separate caller-native relay contract. The Desktop browser's Chat identity and the inference account selected by Switcher are different state values.

Choose the inference account after parsing each HTTP request, including requests on a reused tunnel. Snapshot the active selection revision and account credential generation. Refresh through the credential's owner, then re-check the snapshot before sending. A manual switch that completes during an asynchronous wait must win over a stale selection proposal. Attribute quota and errors to the account actually sent, not the account active when the response arrives.

Existing Switcher code already contains much of this foundation:

| Current local file and lines | Existing mechanism | Desktop integration use |
|---|---|---|
| `internal/proxy/proxy.go:952-1073` | Buffered request, bounded attempts, real 401 refresh, recognized exhaustion failover | Keep request replay and account policy in one forwarding owner |
| `internal/proxy/proxy.go:1120-1130` | `RefreshAfter401` receives the rejected access generation | Delegate native refresh rather than adding a Desktop grant consumer |
| `internal/proxy/routing.go:5-47` | Selection and credential epochs; persisted exhaustion decision | Fence late upstream feedback and route changes |
| `internal/proxy/native.go:14-60` | `ActivateForClient` separates explicit native activation from automatic proxy failover | Keep native login and proxy selection visible separately |
| `internal/provider/claude/claude.go:292-319` | `ApplyAuth` replaces bearer, removes client API key and merges feature betas | Native Messages forwarding with the chosen OAuth account |

These line references describe the current uncommitted working tree inspected for this task. They are not upstream commit permalinks.

### Session affinity, threads and streaming

- Preserve Switcher's active-account-first policy. Start with explicit session/thread IDs and avoid round-robin or inferred history matching. Use affinity only where server-held state requires it, with a documented manual-switch rule.
- A stateless full-history Messages request can be replayed before output under the normal account policy. A `thread.continue` delta depends on state held upstream. Do not assume its `previous_message_id` can move to another account, model or provider.
- For a native thread, keep its account and protocol scope or negotiate a full-history retry before sending to a route that cannot honor the state. Opencodex's exact `400` error code is the best reference, but its applicability to a native cross-account migration still needs a client fixture and live-client validation.
- Never silently delete `thread`, `system`, `tools`, thinking signatures or opaque continuation fields to make a request fit another protocol. Require full history or an explicit compatibility policy.
- Detect bootstrap errors before exposing response bytes. After downstream headers or SSE bytes are committed, finish or terminate that stream. An automatic replacement stream would duplicate output or tool calls. Client cancellation must cancel the active upstream attempt.

### Third-party profiles

Implement a separate owned gateway profile with its own UUID, local admission credential and model-list contract. Base apply/restore on CC Switch's field ownership and multi-file intent. Preserve foreign entries, unrelated settings, and the previous selection where Switcher can verify ownership. Expose native Anthropic Messages first. Add protocol translation and display aliases only with explicit model-capability data and the thread fallback contract.

Represent gateway profile state, first-party transport state, native Code login, selected inference account, and Desktop browser sign-in separately. The references show why one generic Active badge would be misleading.

### 401 policy and refresh ownership

Use a genuine upstream authentication rejection for bounded same-account recovery. Re-read the rejected generation before consuming a refresh grant. Adopt a successor another authorized owner already persisted. Preserve ambiguous-consumption intent and issued successors across persistence failure. Distinguish native-owned, Switcher-owned and externally-owned credentials.

Do not make intentional force-401 an account-selection API. A first-party client may refresh its original native login, leaving Switcher's selection unaffected. If the proxy replaced the bearer, the upstream 401 belongs to that selected proxy credential instead. Thread fallback uses the dedicated `400` code, and transient transport failures do not justify revoking or reauthenticating an account.

## Exact license findings and reuse constraints

All five pinned root licenses are SPDX `MIT`, identified from the standard MIT license text. None of the five root licenses at these pins is GPL or AGPL.

| Project | SPDX | Copyright as written in pinned root license | Source |
|---|---|---|---|
| CC Switch | `MIT` | `Copyright (c) 2025 Jason Young` | [`LICENSE:1-21`](https://github.com/farion1231/cc-switch/blob/b9e9620265a76b2c074a83644ae1ad5ec57c3c98/LICENSE#L1-L21) |
| CLIProxyAPI | `MIT` | `Copyright (c) 2025-2005.9 Luis Pater`; `Copyright (c) 2025.9-present Router-For.ME` | [`LICENSE:1-22`](https://github.com/router-for-me/CLIProxyAPI/blob/6fecc6e5567912661654a4eaf9b8f5436facd1c2/LICENSE#L1-L22) |
| CCR | `MIT` | `Copyright (c) 2025 musistudio` | [`LICENSE:1-21`](https://github.com/musistudio/claude-code-router/blob/f2e01bfe0c01e0c7ea7a37077747473f69869048/LICENSE#L1-L21) |
| opencodex | `MIT` | `Copyright (c) 2026 opencodex contributors` | [`LICENSE:1-21`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/LICENSE#L1-L21) |
| claude-swap | `MIT` | `Copyright (c) 2026 Onur Cetinkol` | [`LICENSE:1-21`](https://github.com/realiti4/claude-swap/blob/3a4e5c14873eb5b32f182d55c68da98ac8c0db45/LICENSE#L1-L21) |

The unusual CLIProxyAPI date string above is quoted exactly. If code is copied or substantially adapted, retain the original copyright and full permission notice in Switcher's attribution. The source clones already retain those licenses; this research adds no implementation code or new attribution to `THIRD_PARTY_NOTICES.md`.

License boundaries worth carrying forward:

- CCR's `packages/cli/LICENSE` repeats MIT. The external gateway dependency has its own source and license boundary; the wrapper repository's MIT license cannot be assumed to license that dependency's implementation. This task did not install or analyze its package contents.
- opencodex includes a separate [`tests/fixtures/minisign/LICENSE`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/tests/fixtures/minisign/LICENSE#L1-L60) with additional author notices and text labeled as an original ISC license. Preserve its exact bundled notice if that fixture is ever reused; it is outside the Desktop implementation candidate.
- opencodex research notes discuss an external AGPL reference, kiro-lb, and explicitly state that behavior was studied without code reuse. See [`devlog/_plan/260829_kiro_quota_pool/002_research_kirolb_headtohead.md:1-4`](https://github.com/lidge-jun/opencodex/blob/ef0297f86c4540c7d757c8595170d66f9c584aec/devlog/_plan/260829_kiro_quota_pool/002_research_kirolb_headtohead.md#L1-L4). This does not change opencodex's root SPDX identifier.
- For any separately licensed copyleft source, use mechanism concepts and an independent implementation. Do not copy its code, comments, tests, or a line-by-line translation into Switcher. Root MIT findings are not a blanket license audit of every dependency or bundled asset.

## Work performed

Created the five durable pinned clones and their reference README, inspected source and license text, and wrote this source map. No repository install, build, tests or runtime were executed. No user credential store, Keychain, live Desktop process, real inference provider, or local Switcher endpoint was accessed. No commit was created. Switcher's existing implementation and review files were only read; this source map is the sole new file in its repository.
