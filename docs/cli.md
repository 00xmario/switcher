# Command line

Switch accounts, sign in, share between Macs, set up Claude Desktop and Codex,
and change settings from a terminal, over SSH or as an agent. Per-conversation
Desktop picks, banked resets and the Usage tab stay in the app for now. The
`switcher` command is a thin client for the Switcher running on
the same Mac: it talks to `http://127.0.0.1:8787`, and the running app does the
work, including Keychain access for Claude Code.

## Install

The app contains the command. Put it on your PATH once:

```sh
/Applications/Switcher.app/Contents/MacOS/SwitcherServer install-cli
```

It links `switcher` into `/usr/local/bin`, `/opt/homebrew/bin` or
`~/.local/bin`, whichever is writable. The [install script](../install.sh)
does this for you, and a source build (`make install`) is the same binary.

## Commands

| Command | What it does |
|---|---|
| `switcher status` | Accounts with usage, sharing, Claude Desktop, Away from home |
| `switcher accounts [--provider P]` | Accounts with their usage windows |
| `switcher use <account>` | Use an account; for Claude this also switches Claude Code's login |
| `switcher login <provider>` | Sign in and add an account (see below) |
| `switcher login finish <address>` | Finish a sign-in done in a browser on another device |
| `switcher login import <provider>` | Adopt the login of the provider's own CLI on this Mac |
| `switcher add-key <provider> [--key K]` | Add an API-key account; pipe the key on stdin to keep it out of shell history |
| `switcher remove <account> --yes` | Remove an account from Switcher |
| `switcher refresh [account]` | Refresh usage for one or all accounts |
| `switcher share [on\|off\|code\|devices\|revoke <id> --yes]` | Share this Mac's accounts with your other Macs |
| `switcher connect <address> --code CODE` | Use the accounts of a Mac that shares them |
| `switcher disconnect --yes` | Use this Mac's own accounts again |
| `switcher discover` | Find sharing Switchers on your network and tailnet |
| `switcher away [on\|off\|remove --yes]` | The Tailscale add-on for use away from home |
| `switcher desktop [connect\|disconnect\|restart --yes]` | Route Claude Desktop through Switcher |
| `switcher setup [codex [--undo\|--test]]` | CLI setup status; point Codex at Switcher, undo it, or send one test request through Switcher's Codex route |
| `switcher settings [set key=value ...]` | Display and banked-reset settings |
| `switcher start` | Start Switcher in the background if it is not running |
| `switcher open` | Open the dashboard |
| `switcher install-cli [--dir DIR]` | Put `switcher` on your PATH; another tool called `switcher` is left alone |

An `<account>` is its id, its email, `provider:email` when the same email has
accounts with several providers, the start of its id, or a unique part of its
email. `switcher help <command>` explains one command.

## For agents and scripts

- Every command takes `--json` (before or after the command) and then prints
  one JSON document on stdout. Commands that wait (`login`, `away on`) print
  one JSON object per line, one per event (`started`, `sign_in`), ending with
  a `done` event, or with the error document below if they fail.
- Exit codes: `0` ok, `1` failed, `2` usage error, `3` Switcher is not running.
  With `--json`, a failure also prints `{"error": "...", "code": "failed" | "usage" | "not_running"}`.
- Nothing prompts. Destructive or disruptive commands need `--yes`
  (`remove`, `disconnect`, `share revoke`, `away remove`, `desktop restart`).
  `add-key` refuses to wait on a terminal: pass `--key`, or pipe the key in.
- `--url` reaches a Switcher on another port; `SWITCHER_URL` does the same.
  The dashboard's device token is only ever sent to this Mac.
- Accounts in JSON have `id`, `provider`, `email`, `plan`, `active`,
  `claude_code` (Claude Code's own login uses it), `health` and `usage`
  (`label`, `used_percent`, `resets_at`).

## Signing in over SSH

Provider sign-ins return to a page on `localhost`, the machine with the
browser. That still works when Switcher runs on another Mac:

1. `switcher login claude` prints a sign-in link. Open it in a browser on any
   device and sign in.
2. The browser ends on a page that cannot load, because nothing listens on
   that device's `localhost`. Copy its address, which starts with
   `http://localhost:54545/callback?code=...`.
3. Paste it into the waiting command, or run
   `switcher login finish '<address>'`. Switcher delivers it to its own
   callback on its Mac and adds the account.

An agent runs `switcher login claude --no-wait --json`, shows the user the
`url` it prints, and passes the address the user pastes back to
`switcher login finish '<address>' --json`, which prints the new account.

Grok and GitHub Copilot use a device code instead: open the printed page and
enter the code. OpenCode Go takes an API key: `switcher add-key opencode`.
If the provider's own CLI is already signed in on that Mac,
`switcher login import claude` adopts it.

Forwarding the callback port also works when the device with the browser does
not run Switcher itself: `ssh -L 54545:127.0.0.1:54545 other-mac` for Claude
(Codex uses 1455, Antigravity 51121, Gemini 51122).

## Setting up a Mac over SSH

```sh
ssh you@other-mac
curl -fsSL https://raw.githubusercontent.com/00xmario/switcher/main/install.sh | sh

# Use your main Mac's accounts: no sign-ins needed on this Mac.
switcher connect studio.local --code K7QF-M2XP    # code from: switcher share code

# Or give this Mac its own accounts.
switcher login claude
switcher setup codex
```

Switcher starts in the logged-in user's desktop session when there is one.
Without one, `switcher start` runs the server on its own; Claude Code login
switching then needs the user to be logged in, because the login lives in the
Keychain.
