# github-core-clone-01 — a GitHub-compatible forge in one static binary

[![bounty](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/ytta90/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/ytta90/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01)

A from-scratch GitHub-compatible forge written in Go, built for the
[github-core-clone-01 bounty](https://make.fun/competitions/github-core-clone-01)
on [make.fun](https://make.fun).

## What it serves

- **Git smart HTTP** — protocol v2 (`ls-refs`, `fetch` with shallow/deepen,
  thin-pack) and v0 (`fetch`, `receive-pack` with `report-status-v2`,
  side-band-64k, non-fast-forward rejection), with real packfile
  read/write, delta resolution and SHA-1 verification.
- **REST v3** — repositories, branches, commits, contents, labels, issues,
  comments and pull requests (including squash merge with three-way
  conflict detection), plus `Link` pagination.
- **GraphQL v4** — viewer, repository, issues, pull requests, mutations
  (create/close issue, create/merge pull request, add comment), `__type`
  introspection, `node(id:)` and cursor pagination, batched operations.
- **MCP** — Streamable HTTP at `/mcp`, modern `2026-07-28` era via
  `server/discover` (legacy `initialize` fallback), six tools.

## Layout

- `main.ts` — entry lifecycle: spawns `./forge`, registers with the env,
  polls `/done`.
- `forge` — the statically linked Linux amd64 server binary (Go, no
  runtime dependencies). Sources: `*.go` in this repo.
- `LICENSE` — MIT.

## Running it locally

```sh
go build -o forge .
FORGE_PORT=8080 MAKEFUN_SHOP_TOKEN=test-token ./forge
```

Then `GET /healthz` answers `{"ok": true}` and the API lives under
`/api/v3`, `/api/graphql`, `/mcp` and `/{owner}/{repo}.git`.
