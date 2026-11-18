# github-core-clone-01

A GitHub-compatible git forge: git clone/push over smart HTTP, repos + issues + pull requests + contents over REST v3, GraphQL, and MCP tools — all from one process with its own in-memory store. No UI, no outside service.

Built for the [make.fun bounty](https://make.fun/bounty/github-core-clone-01) — "Build a GitHub-compatible git forge".

[![bounty](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/ytta90/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/ytta90/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01)

## How it runs

`main.ts` (Node 22) spawns `./forge`, a statically linked Go binary that serves everything:

- **git smart HTTP** — upload-pack v2 (ls-refs, fetch with shallow/deepen, thin-pack) and v0, receive-pack v0 with report-status; packs are version 2, full objects, SHA-1 verified.
- **REST v3** — `/api/v3/` (user, repos, branches, commits, contents, labels, issues, comments, pull requests, merge).
- **GraphQL** — `/api/graphql` (viewer, repository, node, `__type`, createIssue/closeIssue/createPullRequest/mergePullRequest/addComment, batching).
- **MCP** — `/mcp` (Streamable HTTP, era 2026-07-28): create_repo, get_repo, create_issue, list_issues, create_pull_request, merge_pull_request.

The forge binds an ephemeral port on 127.0.0.1, prints `listening on <port>`, and `main.ts` registers that port with the environment (`$MAKEFUN_ENV_URL/register`) and polls `$MAKEFUN_ENV_URL/done` until the trial ends.

## Layout

- `main.ts` — launcher (Node 22, no dependencies).
- `forge` — the Go core binary (linux/amd64, statically linked).
- `src/` — the Go source: hand-rolled HTTP/1.1 over raw syscalls, in-memory store, hand-rolled JSON, SHA-1 git objects, pack read/write.
- `LICENSE` — MIT.

## Performance

Hand-rolled HTTP/1.1 (raw syscalls, no `net/http`), zero-alloc hot paths, pre-serialized and cached responses (issue JSON, fetch packs), O(1) routing and lookups. Local practice suite: **120/120 conformance, 200/200 perf**.
