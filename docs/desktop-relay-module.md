# Desktop relay module

`internal/desktoprelay` is a local HTTPS proxy that lets Claude Desktop's Code
tab use a selected Switcher account per conversation. It replaces only the
bearer credential. Everything else is passed through unchanged, and every error
or rate limit the client sees comes from Anthropic, except the few local cases
listed under [Local responses](#local-responses) and the move of a conversation
whose account ran out of usage ([Out of usage](#out-of-usage)).

## How a request flows

1. Desktop's Code worker connects with `HTTPS_PROXY` (Basic auth with a scope
   ID and secret) and trusts the relay CA through `NODE_EXTRA_CA_CERTS`.
2. `CONNECT api.anthropic.com:443` is terminated with a leaf certificate from
   the relay CA. Every other destination is tunneled unchanged, and plain
   proxy requests such as `POST http://127.0.0.1:3773/` (T3 Code's MCP server)
   are forwarded unchanged with streaming and upgrades, without a proxy login.
3. For `POST /v1/messages` and `/v1/messages/count_tokens`, the relay reads the
   session UUID from `X-Claude-Code-Session-Id`, or from `session_id` in the
   JSON string `metadata.user_id`. Unreadable or missing identity means the
   caller's own credential is used. Requests are never rejected for it.
4. The session's account is looked up in memory. A conversation selection
   applies to every request session that native Desktop metadata links to that
   conversation, including sessions started later.
5. With a selected account, the request is sent the way Claude Code logged in
   to that account would send it: its bearer token, the `oauth-2025-04-20`
   beta if missing, and `metadata.user_id`'s `account_uuid` set to that
   account. `X-Api-Key` is removed. All other bytes and headers are unchanged.
6. The response is streamed back as received. A selected-account 401 gets one
   token refresh and one retry. If the refresh fails, the relay answers 503
   `credential_unavailable` (or 404 `account_not_found`) instead of passing a
   401 that Claude Code would blame on its own login.
7. A subagent session without its own selection follows its parent session.
8. Message threads (`thread: {type: "continue"}`) live on the account that
   created them. If a session's account changed since its last thread request,
   the continuation gets 400 `thread_unsupported_request`; Claude Code answers
   that by resending the full conversation without a thread. Nothing else is
   refused.

There is no local concurrency limit, connection limit, request size limit or
response timeout. Anthropic does its own rate limiting.

## Conversation linking

`Config.Conversations` resolves request session UUIDs to Desktop conversation
UUIDs from `claude-code-sessions/**/local_*.json` (`cliSessionId` ->
`sessionId`). A session claimed by two conversations is left unlinked. Once a
session is linked, the link is stored with the session and never changes.

On the request path the relay first asks the resolver's in-memory snapshot
(`CachedConversationResolver`). Only a brand-new session in a scope with a
selected conversation waits for metadata, retrying for at most two seconds
because Desktop may write it just after the first request. Other unlinked
sessions are linked in the background at most every fifteen seconds, so one
conversation shows up as one entry. Lookup failures are ignored.

## Local responses

| Case | Response |
|---|---|
| Missing or wrong proxy credentials | 407 on CONNECT |
| Thread continued right after a switch | 400 `thread_unsupported_request` |
| Non-Anthropic destination cannot be dialed | 502 on CONNECT |
| Selected account's credential cannot be prepared | 503 `credential_unavailable`, or 404 `account_not_found` |
| api.anthropic.com unreachable | 502 |
| Relay profile revoked while a tunnel is open | 403 |

A credential failure never falls back to the caller's account.

## Out of usage

With **Switch Claude automatically** on (the default), a conversation whose
account runs out of usage moves to the Switcher account with the most usage
left. `send` handles it before any byte reaches Desktop:

1. Anthropic answers a Messages request with 429. The relay reads at most
   64 KiB of the refusal; a longer body streams to Desktop unchanged.
2. The refusal counts as out of usage when Anthropic's
   `anthropic-ratelimit-unified-5h-status` or `-7d-status` header says
   `rejected`. For a Switcher account, `proxy.DesktopCredentialSource.OutOfUsage`
   also checks its usage the way the proxy does (`ParseRateLimit`) and parks
   it until its reset. Burst limits and other 429s pass through as before.
3. `Takeover` names the Claude account with the most session and weekly usage
   left. With none, or with the setting off, the refusal passes through.
4. The conversation is bound to that account (`setConversation`), or the
   session alone when its conversation is unknown, so later requests and
   subagents follow and the Settings list shows the new account.
5. The request is sent again with the new account's credential from its
   original bytes. A continued thread lives on the old account, so the relay
   answers 400 `thread_unsupported_request` instead and Claude Code resends the
   full conversation, which then goes to the new account.

This applies to Desktop's own login too, when Anthropic's headers say it is
out, and it never changes Desktop's own sign-in, another conversation, Claude
Code's native login or the proxy's selection. Requests forwarded to a
connected Switcher host keep the host's handling.

## Credentials

`CredentialSource` is implemented by `proxy.DesktopCredentialSource`.
`Prepare` returns the stored access token while it is valid, without locks,
native Keychain sync or network calls, also for the account Claude Code itself
is logged in to. Only an expired token, a rejected token or a pending recovery
takes the slow path that syncs native credentials and refreshes; a token
Claude Code rotated is adopted there without a new grant. The relay never
stores OAuth tokens.

## Management API

`Start`, `Resume`, `Stop`, `Close`, `Status`, `CreateScope`, `DeleteScope`,
`Scopes`, `Sessions`, `Bind`/`Unbind` (one request session, revision checked),
`BindConversation`/`UnbindConversation` (a whole conversation),
`ConversationBindings`, `AssociateConversations`, and setup:
`Configure`, `SetupStatus`, `RestoreSetup`.

Selections are saved immediately. Request observations (last seen, counts,
model, last upstream status) are saved in the background a few seconds later
and on shutdown, outside the request lock; a failed save is logged and never
affects requests. Idle sessions are forgotten after 14 days and at most 2000
are kept.

## Storage

`state.json` in the private data directory holds the CA, scope secrets,
sessions, conversation selections and the Desktop setup journal. `ca.pem` is
the exported CA certificate. Loading ignores unknown fields and drops records
whose scope no longer exists.

## Setup

`Configure` merges `HTTPS_PROXY` and `NODE_EXTRA_CA_CERTS` into the Claude
settings file's `env`, adds `localhost,127.0.0.1,::1` to `NO_PROXY` (keeping
existing entries; Restore puts the original back), keeps a private backup, and records the original values
so `RestoreSetup` can put them back without touching unrelated edits. Neither
restarts any application.

## Tests

```sh
go test ./internal/desktoprelay ./internal/sessionmeta ./internal/server ./internal/proxy
```

`relay_test.go` covers pass-through of requests the relay does not understand,
credential replacement with the OAuth beta and `account_uuid`, 401 refresh and
retry, credential failures, many concurrent requests, conversation selection
across rotated sessions with metadata failures, subagents, threads after a
switch, and opaque tunnels. The web UI is covered by `web/desktop-relay_test.mjs`.
