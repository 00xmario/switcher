# Switcher

A tiny local tool for switching your AI CLI logins between accounts on demand.

One active account serves all traffic. You switch accounts from the web UI.
Switching happens **only** when you switch, or when the active account runs
out of usage, in which case Switcher moves to another account and retries
transparently. That is the entire feature list.

```
codex CLI ──▶ Switcher (127.0.0.1:8787) ──▶ upstream, signed in as the active account
                    ▲
                    └── web UI: add / switch / remove accounts
```

## Why

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

One account is chosen, all traffic is billed to it, and that choice never
changes on its own.

Protocol translation (letting a client speak one wire format to an upstream
that speaks another) is deliberately left open. It is genuinely useful and
may be added later.

Everything else is deliberately missing. No API keys, no model aliases, no
round-robin, no plugins.

## Features

- **Manual switching, only from the web UI**: switching never happens on its
  own as long as the active account works.
- **Automatic failover on exhaustion**: when the active account reports it
  is out of usage (HTTP 429 `usage_limit_reached`), Switcher marks it with
  the upstream reset time and, if another account is usable, retries your
  in-flight request on it transparently.
- **No switch when there is nowhere to go**: if every account is out of
  usage, Switcher does not rotate; it passes the upstream error through.
- **Login in the app**: adding an account runs the provider's real OAuth
  flow in your browser; tokens are stored locally, `0600`.
- **Single Go binary**: the frontend is embedded; there is no Node build.

## Providers

| Provider | Status |
|---|---|
| Codex (ChatGPT Plus / Pro login) | supported |
| Grok | planned |
| Claude | planned |

The `provider.Provider` interface (login, refresh, forward, usage) is the
only integration point; a new provider is one package.

## Install

Requires Go 1.24+.

```sh
git clone <your-fork-url> && cd switcher
make install          # builds and puts `switcher` on your PATH
```

## Quick start

```sh
switcher                 # serves the UI + proxy on http://127.0.0.1:8787
switcher install         # points the codex CLI at Switcher (idempotent, backs up config)
```

1. Open <http://127.0.0.1:8787>.
2. **Add Codex account**: a browser window opens for the normal ChatGPT
   sign-in; the account lands in Switcher.
3. Click **Use this account** to make it active.
4. Run `codex` as usual. That's it.

`switcher uninstall` removes Switcher from the codex config again (a
`.switcher-backup` copy of your config is kept alongside it).

## Switching rules, precisely

| Situation | Behaviour |
|---|---|
| You click *Use this account* | marks it active from now on |
| No account was ever activated | the first usable account (by email order) serves traffic |
| Active account returns 429 `usage_limit_reached` | mark it exhausted until the upstream reset time; switch to another usable account and retry the request transparently |
| Every account is out of usage | no rotation; the upstream error is passed through |
| A 429 that is *not* a usage-limit error (e.g. burst limit) | passed through, nothing marked, no switch |
| Active account's token expired | refreshed via its refresh token before use |
| Token refresh fails (account needs re-login) | parked for an hour; traffic moves to another account |

## Security model

- Binds to `127.0.0.1` only; there is no authentication because there is no
  network exposure. Do not expose it.
- Tokens are stored unencrypted under `~/.switcher/` with `0600`
  permissions, same trust model as the CLIs themselves.
- Request bodies are forwarded verbatim; Switcher never inspects prompts.

## Layout

```
main.go                  entry point + subcommands (serve / install / uninstall)
internal/config/         paths and defaults
internal/store/          account + state persistence (atomic JSON writes)
internal/provider/       provider contract
internal/provider/codex/ Codex OAuth + upstream details
internal/login/          browser login orchestration
internal/proxy/          request forwarding + the switching rules
internal/codexcfg/       codex config.toml installer (idempotent)
web/                     dependency-free frontend (served from the binary)
```

## Development

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

## Disclaimer

Switcher is an unofficial tool and is not affiliated with OpenAI. It proxies
your own accounts on your own machine; what you do with that is your
business and your responsibility.

## License

MIT. See [LICENSE](LICENSE).
