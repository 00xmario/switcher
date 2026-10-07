# Share between Macs

One Mac (the host) keeps your accounts. Your other Macs (clients) use them
through the host's Switcher. Accounts, OAuth tokens and refreshes stay on the
host, and every request to Claude, Codex and the other providers leaves from
the host. To Anthropic and OpenAI it looks like one machine using the account.

Keeping tokens on one machine is also what keeps the accounts healthy: OAuth
refresh tokens rotate, so two Switchers refreshing the same account would sign
each other out.

## Set up

1. On the host: **Settings → Share between Macs → Share this Switcher**, then
   **Pair a Mac**. A code such as `K7QF-M2XP` is valid for ten minutes.
2. On the client: the same card lists Switchers found on your network. Click
   **Connect**, enter the code and connect. Away from home, enter the host's
   Tailscale name or address instead.

macOS may ask once whether Switcher may accept incoming connections on the
host; allow it.

## What follows the host

- Account cards, usage and the menu bar show the host's accounts. Switching an
  account on the client switches it on the host.
- Codex CLI and every provider proxy on `127.0.0.1:8787`, and the T3 Code hub.
  Their configuration does not change.
- Claude Code and Claude Desktop, once **Connect Claude Desktop** is done on the
  client: their Claude requests go to the host, which uses its selected Claude
  account (or the account picked for a conversation). Claude Code on the client
  still needs to be logged in to some account to start.

The client's own settings, CLI setup and usage history stay local. While
connected, the client does not refresh or poll its own accounts. **Disconnect**
returns the client to its own accounts.

## Network and security

- The host listens on port 8788 with its own certificate. The client pins that
  certificate during pairing, so it reaches the host by its Bonjour name, LAN
  address or Tailscale address without reconfiguration.
- The pairing code is bound to the host certificate the client sees, so a
  device in the middle cannot pair with its own certificate. Five wrong codes
  invalidate a code.
- Only paired Macs are served, with their own revocable token, and only for
  account views and actions, provider proxies, the hub and Claude inference.
  Tokens are never sent to clients.
- A Mac cannot share and use another Switcher at the same time.
- Over the internet, use Tailscale rather than opening port 8788 on your router.

## Implementation

`internal/remote` holds the host listener, pairing, device tokens, Bonjour
(via macOS `dns-sd`) and the client's forwarding. The client's server forwards
`/api/accounts/…`, `/api/providers/…`, `/api/usage/refresh`, provider paths and
`/v0/management/…`, merges `/api/state` with its local fields, and gives the
Desktop relay a remote inference hook. The host serves `/remote/anthropic/…`
through `desktoprelay.Manager.ServeAccount` and adds its own management key to
hub requests.
