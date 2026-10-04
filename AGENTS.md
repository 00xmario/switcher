# Project guidance

## Versioning

Bump `VERSION`, build-time version strings, and app bundle versions only when
preparing a release explicitly requested by the user. Ordinary development,
testing, local installs, and work sessions keep the current release version.
Do not invent a new preview version for each session. Mark unreleased work in
the task summary or Git state instead.

## Runtime and reference material

Keep active Claude Desktop and CLI sessions intact during development. Use
temporary profiles, mock credentials and isolated upstreams for verification.
Restart or reconfigure the user's running applications only when requested.

Before changing Desktop routing, native credential ownership, or setup, read
`docs/README.md` and the relevant linked contract. The pinned reference projects
and local clone paths are documented in `docs/desktop-reference-map.md`.
