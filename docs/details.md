# Switcher in detail

The [README](../README.md) is the overview. This page keeps the details: every
feature, the provider matrix, the exact switching rules and the security model.

## Why Switcher exists

Tools that juggle multiple paid subscriptions are usually built to serve many
users from many accounts at the same time. That is a different problem from
yours, and the features they pile on exist for it:

- **Rotation pools** exist to saturate a set of subscriptions: spread every
  request across all accounts so the aggregate never trips one account's
  rate limit. That matters when one account cannot absorb the load, which is
  a team or reseller problem. For one person coding, it almost never is, and
  the cost is real: you can no longer tell which subscription is being
  billed, and the system makes routing decisions you never asked for.
- **Cooldown schedulers** are circuit breakers for big credential fleets.
  When one account out of many fails, bench it so the rest keep flowing.
  With a small fleet this backfires: benching half of your accounts for a
  minute over a two second network blip turns a hiccup into a lockout.

Switcher answers two questions instead:

- **Which account am I using right now?**
- **Switch me to another one** (yourself, with one click, or automatically
  when the active one is out of usage).

Each provider's proxied traffic uses its selected account. That choice changes
when you switch or the active account reports a known exhaustion signal. Claude
Code's native selection is shown separately from the proxy selection.

Protocol translation (letting a client speak one wire format to an upstream
that speaks another) is deliberately left open. It is genuinely useful and
may be added later.

Everything else is deliberately missing. Switcher adds no shared proxy API
key, model aliases, round-robin, or plugins. Provider credentials are still
required; OpenCode Go uses its own API key.

## Features

- **Manual switching, only from you**: switching never happens on its own
  as long as the active account works. Use the web UI or the menu bar
  dropdown.
- **Automatic failover on exhaustion**: when the active account reports it
  is out of usage (HTTP 429 `usage_limit_reached`), Switcher marks it with
  the upstream reset time and, if another account is usable, retries your
  in-flight request on it transparently. Paid accounts come first. A Free
  account comes last, after a banked reset when auto-use is on, because
  Free tiers lack the paid models. Grok's session service has no
  verified account-exhaustion error yet, so its 429s pass through without
  switching accounts.
- **No switch when there is nowhere to go**: if every account is out of
  usage, Switcher does not rotate; it passes the upstream error through.
- **Per-account usage windows**: session / weekly / monthly limits with
  reset countdowns, live in the web UI and the menu bar dropdown. The menu
  shows a compact remaining-quota bar beside each percentage by default;
  turn it off in Settings if you prefer text only. Hover a future reset
  countdown for the exact provider-reported time in your local time zone.
- **Account health**: see when quota data was last current, when it has gone
  stale for five minutes, or when a rejected refresh credential needs a
  relogin. Recheck one account without waiting on the page or switching it.
  A quota endpoint that returns HTTP 429 pauses automatic checks for five
  minutes; Recheck can try sooner. A rotated native token clears the previous
  credential's health and polling backoff. Expired usage windows are hidden
  individually during an outage; remaining valid windows stay visible.
  Usage polling failures do not change request routing.
- **Current Codex plan**: successful quota checks update the saved plan from
  the same response's `plan_type`, including downgrades to Free. Both the menu
  and web cards display Free instead of retaining the paid tier from login.
  Free-tier quota windows remain visible when OpenAI reports them; a monthly
  allowance does not imply a paid subscription. Missing or failed plan data
  does not guess a replacement tier or model entitlement.
- **Reset alerts**: opt in under Settings → Menu bar to receive macOS
  notifications at provider-reported usage-window reset times. Switcher
  remembers pending alerts across menu app restarts and catches up within a
  day after sleep. The menu shows macOS permission status and offers a test
  alert. Delivery requires the menu bar app to be running and macOS to allow
  notifications. Even unused windows with a reported reset time can alert.
  Nothing is pre-queued in macOS; an alert marks the reported time, not a
  verified return of quota. Focus can route a banner to Notification Center
  instead. Check permission from Terminal with
  `/Applications/Switcher.app/Contents/MacOS/Switcher --notification-status`.
  To send one test alert, use `--test-notification` instead.
- **Cost and tokens**: a Usage tab that reads the provider CLIs' own
  session logs (like ccusage does) and prices them with LiteLLM rates:
  daily cost chart, per-provider and per-model breakdowns, cache savings.
- **Claude keychain import**: Switcher can copy Claude Code's stored login
  from the macOS keychain, or the native credentials file on Linux. The full
  account-scoped wrapper and identity are preserved. Switcher synchronizes
  the current native generation instead of independently refreshing a copy
  of the active Claude Code login. If that native generation expires while
  Code is idle, Switcher refreshes the locked live credential, saves the
  successor back to the native stores, then fetches current usage.
- **Native Claude Code switching**: Claude account actions in the web and
  menu bar apps write Code's native credential store and `oauthAccount`
  config section, then select the same Switcher proxy account. Shared MCP
  OAuth fields, project configuration, and transcripts stay in place.
  The operation uses Claude Code's own refresh/config locks, creates private
  backups, verifies writes, and rolls back partial failures. The native
  active badge is separate from automatic proxy routing. Running macOS
  sessions may take about 30 seconds to notice a switch; reopen Code for
  immediate application. This changes Claude Code, not Claude Desktop's
  signed-in account. Do not `/logout` first because it can revoke the old
  saved refresh token. Machine-level switching and import require a local
  connection. Same-store `CLAUDE_CONFIG_DIR` profiles are supported; split secure
  storage and credential-overriding environment variables block the operation
  with an explanation. See [the source analysis and comparison](claude-swap-analysis.md).
- **Share between Macs**: let your other Macs use one Mac's accounts. Pair once
  with a code (Switchers on your network are found automatically). The optional
  **Away from home** add-on, downloaded only when turned on, connects your Macs
  over Tailscale from anywhere. Accounts, tokens and every provider request stay on
  that Mac; the other Macs' account views, Codex, T3 Code hub, Claude Code and
  Claude Desktop follow it. See [sharing](sharing.md).
- **Command line**: switch accounts, sign in, share between Macs, set up
  Claude Desktop and Codex, and change settings from a terminal, over SSH or
  for an agent, with `--json` output and stable exit codes. See
  [the command line](cli.md).
- **Claude Desktop account switching (optional, off by default)**: in
  Settings → Claude Desktop you can pick a Switcher account for individual
  Desktop Code conversations. Connect once (**Connect Claude Desktop**, then
  restart Desktop when your work is done); after that, switching is one click
  and never needs a restart. Conversations you don't pick keep Desktop's login. New
  messages use the picked account; replies already running finish where they
  started. The Switcher mark, the default option, keeps the account Desktop is
  signed in to. A local
  relay swaps only the OAuth token (plus the matching OAuth beta and
  `account_uuid`); errors and rate limits are Anthropic's own. **Disconnect**
  restores your previous settings. The relay port is `8789`, configurable with
  `--desktop-relay-port`. See [setup](desktop-relay-usage.md), the
  [module](desktop-relay-module.md) and the
  [source comparison](desktop-reference-map.md).
- **Claude Desktop session sync**: the small two-arrow sync icon after the Claude name in the
  menu bar and web app makes new Claude Code chats discoverable across your
  Desktop account indexes. Clicking it first asks for confirmation, with
  Cancel selected by default, because syncing closes and reopens Claude
  Desktop even when it is running in the background. It backs up every
  `local_*.json` to a unique folder under
  `~/.claude/desktop-session-sync-backups/`, copies only missing pointers,
  then reopens Desktop. Existing pointers, archive/deletion markers, and
  the actual transcripts under `~/.claude/projects/` are left alone.
  The Go implementation follows the local `claude-sync` script's copy rules,
  discovers account/workspace indexes automatically, and uses Switcher
  account names when the account UUID matches. Empty indexes participate.
  Both controls use the same loopback-only backend operation and report
  added-pointer counts. On failure, Details includes the backup location
  if one was created. Quit or backup failures stop the merge.
- **Claude plan labels**: profile metadata distinguishes Max 5x, Max 20x,
  and Pro in both UIs. Existing accounts receive plan metadata during usage
  checks, at most hourly after success. Recheck retries a failed lookup.
  A generic Max flag is displayed as Max rather than guessing a multiplier.
- **Relogin per account**: sign in again in one click; the tokens overwrite
  the matching account in place, keeping its id, active slot, and history.
  Claude identity includes its organization. Older Claude records must first
  verify that identity from their saved token or matching native login; a new
  organization is added separately rather than overwriting an unknown one.
- **Banked resets**: Codex usage-limit resets can be spent from the UI. A
  compact count appears beside the account plan in the web UI and menu bar.
  **Auto-use** can spend one automatically when an account runs out and
  failover finds no other paid account: before moving to a Free account,
  or when no account is left at all. It is off by
  default: enable it under Settings → Switching, or override it per
  account from the account's ⋯ menu (Global / On / Off). At most one credit
  is spent per request, and an account is not auto-reset again for five
  minutes, so a client retry loop cannot drain every banked reset.
  Redeeming a reset acknowledges success immediately, removes the spent
  credit, and refreshes quota in the background. The web card gets a brief
  success sweep and animates its bar to the balance reported by the provider.
  Reduced-motion settings are respected. Automatic resets use the same
  feedback; old resets are not replayed when opening the page.
  Manual requests are tied to the displayed credit ID, so a delayed response
  or repeated click cannot silently spend the next credit. Quota refreshes
  are bounded, and late responses cannot overwrite a newer balance.
  Expired access tokens are refreshed before credit operations, with at most
  one authentication retry and the original credit ID kept throughout.
- **Account actions**: Recheck and account switching stay on the account card.
  Use the ⋯ menu for a banked reset, relogin, or removal.
- **Compact account view**: a single switch under Settings → Display puts
  accounts for the same provider into responsive side-by-side cards. Each
  card still shows its plan, every available quota window, the reported
  reset date and countdown, health, and account actions. The expanded view
  remains the default. With compact view on, all tabs keep the same width;
  the active card has a quiet border and an inline Active badge beside its
  account name. Both layouts avoid a duplicate disabled Active button.
- **Appearance themes**: choose Graphite, Tide, or Ember under Settings →
  Appearance. Every preset supports light, dark, and system mode. Create up
  to eight named custom themes with a live preview of background, accent,
  corner style, action buttons, and per-provider usage-bar colors. Save or
  cancel a preview, edit or delete custom themes, or return to a preset.
  Appearance is saved in this browser, like the light/dark preference, and
  updates other tabs on the same origin. Text contrast adjusts automatically.
  Bar colors are shared by compact cards, expanded cards, and usage charts.
- **T3 Code hub**: speaks CLIProxyAPI's management protocol, so T3 Code
  shows every account's quota from one place.
- **Menu bar app**: the macOS app supervises the server, shows every
  account's usage and plan, switches, refreshes, and updates itself. The
  Copilot mark follows the menu's light or dark appearance. It retries an
  unexpectedly exited server that it started at most three times; an existing
  external server is never adopted or terminated. **Check for updates** fetches
  release metadata only. Installation is a separate explicit action that
  verifies the asset checksum and version before atomic replacement.
- **CLI setup status**: Settings checks the inspected Codex user config for
  Switcher's URL and lists stored Switcher accounts separately from native
  CLI configuration. Configure Codex can update that file; no native CLI
  request is tested by this check. The optional **Test Switcher route** action
  makes one bounded Codex request using quota from the explicitly active
  Switcher account; it does not run or verify the native Codex CLI. Other
  clients are informational until their routing is validated.
- **Single Go binary**: the frontend is embedded; there is no Node build.
  Dark and light themes, drag-to-reorder providers, hide providers you
  do not use.

## Providers and native CLI readiness

Adding a provider login to Switcher, testing its HTTP forwarding with a fake
upstream, and configuring that provider's own CLI are three different things.

| Switcher provider | Account login | HTTP forwarding evidence | Native CLI setup |
|---|---|---|---|
| Codex | Browser OAuth | Fixed Codex route-probe fixture, not a full native CLI test | Codex user-file configuration; native request untested |
| Claude | Browser OAuth or native Claude Code login import | Messages, token counting, and SSE fixture | Native credential/config switching with ownership and rollback fixtures; setup checks do not run a real native request |
| OpenCode Go | Provider API key | Responses, chat, messages, and SSE fixture | Not enabled; v2 account, model, and transport unverified |
| Grok Build | Device-code flow | HTTP session-service and SSE fixture only | Not enabled; entitlement and separate WebSocket relay unverified |
| Antigravity (IDE) | Google OAuth with project onboarding | Full forwarding fixture not yet added | `agy` is a separate CLI; no verified setup |
| Gemini | Google OAuth | Full forwarding fixture not yet added | No verified native CLI route |
| GitHub Copilot | GitHub device flow or Copilot CLI import | Full forwarding fixture not yet added | No native subscription setup; Copilot CLI BYOK is a separate billing path |

Grok native validation requires an active plan; its current entitlement has
not been verified. A custom `ANTHROPIC_BASE_URL` can disable Claude Code's
Remote Control, and Switcher does not set that variable automatically.

GitHub Copilot sign-in stores the authorized GitHub identity first. Switcher
obtains a short-lived Copilot API token when that account is used, so a
temporary Copilot token-service failure does not discard a successful login.

The `provider.Provider` interface handles login, refresh, forwarding, and usage.
Optional interfaces add native-login switching and provider-specific actions.
A new provider lives in one package.

## Codex setup

`switcher setup codex` (or `switcher install`) points the Codex user config at
Switcher; `switcher setup codex --undo` (or `switcher uninstall`) restores the
previous provider.

`switcher uninstall` restores the previous Codex provider selection when
Switcher recorded it during setup. Older, untracked configurations have no
recoverable previous selection; use `switcher uninstall --legacy-remove` to
remove only a recognized legacy Switcher entry. Codex setup respects an
absolute, user-owned `CODEX_HOME` with no symlinked path components. The
menu app can inspect that variable only if it was present in the app's own
environment; a terminal-only override needs the CLI command. The first
`.switcher-backup` of an existing config is kept.

## Switching rules, precisely

| Situation | Behaviour |
|---|---|
| You click *Use this account* | selects that provider's proxy account |
| You click *Use in Claude Code* locally | verifies and writes the native login, then commits the matching proxy selection; failure preserves or rolls back the original login |
| No account was ever activated | the first usable account (by email order) serves traffic |
| Active proxy account returns 429 `usage_limit_reached` | mark it exhausted until the upstream reset time; select another usable proxy account and retry the request transparently; native Claude Code selection stays as it was |
| Every account is out of usage | no rotation; the upstream error is passed through |
| A 429 that is *not* a usage-limit error (e.g. burst limit) | passed through, nothing marked, no switch |
| Stored token expired | refresh before use; native Claude tokens are first synchronized with the live store |
| Claude Code's current native token is still expired | refresh that verified live generation under Claude's locks, save the successor to native storage and Switcher, then fetch usage; never consume an independent backup copy |
| Provider rejects a refresh credential and requires re-login | park the proxy account for an hour and try another account |
| Refresh fails transiently | preserve the selected account and report the failure; do not switch identities |
| Claude refresh outcome is uncertain | retain its consumption intent; require successor recovery or a new login before another grant |

## Security model

- **Default: local only, no authentication.** The server binds `127.0.0.1`
  and nothing leaves the machine, which is why the default install has no
  login. This is the same trust model as the CLIs themselves.
- **Optional authentication.** The Settings tab can require a password
  (PBKDF2-SHA256, 600k iterations, hashed with a per-user salt; sessions
  are random tokens stored only as SHA-256 hashes with a 7-day sliding
  expiry, HttpOnly SameSite=Strict cookies, CSRF-protected mutations, and
  login lockout). A local device token lets the menu bar app authenticate
  without a browser. When enabled, unauthenticated browsers receive only a
  standalone login page; the Accounts page, its assets, and dashboard APIs
  require a valid session. Logout and expired sessions return to that page.
  Password rotation invalidates prior credential-generation sessions. Sessions
  saved by older builds require a fresh login after upgrading. Settings read
  or parse errors fail closed, and logout reports a failed durable revocation
  rather than acknowledging success before it is saved.
- **LAN exposure, opt-in and TLS-only.** Once a password is set, a
  second listener can bind the LAN IP; it is always TLS (self-signed
  ECDSA certificate, regenerated automatically when your IP changes) and
  the local listener stays plain HTTP so the CLIs need no changes. Know
  the edges: the CLI proxy paths and the management-key hub stay reachable
  on the LAN, so only enable this on networks you trust.
  Disabling authentication or invalidating LAN prerequisites denies dashboard
  access immediately, including while a restart is pending or fails.
- **Phone access, opt-in and tailnet-only.** The Tailscale add-on serves a
  small phone page over HTTPS with Tailscale's certificate. It admits only
  your own Tailscale devices, and Switcher admits only phones approved on
  this Mac by the code they show. Sessions are bound to the phone's
  Tailscale device. The page reads usage, refreshes, switches proxy accounts
  and spends banked resets, nothing else. See [phone](phone.md).
- Hardened headers everywhere: CSP (no inline script, form-action self),
  nosniff, no-referrer. State-changing requests from a non-loopback socket
  are refused for credential routes.
- Tokens are stored unencrypted under `~/.switcher/` with `0600`
  permissions, same trust model as the CLIs themselves.
- Request bodies are forwarded verbatim; Switcher never inspects prompts.

Forgot your password? Delete `~/.switcher/settings.json` and restart
Switcher; authentication resets to off.

## Verification before a release

`make benchmark-menu` measures optimized menu construction and the click-time
preparation path using six synthetic accounts and bundled logos. The menu
keeps prepared views, patches changed providers, caches blurred email images,
and reuses formatters and text measurements. It updates countdowns on open.
State polling compares only menu-visible data; hidden web tabs pause polling,
and unchanged account cards retain their DOM nodes.

Run `make verify` before creating a tag. The same Go, race, vet, web-state,
Swift menu, and cross-platform build checks run on pushes and pull requests
and again before the tag-triggered release publishes assets. Before tagging,
also inspect the diff for credentials and verify the Settings page with a
current-source build and isolated account data. Passing checks establishes
readiness to decide on a release; it does not publish one by itself.

## Layout

```
main.go                  entry point: the server, or a command
internal/cli/            the switcher command line (a client of the local API)
internal/config/         paths and defaults
internal/store/          account + state persistence (atomic JSON writes)
internal/provider/       provider contract
internal/provider/codex/ Codex OAuth + upstream details
internal/claudecode/      native Claude Code stores, ownership, locks, and recovery
internal/desktoprelay/    opt-in Desktop CONNECT/TLS relay and per-task selection
internal/login/          browser login orchestration
internal/proxy/          request forwarding + the switching rules
internal/codexcfg/       codex config.toml installer (idempotent)
internal/usage/          cost and token usage from the CLIs' own session logs
internal/mgmtapi/        CLIProxyAPI-compatible hub surface for T3 Code
internal/update/         self-update from GitHub releases
internal/remote/         sharing between Macs: host, pairing, client, Tailscale add-on manager
cmd/switcher-tailnet/    the optional Tailscale add-on (separate module, downloaded on demand)
build/macos/             the menu bar app (single-file Swift) and packaging
web/                     dependency-free frontend (served from the binary)
```

## Development

See the [documentation index](README.md) for Desktop routing contracts,
verification limits, and the five pinned reference repositories retained for
future implementation work.

```sh
make dev      # run with the frontend served from web/ (SWITCHER_DEV=1)
```

In dev mode the UI is served straight from disk: edit a file in `web/`,
refresh the browser, and the change is live. No rebuild, no restart.

Go changes still need a rebuild and restart. That is a deliberate trade:
all routing state (active account, exhaustion windows) lives in
`state.json`, so a restart is instant and lossless, and building a custom
in-process hot reloader would add real complexity for near-zero benefit.
If you want watch-and-restart for Go code anyway, the standard
[air](https://github.com/air-verse/air) tool works fine with Switcher.
