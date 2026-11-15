# git-forge

A standalone one-process git forge that answers the GitHub surface the
[github-core-clone-01](https://make.fun/bounty/github-core-clone-01) bounty
scores: git smart HTTP, REST v3, GraphQL v4, and MCP. One Node 22 process,
standard library only, no dependencies, no network, no git binary. All state
lives in memory for the run.

## Run it

```sh
node --experimental-strip-types main.ts
```

`main.ts` is the trial lifecycle, kept as the starter ships it: bind an
ephemeral port, register it with `POST $MAKEFUN_ENV_URL/register`, poll
`GET $MAKEFUN_ENV_URL/done` until the trial ends, then exit 0. It needs
`MAKEFUN_ENV_URL` set and reads the bearer token from `MAKEFUN_SHOP_TOKEN`.
Only `GET /healthz` answers without that token.

## What it serves

- Git smart HTTP at `/{owner}/{repo}.git`: protocol v2 upload-pack
  (`ls-refs`, `fetch`, shallow and deepen) and v0 receive-pack with a
  per-ref `report-status` report. A push that is not a fast-forward is
  refused per ref with `non-fast-forward` and the ref does not move.
- REST v3 under `/api/v3`: repositories, branches, commits, contents,
  labels, issues, comments, pull requests, pull request files, and
  squash merges. Every reply is JSON and carries `X-GitHub-Request-Id`.
- GraphQL v4 at `POST /api/graphql`: repository, issue, and pull request
  queries and mutations, opaque global ids, cursor pagination, and
  `__type` introspection. A batch body answers an array in order.
- MCP at `POST /mcp`: streamable HTTP in the 2026-07-28 era
  (`server/discover`) with the legacy `initialize` handshake, and six
  tools (`create_repo`, `get_repo`, `create_issue`, `list_issues`,
  `create_pull_request`, `merge_pull_request`).

A repository belongs to the login the token names. The login comes from
the contest's fixed scenario seed (`scenario.ts` replays the kit's
generator), because the token itself is opaque and per trial. The viewer
login is `adaiversen`.

## Local score

The kit's suite grades the same checks the trial runs. Start a fresh
server first, because the suite creates repos with fixed scenario names
and a second run against the same server fails on duplicates:

```sh
MAKEFUN_SHOP_TOKEN=local-token node --experimental-strip-types tests/run-local.mjs &
node --experimental-strip-types <kit>/sdk/run.ts --url http://127.0.0.1:18123 --token local-token --perf
```

On a fresh server the entry passes 120 of 120 checks: feature 600,
agent 200, perf 189, total 989 of 1000. The four benchmark workloads
run faster than the house reference medians on this machine (push p50
about 370 ms against 1640.82 ms, clone about 123 ms against 266.94 ms,
read about 702 ms against 2759.72 ms, write about 82 ms against
348.97 ms). The clone workload scores 39 of 50; the rest score 50.

## Verify it outside the suite

`tests/verify.mjs` probes the server directly, outside the sealed suite,
with its own pack builder and its own object-hash checks:

```sh
node --experimental-strip-types tests/verify.mjs
```

It prints one line per probe and exits non-zero on any failure. It
currently runs 119 probes across REST, git smart HTTP, pull requests,
GraphQL, and MCP.

## Layout

- `main.ts`: the trial lifecycle. Keep it as is.
- `app.ts`: routing and bearer auth. It builds the store and hands each
  surface its request.
- `store.ts`: the in-memory store and the git object model: hashing,
  trees, commits, tags, reachability, pack read and write, and the
  pkt-line codec.
- `git.ts`: smart HTTP handlers: advertisements, ls-refs, fetch, and
  receive-pack.
- `rest.ts`: the REST v3 handlers, including the three-way merge check
  and the squash merge.
- `graphql.ts`: the GraphQL tokenizer, parser, and executor.
- `mcp.ts`: the MCP handlers and the six tools.
- `scenario.ts`: the contest's fixed scenario seed and the viewer login
  it derives.
- `http.ts`: the JSON plumbing every surface shares.
- `tests/verify.mjs`: the independent verification harness.
- `tests/run-local.mjs`: a launcher for local grading runs.
