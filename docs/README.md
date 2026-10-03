# Project documentation

## Claude Desktop implementation references

Read [Desktop reference map](desktop-reference-map.md) before changing Desktop
account routing. It records the source files, functions, licenses, comparisons,
and pinned commits used from these projects:

- CC Switch: `farion1231/cc-switch`
- CLIProxyAPI: `router-for-me/CLIProxyAPI`
- Claude Code Router: `musistudio/claude-code-router`
- OpenCodex: `lidge-jun/opencodex`
- claude-swap: `realiti4/claude-swap`

Full source clones are retained at
`/Users/mariomakdis/Documents/Makdis/Projects/switcher-references/`, beside this
repository. These local paths are references, not build or runtime dependencies.
On another machine, use the map's GitHub origins and exact commit pins.

The research did not establish that Claude Desktop's Code tab reliably follows
standalone CLI credential swaps. That is why Desktop uses a separate proxy-based
mechanism. Do not confuse native CLI login selection, Desktop sign-in, inference
credential selection, or session-index synchronization.

## What is implemented and what is verified

The local authenticated relay routes the Code tab's requests through Switcher.
For selected conversations, it substitutes the selected account's credential
while preserving native Anthropic Messages bodies, tools, signatures, and
streaming. Desktop sign-in and usage/control-plane requests keep their original
credentials. Real Desktop traffic through the relay has been observed.

This is a network intermediary: it can affect request availability, latency,
and retries. It must not be described as having no effect on Anthropic requests.
Successful relay forwarding is not proof of independent provider-side billing.
Complete account-B billing, task continuity, and bundled-client compatibility
have not been conclusively verified together.

Version 0.5.10 caused local conversation-association 503s on ordinary traffic.
Version 0.5.12 removes metadata-I/O dependency from caller and exact-session
traffic when there is no active conversation-group selection in that scope.
Cached discovery is nonblocking, and repeated unknown-ID lookups use a bounded
negative cache. Group-selected requests still require appropriate association
proof before substituting credentials; they never silently fall back to a
different account. Keep regressions for this distinction when changing routing.

## Guides

- [Setup, usage, and live validation](desktop-relay-usage.md)
- [Relay module and wire contract](desktop-relay-module.md)
- [Conversation identity and upstream response evidence](desktop-conversation-identity.md)
- [Initial implementation plan and subsequent setup changes](desktop-relay-plan.md)
- [claude-swap native credential integration analysis](claude-swap-analysis.md)

Verification uses temporary profiles, synthetic credentials, mocked upstreams,
and ephemeral listeners. Never treat those results as live Desktop billing
verification or restart active user sessions as part of routine tests.
