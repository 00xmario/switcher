# Claude Desktop account switching

Switcher lets you pick an account per Claude Desktop Code conversation. A local
relay swaps the OAuth token on that conversation's requests; Desktop itself
stays signed in to its own account. Real Desktop traffic has been observed
through the relay. Complete billing attribution and task continuity across a
switch have not been verified end to end. The relay is a network intermediary,
so it can add latency, but it never fails a request because of its own
bookkeeping.

The native **Use in Claude Code** action on an account card is separate: it
switches Claude Code's machine login. The relay only changes the credential of
conversations you pick it for.

## Set up once

1. Open Switcher on this Mac (localhost) and go to **Settings → Claude Desktop**.
2. Click **Connect Claude Desktop**. This starts the relay, creates its profile,
   backs up `~/.claude/settings.json` (or the file in `CLAUDE_CONFIG_DIR`) and
   adds only `HTTPS_PROXY` and `NODE_EXTRA_CA_CERTS` to its `env`. Other
   settings stay as they are. No system trust root or Keychain item is added.
3. When your current work is done, click **Restart Claude Desktop** and confirm.
   Switcher never restarts Desktop on its own.

The relay listens on `127.0.0.1:8789` (`--desktop-relay-port` changes it). The
shared settings file also affects new terminal `claude` sessions, which then go
through the relay with their own credentials unless you pick an account for them.

## Switch a conversation

Per-conversation routing is off by default. **Settings → Claude Desktop →
Per-conversation accounts** lists recent conversations with their saved title
and project. Pick an account in a row's switcher; **Desktop login** (the
default) means the account Desktop is signed in to.

- New messages in that conversation use the picked account, including sessions
  Desktop starts for it later and its subagents.
- A reply that is already running finishes on the account it started with.
- No restart is needed.
- "last reply · …" shows which credential answered the latest message.

A conversation is linked to its request sessions through Desktop's own
`local_*.json` metadata (`cliSessionId` → `sessionId`). Titles never link
sessions, and message transcripts are never read.

## What the relay changes on a request

Only `POST /v1/messages` and `/v1/messages/count_tokens` of a picked
conversation are changed, and only so they look like Claude Code logged in to
the picked account: its bearer token, the `oauth-2025-04-20` beta if missing,
and `metadata.user_id`'s `account_uuid`. `X-Api-Key` is removed. Everything
else, including model, tools, thinking and unknown fields, is passed through.
Other traffic keeps the caller's credentials, and other hosts are tunneled
unchanged.

Errors and rate limits are Anthropic's own, with three exceptions:

- If the picked account's token is rejected and cannot be refreshed, the relay
  answers 503 `credential_unavailable` so Claude Code does not blame its own
  login. Re-login that account in Switcher.
- A removed account answers 404 `account_not_found`. The relay never falls back
  to another account.
- A message thread continued right after a switch belongs to the previous
  account. The relay answers 400 `thread_unsupported_request`, and Claude Code
  resends the full conversation without a thread.

## Disconnect

**Settings → Claude Desktop → Disconnect** restores the previous `HTTPS_PROXY`
and `NODE_EXTRA_CA_CERTS` values and keeps other edits. Restart Desktop
afterwards. **Advanced** shows the relay status, lets you stop it once Desktop
is disconnected, and manages manual profiles for other apps.

## Still worth verifying live

On a disposable conversation, not active work:

| Check | Pass condition |
|---|---|
| Attribution | After switching A → B, B's usage moves and A's does not |
| Continuity | A tool called under A completes and the conversation continues on B |
| Subagents and threads | A subagent running across a switch recovers after the thread retry |
| Rotation | A token Claude Code refreshed is picked up without a new grant |

Account-bound signed thinking (for example Sonnet 5.5) can be dropped by
Anthropic across accounts even when the request succeeds.

## References

Source clones are kept beside Switcher in `switcher-references/`. The
[source map](desktop-reference-map.md) pins commits and compares CC Switch,
CLIProxyAPI, Claude Code Router, OpenCodex and claude-swap. The
[module](desktop-relay-module.md) describes the implementation.
