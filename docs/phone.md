# Switcher on your phone

Open Switcher on your phone wherever you are. You can:

- see every account's usage;
- refresh it;
- switch accounts, Claude included;
- spend a banked Codex reset.

The phone reaches your Mac over Tailscale only, at an address with a real
HTTPS certificate. No port is opened on your router, and the page cannot be
reached from the internet.

## Set it up once

1. **Turn on the Tailscale add-on.** Go to Settings → Share between Macs and
   turn on **Away from home**, then sign in with Tailscale. From a terminal:
   `switcher away on`.
2. **Turn on MagicDNS and HTTPS certificates** in your tailnet's
   [DNS settings](https://login.tailscale.com/admin/dns). They give this Mac's
   Switcher an address like `https://switcher-studio-mac.tail1234.ts.net` with
   a certificate Safari trusts.
3. **Turn on phone access** under Settings → Phone, or run
   `switcher phone on`. Settings shows what is still missing and the address
   once it works.
4. **Install the Tailscale app on your phone** and sign in with the same
   Tailscale account.
5. **Approve your phone.**
   1. Open the address on your phone and tap **Pair this phone**.
   2. The phone shows a six-digit code.
   3. Type that code into Settings → Phone on the Mac, or run
      `switcher phone approve 482913`.
   4. The phone opens Switcher within two seconds.

Add the page to your home screen for one-tap access.

## Who can get in

A request from a phone passes four checks before it reaches anything:

1. **Your tailnet only.** The page is served by the add-on's own Tailscale
   device. Devices that are not in your tailnet cannot connect at all; this
   includes devices on your Wi‑Fi and everything on the internet.
2. **Your devices only.** For every connection the add-on asks Tailscale which
   device and user it comes from. It refuses:
   - devices of other users in the same tailnet;
   - devices shared in from another tailnet;
   - tagged devices.
   It also refuses all connections if the add-on itself is tagged. A browser
   cannot pretend to be another device: the add-on replaces any identity
   headers the browser sends.
3. **Approved phones only.** A device of yours still needs approval on this
   Mac. Approving means typing the code the phone shows into Switcher on the
   Mac, so you approve the phone in your hand and nothing else.
   - Codes expire after five minutes.
   - At most three phones can wait at once, and one device can ask for a new
     code every five seconds.
   - After ten wrong codes, approvals pause for up to five minutes.
4. **The phone's own session.** An approved phone receives a session cookie
   with these properties:
   - Secure, HttpOnly and SameSite=Strict.
   - It works only from the Tailscale device it was issued to, so a copied
     cookie is useless on any other device.
   - It expires 30 days after the phone was last used, and at most 90 days
     after approval.
   - Every action also needs a CSRF token and a same-origin request.
   - Switcher stores only a hash of the cookie.

One thing does become public: like every HTTPS certificate, the one
Tailscale issues for this Mac's Switcher is listed in public Certificate
Transparency logs. That reveals the name `switcher-<your Mac's name>` and your
tailnet's name, but not an address or a way in.

Revoke a phone under Settings → Phone or with
`switcher phone revoke <name> --yes`. Turning phone access off closes the page
right away, for every phone. Approved phones stay approved, though: turning
access on again lets them back in, so revoke a phone you lost.

## What the phone can do

The phone page talks to its own listener, which has a short, fixed list of
actions:

- read accounts and usage;
- refresh usage;
- use an account, including a Claude account, which also switches Claude
  Code's login on the Mac;
- spend a banked reset;
- sign itself out.

Everything else stays on the Mac:

- settings and passwords;
- sign-ins and keys, including the T3 Code hub key;
- Claude Desktop and sharing.

The phone listener has its own router that admits only the API requests
behind these actions. Those requests never count as coming from this Mac, so
anything Switcher keeps local refuses them as well. The one exception is
switching Claude Code's login: Switcher allows it for a request that carries an
approved phone. Only the phone listener can mark a request that way, after the
session, CSRF and origin checks, and the mark never crosses the network.

## Troubleshooting

- **The address does not open:** make sure Tailscale is connected on the
  phone and the Mac is awake. Settings → Phone says what is still missing on
  the Mac.
- **"This Switcher only answers its owner's devices":** the phone is signed in
  to Tailscale as another user. Sign in with the account you used on the Mac.
- **The code expired:** tap Pair this phone again for a new one.

## Implementation

The add-on is `cmd/switcher-tailnet`; its phone part is in `phone.go`.

1. **Serving.** With phone access on, the add-on serves port 443 on its
   tailnet device with `tsnet.ListenTLS`, which uses Tailscale's certificate
   for that name.
2. **Checking the caller.** For each request, the add-on:
   - calls `WhoIs` and compares the device's user with its own;
   - checks the requested host name;
   - removes every `X-Switcher-Phone-*` header the browser sent.
3. **Passing it on.** It then forwards the request to Switcher's phone
   listener on a random loopback port. It adds the device's stable node id,
   name, OS and Tailscale login, and a phone key. Switcher picks a new phone
   key each time it starts the add-on, separate from the add-on's control
   token, and hands it over in the environment.

`internal/phone` is Switcher's side:

- It accepts only requests that carry the add-on's phone key.
- It handles pairing and node-bound sessions, and stores approved phones in
  `~/.switcher/remote/phone.json` (mode 0600, session hashes only).
- It sends the fixed actions to Switcher's API through a router that knows
  only those routes, from a non-local address.
- It returns a reduced state with no keys or settings.

Phone management (`/api/phone…`) is local to this Mac. Paired Macs cannot
reach it, and neither can the phone listener.
