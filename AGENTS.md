# Project guidance

## Versioning

Bump `VERSION`, build-time version strings, and app bundle versions only when
preparing a release explicitly requested by the user. Ordinary development,
testing, local installs, and work sessions keep the current release version.
Do not invent a new preview version for each session. Mark unreleased work in
the task summary or Git state instead.

Release numbers after v1.1.0 raise the middle number by one, with the last
number at 0: v1.1.0, then v1.11.0, v1.12.0, v1.13.0 and so on. Do not use
v1.2.0 or patch releases such as v1.1.1. Confirm the number with the user
right before tagging.

## Runtime and reference material

Keep active Claude Desktop and CLI sessions intact during development. Use
temporary profiles, mock credentials and isolated upstreams for verification.
Restart or reconfigure the user's running applications only when requested.

Before changing Desktop routing, native credential ownership, or setup, read
`docs/README.md` and the relevant linked contract. The pinned reference projects
and local clone paths are documented in `docs/desktop-reference-map.md`.
