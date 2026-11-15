// REST v3: the /api/v3 surface.
//
// Rules the whole surface lives by: every reply is JSON (never HTML), every
// reply carries a non-empty X-GitHub-Request-Id, 401/404/422 bodies carry a
// string `message`, an unknown route is a JSON 404, a malformed JSON body is
// a 400. A repo belongs to the login the token names; the same repo name under
// another owner is a 404, never a 403.
//
// Endpoints: GET /api/v3/ and /api/v3/meta, GET /api/v3/user, user/repos
// (list with Link pagination, create), repos/{owner}/{repo} (get, patch,
// delete), git/refs (create, read one, list branches), commits (per-branch),
// contents (put, get, delete), labels, issues (create, list, get, patch,
// comments), pulls (create, get, list, files, merge).

import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  FIXED_EPOCH_SEC,
  FIXED_ISO,
  buildCommit,
  buildTree,
  commitList,
  diffTrees,
  flattenTree,
  isObjectId,
  mergeBase,
  parseCommit,
  parseTree,
  putObject,
  serverIdentity,
  treeEntryAt,
  treeWithPath,
  ZERO_OID,
} from './store.ts';
import type { Issue, PullRequest, Repo, Store } from './store.ts';
import { isRecord, parseJsonBody, pathOf, queryOf, readBody, sendEmpty, sendJson } from './http.ts';

const MAX_BODY = 16 * 1024 * 1024;
const DEFAULT_PER_PAGE = 30;

export interface RestContext {
  readonly store: Store;
  readonly login: string;
  readonly requestId: string;
}

type Handler = (ctx: RestContext, req: IncomingMessage, res: ServerResponse, params: Record<string, string>) => Promise<void>;

interface Route {
  readonly method: string;
  readonly pattern: RegExp;
  readonly keys: string[];
  readonly handler: Handler;
}

function route(method: string, pattern: string, handler: Handler): Route {
  const keys: string[] = [];
  // {path} and {branch} are always the last segment and may contain slashes
  // (a nested file path, a release/1.0 branch); every other parameter is one
  // segment. A greedy match on a middle segment would swallow the rest.
  const source = pattern.replace(/\{(\w+)\}/g, (_match, key: string) => {
    keys.push(key);
    return key === 'path' || key === 'branch' ? '(.+)' : '([^/]+)';
  });
  return { method, pattern: new RegExp(`^${source}$`), keys, handler };
}

function headers(ctx: RestContext): Record<string, string> {
  return { 'x-github-request-id': ctx.requestId };
}

function notFound(ctx: RestContext, res: ServerResponse): void {
  sendJson(res, 404, { message: 'Not Found' }, headers(ctx));
}

function badRequest(ctx: RestContext, res: ServerResponse, message: string): void {
  sendJson(res, 400, { message }, headers(ctx));
}

function unprocessable(ctx: RestContext, res: ServerResponse, message: string): void {
  sendJson(res, 422, { message }, headers(ctx));
}

/** The repo a request names, or null when the owner or the repo is wrong. */
function resolveRepo(ctx: RestContext, owner: string, name: string): Repo | null {
  if (owner !== ctx.login) return null;
  return ctx.store.getRepo(owner, name) ?? null;
}

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: repo.id },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
  };
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const label = repo.labels.get(name);
      return { name, color: label?.color ?? '000000' };
    }),
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
  };
}

function pullJson(repo: Repo, pull: PullRequest): Record<string, unknown> {
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    head: { ref: pull.headRef, sha: pull.headSha },
    base: { ref: pull.baseRef, sha: pull.baseSha },
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
  };
}

/** Read and parse a JSON body; null when the body is empty, 400 when malformed. */
async function readJsonBody(ctx: RestContext, req: IncomingMessage, res: ServerResponse): Promise<Record<string, unknown> | null | 'bad'> {
  let body: Buffer;
  try {
    body = await readBody(req, MAX_BODY);
  } catch {
    badRequest(ctx, res, 'request body too large');
    return 'bad';
  }
  let parsed: unknown;
  try {
    parsed = parseJsonBody(body);
  } catch {
    badRequest(ctx, res, 'malformed JSON body');
    return 'bad';
  }
  if (parsed === null) return null;
  if (!isRecord(parsed)) {
    badRequest(ctx, res, 'JSON body must be an object');
    return 'bad';
  }
  return parsed;
}

function parsePerPage(query: URLSearchParams): number {
  const raw = query.get('per_page');
  if (raw === null) return DEFAULT_PER_PAGE;
  const value = Number.parseInt(raw, 10);
  return Number.isFinite(value) && value > 0 ? Math.min(value, 100) : DEFAULT_PER_PAGE;
}

function parsePage(query: URLSearchParams): number {
  const raw = query.get('page');
  if (raw === null) return 1;
  const value = Number.parseInt(raw, 10);
  return Number.isFinite(value) && value > 0 ? value : 1;
}

/** The absolute URL of this request, for Link pagination headers. */
function absoluteUrl(req: IncomingMessage): string {
  const host = req.headers.host ?? '127.0.0.1';
  return `http://${host}${req.url ?? '/'}`;
}

/** A Link header for the next page, or null when this page is the last. */
function nextLink(req: IncomingMessage, page: number, perPage: number, hasMore: boolean): string | null {
  if (!hasMore) return null;
  const url = new URL(absoluteUrl(req));
  url.searchParams.set('page', String(page + 1));
  url.searchParams.set('per_page', String(perPage));
  return `<${url.toString()}>; rel="next"`;
}

/** Slice `items` to one page and answer with an optional Link header. */
function sendList<T>(ctx: RestContext, req: IncomingMessage, res: ServerResponse, items: readonly T[]): void {
  const query = queryOf(req);
  const perPage = parsePerPage(query);
  const page = parsePage(query);
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  const link = nextLink(req, page, perPage, start + perPage < items.length);
  sendJson(res, 200, slice, link === null ? headers(ctx) : { ...headers(ctx), link });
}

/** The tip of a branch ref, or null when the branch does not exist. */
function branchTip(repo: Repo, branch: string): string | null {
  return repo.refs.get(`refs/heads/${branch}`) ?? null;
}

/**
 * Write a contents commit: a new commit on `branch` whose tree is the current
 * tree with `path` set to `blobOid` (or removed when null). Returns the commit.
 */
function writeContentsCommit(repo: Repo, ctx: RestContext, branch: string, path: string, blobOid: string | null, message: string): string {
  const tip = branchTip(repo, branch);
  const baseTree = tip === null ? null : parseCommit(repo.objects.get(tip)!.data).tree;
  const treeOid = treeWithPath(repo, baseTree, path.split('/'), blobOid);
  const parents = tip === null ? [] : [tip];
  const identity = serverIdentity(ctx.login);
  const commitOid = putObject(
    repo,
    'commit',
    buildCommit({ tree: treeOid, parents, message, author: identity, committer: identity, timestamp: FIXED_EPOCH_SEC }),
  );
  repo.refs.set(`refs/heads/${branch}`, commitOid);
  return commitOid;
}

/** The files a pull request changes: merge-base tree versus head tree. */
function pullFiles(repo: Repo, pull: PullRequest): Array<Record<string, unknown>> {
  const headCommit = repo.objects.get(pull.headSha);
  if (headCommit === undefined || headCommit.type !== 'commit') return [];
  const headTree = parseCommit(headCommit.data).tree;
  const base = mergeBase(repo.objects, pull.baseSha, pull.headSha);
  const baseTree = base === null ? null : parseCommit(repo.objects.get(base)!.data).tree;
  return diffTrees(repo, baseTree, headTree).map((change) => ({
    filename: change.path,
    status: change.status === 'deleted' ? 'removed' : change.status,
    additions: change.additions,
    deletions: change.deletions,
    changes: change.additions + change.deletions,
  }));
}

/** True when merging `pull` would conflict (base moved under a changed file). */
export function pullConflicts(repo: Repo, pull: PullRequest): boolean {
  const headCommit = repo.objects.get(pull.headSha);
  if (headCommit === undefined || headCommit.type !== 'commit') return true;
  const base = mergeBase(repo.objects, pull.baseSha, pull.headSha);
  if (base === null) return true;
  const baseTip = branchTip(repo, pull.baseRef);
  if (baseTip === null) return true;
  const baseTree = parseCommit(repo.objects.get(base)!.data).tree;
  const headTree = parseCommit(headCommit.data).tree;
  const currentTree = parseCommit(repo.objects.get(baseTip)!.data).tree;
  const headFiles = flattenTree(repo, headTree);
  const baseFiles = flattenTree(repo, baseTree);
  const currentFiles = flattenTree(repo, currentTree);
  for (const [path, headEntry] of headFiles) {
    const baseEntry = baseFiles.get(path);
    if (baseEntry !== undefined && baseEntry.oid === headEntry.oid) continue;
    const currentEntry = currentFiles.get(path);
    if (baseEntry === undefined) {
      // Added on the head: a conflict only when the base added the same path.
      if (currentEntry !== undefined) return true;
    } else if (currentEntry === undefined || currentEntry.oid !== baseEntry.oid) {
      // Changed on the head: a conflict when the base deleted it or changed it.
      return true;
    }
  }
  for (const [path, baseEntry] of baseFiles) {
    if (headFiles.has(path)) continue;
    const currentEntry = currentFiles.get(path);
    // Deleted on the head: a conflict only when the base changed it (not when
    // the base deleted it too or left it alone).
    if (currentEntry !== undefined && currentEntry.oid !== baseEntry.oid) return true;
  }
  return false;
}

/** Apply a squash merge of `pull` onto its base branch; returns the merge commit. */
export function squashMerge(repo: Repo, ctx: { readonly login: string }, pull: PullRequest): string {
  const headCommit = repo.objects.get(pull.headSha)!;
  const headTree = parseCommit(headCommit.data).tree;
  const baseTip = branchTip(repo, pull.baseRef)!;
  const baseTree = parseCommit(repo.objects.get(baseTip)!.data).tree;
  const base = mergeBase(repo.objects, pull.baseSha, pull.headSha);
  const baseTreeAtMerge = base === null ? null : parseCommit(repo.objects.get(base)!.data).tree;
  // Start from the base tree, apply the head's changes over it.
  const merged = new Map<string, { mode: string; name: string; oid: string }>();
  for (const [path, entry] of flattenTree(repo, baseTree)) merged.set(path, entry);
  const headFiles = flattenTree(repo, headTree);
  const baseFiles = baseTreeAtMerge === null ? new Map<string, { mode: string; name: string; oid: string }>() : flattenTree(repo, baseTreeAtMerge);
  for (const [path, entry] of headFiles) merged.set(path, entry);
  for (const [path] of baseFiles) if (!headFiles.has(path)) merged.delete(path);
  // Rebuild the tree from the flat map, one directory at a time: a path
  // contributes its first segment after this directory (a file when that is
  // the whole remainder, a subdirectory otherwise).
  const treeCache = new Map<string, string>();
  const buildDir = (dir: string): string => {
    if (treeCache.has(dir)) return treeCache.get(dir)!;
    const entries: Array<{ mode: string; name: string; oid: string }> = [];
    const children = new Set<string>();
    const prefix = dir === '' ? '' : `${dir}/`;
    for (const [path, entry] of merged) {
      if (!path.startsWith(prefix)) continue;
      const rest = path.slice(prefix.length);
      if (rest === '') continue;
      const slash = rest.indexOf('/');
      if (slash < 0) {
        entries.push({ mode: entry.mode, name: rest, oid: entry.oid });
      } else {
        children.add(rest.slice(0, slash));
      }
    }
    for (const child of children) {
      entries.push({ mode: '40000', name: child, oid: buildDir(dir === '' ? child : `${dir}/${child}`) });
    }
    const oid = putObject(repo, 'tree', buildTree(entries));
    treeCache.set(dir, oid);
    return oid;
  };
  const treeOid = buildDir('');
  const identity = serverIdentity(ctx.login);
  const commitOid = putObject(
    repo,
    'commit',
    buildCommit({ tree: treeOid, parents: [baseTip], message: pull.title, author: identity, committer: identity, timestamp: FIXED_EPOCH_SEC }),
  );
  repo.refs.set(`refs/heads/${pull.baseRef}`, commitOid);
  return commitOid;
}

const routes: Route[] = [
  route('GET', '/api/v3/', async (ctx, _req, res) => {
    sendJson(res, 200, { current_user_url: '/api/v3/user' }, { ...headers(ctx), 'x-oauth-scopes': 'repo, read:org' });
  }),
  route('GET', '/api/v3/meta', async (ctx, _req, res) => {
    sendJson(res, 200, { installed_version: '1.0.0' }, headers(ctx));
  }),
  route('GET', '/api/v3/user', async (ctx, _req, res) => {
    sendJson(res, 200, { login: ctx.login }, headers(ctx));
  }),
  route('GET', '/api/v3/user/repos', async (ctx, req, res) => {
    sendList(ctx, req, res, ctx.store.listRepos(ctx.login).map((repo) => repoJson(repo)));
  }),
  route('POST', '/api/v3/user/repos', async (ctx, req, res) => {
    const body = await readJsonBody(ctx, req, res);
    if (body === 'bad' || body === null) {
      if (body === null) badRequest(ctx, res, 'a repo name is required');
      return;
    }
    const name = typeof body.name === 'string' ? body.name : '';
    if (name === '') {
      unprocessable(ctx, res, 'name is required');
      return;
    }
    const repo = ctx.store.createRepo(
      ctx.login,
      name,
      body.private === true,
      typeof body.description === 'string' ? body.description : '',
    );
    if (repo === null) {
      unprocessable(ctx, res, 'name already exists on this account');
      return;
    }
    sendJson(res, 201, repoJson(repo), headers(ctx));
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}', async (ctx, _req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    sendJson(res, 200, repoJson(repo), headers(ctx));
  }),
  route('PATCH', '/api/v3/repos/{owner}/{repo}', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const body = await readJsonBody(ctx, req, res);
    if (body === 'bad') return;
    if (body !== null) {
      if (typeof body.description === 'string') repo.description = body.description;
      if (typeof body.private === 'boolean') repo.isPrivate = body.private;
    }
    sendJson(res, 200, repoJson(repo), headers(ctx));
  }),
  route('DELETE', '/api/v3/repos/{owner}/{repo}', async (ctx, _req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    ctx.store.deleteRepo(repo.owner, repo.name);
    sendEmpty(res, 204, headers(ctx));
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}/branches', async (ctx, _req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const branches: Array<Record<string, unknown>> = [];
    for (const [ref, sha] of [...repo.refs.entries()].sort()) {
      if (!ref.startsWith('refs/heads/')) continue;
      branches.push({ name: ref.slice('refs/heads/'.length), commit: { sha } });
    }
    sendJson(res, 200, branches, headers(ctx));
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}/branches/{branch}', async (ctx, _req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const tip = branchTip(repo, params.branch);
    if (tip === null) {
      notFound(ctx, res);
      return;
    }
    sendJson(res, 200, { name: params.branch, commit: { sha: tip } }, headers(ctx));
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}/commits', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const query = queryOf(req);
    const branch = query.get('sha') ?? repo.defaultBranch;
    const tip = branchTip(repo, branch);
    if (tip === null) {
      if (isObjectId(branch)) {
        const object = repo.objects.get(branch);
        if (object !== undefined && object.type === 'commit') {
          sendList(ctx, req, res, commitList(repo, branch).map((sha) => commitJson(repo, sha)));
          return;
        }
      }
      notFound(ctx, res);
      return;
    }
    sendList(ctx, req, res, commitList(repo, tip).map((sha) => commitJson(repo, sha)));
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}/git/refs/heads/{branch}', async (ctx, _req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const tip = branchTip(repo, params.branch);
    if (tip === null) {
      notFound(ctx, res);
      return;
    }
    sendJson(res, 200, { ref: `refs/heads/${params.branch}`, object: { sha: tip } }, headers(ctx));
  }),
  route('POST', '/api/v3/repos/{owner}/{repo}/git/refs', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const body = await readJsonBody(ctx, req, res);
    if (body === 'bad' || body === null) {
      if (body === null) badRequest(ctx, res, 'ref and sha are required');
      return;
    }
    const ref = typeof body.ref === 'string' ? body.ref : '';
    const sha = typeof body.sha === 'string' ? body.sha : '';
    if (!ref.startsWith('refs/') || !isObjectId(sha) || !repo.objects.has(sha)) {
      unprocessable(ctx, res, 'ref and a known sha are required');
      return;
    }
    if (repo.refs.has(ref)) {
      unprocessable(ctx, res, 'Reference already exists');
      return;
    }
    repo.refs.set(ref, sha);
    sendJson(res, 201, { ref, object: { sha } }, headers(ctx));
  }),
  route('PUT', '/api/v3/repos/{owner}/{repo}/contents/{path}', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const body = await readJsonBody(ctx, req, res);
    if (body === 'bad' || body === null) {
      if (body === null) badRequest(ctx, res, 'message and content are required');
      return;
    }
    const message = typeof body.message === 'string' ? body.message : '';
    const content = typeof body.content === 'string' ? body.content : '';
    if (message === '' || content === '') {
      unprocessable(ctx, res, 'message and content are required');
      return;
    }
    let contentBytes: Buffer;
    try {
      contentBytes = Buffer.from(content, 'base64');
    } catch {
      unprocessable(ctx, res, 'content must be base64');
      return;
    }
    const branch = typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
    const path = decodeURIComponent(params.path);
    const tip = branchTip(repo, branch);
    const existingSha = tip === null ? null : (() => {
      const commit = repo.objects.get(tip)!;
      const entry = treeEntryAt(repo, parseCommit(commit.data).tree, path.split('/'));
      return entry === null ? null : entry.oid;
    })();
    if (existingSha !== null && body.sha !== existingSha) {
      sendJson(res, 409, { message: 'sha does not match the current blob' }, headers(ctx));
      return;
    }
    const blobOid = putObject(repo, 'blob', contentBytes);
    const commitOid = writeContentsCommit(repo, ctx, branch, path, blobOid, message);
    const status = existingSha === null ? 201 : 200;
    sendJson(
      res,
      status,
      { content: { sha: blobOid, encoding: 'base64' }, commit: { sha: commitOid } },
      headers(ctx),
    );
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}/contents/{path}', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const query = queryOf(req);
    const ref = query.get('ref') ?? repo.defaultBranch;
    let tip = branchTip(repo, ref);
    if (tip === null && isObjectId(ref)) {
      const object = repo.objects.get(ref);
      if (object !== undefined && object.type === 'commit') tip = ref;
    }
    if (tip === null) {
      notFound(ctx, res);
      return;
    }
    const commit = repo.objects.get(tip)!;
    const treeOid = parseCommit(commit.data).tree;
    const path = decodeURIComponent(params.path);
    const segments = path.split('/').filter((part) => part !== '');
    const entry = segments.length === 0 ? null : treeEntryAt(repo, treeOid, segments);
    if (entry === null && segments.length > 0) {
      notFound(ctx, res);
      return;
    }
    if (entry !== null && entry.mode === '40000') {
      const children: Array<Record<string, unknown>> = [];
      const treeObject = repo.objects.get(entry.oid);
      if (treeObject !== undefined && treeObject.type === 'tree') {
        for (const child of parseTree(treeObject.data)) {
          children.push({
            type: child.mode === '40000' ? 'dir' : 'file',
            name: child.name,
            path: `${path}/${child.name}`,
            sha: child.oid,
          });
        }
      }
      sendJson(res, 200, children, headers(ctx));
      return;
    }
    if (entry !== null) {
      const blob = repo.objects.get(entry.oid);
      if (blob === undefined || blob.type !== 'blob') {
        notFound(ctx, res);
        return;
      }
      sendJson(
        res,
        200,
        { type: 'file', encoding: 'base64', content: blob.data.toString('base64'), sha: entry.oid, size: blob.data.length, path },
        headers(ctx),
      );
      return;
    }
    notFound(ctx, res);
  }),
  route('DELETE', '/api/v3/repos/{owner}/{repo}/contents/{path}', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const body = await readJsonBody(ctx, req, res);
    if (body === 'bad') return;
    const path = decodeURIComponent(params.path);
    const branch = body !== null && typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
    const tip = branchTip(repo, branch);
    if (tip === null) {
      notFound(ctx, res);
      return;
    }
    const commit = repo.objects.get(tip)!;
    const entry = treeEntryAt(repo, parseCommit(commit.data).tree, path.split('/'));
    if (entry === null) {
      notFound(ctx, res);
      return;
    }
    if (body !== null && typeof body.sha === 'string' && body.sha !== '' && body.sha !== entry.oid) {
      sendJson(res, 409, { message: 'sha does not match the current blob' }, headers(ctx));
      return;
    }
    const message = body !== null && typeof body.message === 'string' ? body.message : `Delete ${path}`;
    const commitOid = writeContentsCommit(repo, ctx, branch, path, null, message);
    sendJson(res, 200, { commit: { sha: commitOid } }, headers(ctx));
  }),
  route('POST', '/api/v3/repos/{owner}/{repo}/labels', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const body = await readJsonBody(ctx, req, res);
    if (body === 'bad' || body === null) {
      if (body === null) badRequest(ctx, res, 'name is required');
      return;
    }
    const name = typeof body.name === 'string' ? body.name : '';
    if (name === '') {
      unprocessable(ctx, res, 'name is required');
      return;
    }
    const color = typeof body.color === 'string' ? body.color : '000000';
    repo.labels.set(name, { name, color });
    sendJson(res, 201, { name, color }, headers(ctx));
  }),
  route('POST', '/api/v3/repos/{owner}/{repo}/issues', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const body = await readJsonBody(ctx, req, res);
    if (body === 'bad' || body === null) {
      if (body === null) badRequest(ctx, res, 'title is required');
      return;
    }
    const title = typeof body.title === 'string' ? body.title : '';
    if (title === '') {
      unprocessable(ctx, res, 'title is required');
      return;
    }
    const labels: string[] = [];
    if (Array.isArray(body.labels)) {
      for (const label of body.labels) {
        const name = typeof label === 'string' ? label : typeof label === 'object' && label !== null ? (label as Record<string, unknown>).name : undefined;
        if (typeof name !== 'string') continue;
        if (!repo.labels.has(name)) {
          unprocessable(ctx, res, `label ${name} does not exist`);
          return;
        }
        labels.push(name);
      }
    }
    const number = repo.nextIssueNumber++;
    const issue: Issue = {
      id: repo.id * 1_000_000 + number,
      number,
      title,
      body: typeof body.body === 'string' ? body.body : '',
      state: 'open',
      labels,
      comments: [],
      createdAt: FIXED_ISO,
      updatedAt: FIXED_ISO,
    };
    repo.issues.set(number, issue);
    repo.issueOrder.push(number);
    sendJson(res, 201, issueJson(repo, issue), headers(ctx));
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}/issues', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const query = queryOf(req);
    const state = query.get('state') ?? 'open';
    const direction = query.get('direction') ?? 'desc';
    const sort = query.get('sort') ?? 'created';
    let numbers = [...repo.issueOrder];
    if (state !== 'all') numbers = numbers.filter((number) => repo.issues.get(number)!.state === state);
    if (sort === 'created') {
      numbers.sort((a, b) => (direction === 'asc' ? a - b : b - a));
    }
    sendList(ctx, req, res, numbers.map((number) => issueJson(repo, repo.issues.get(number)!)));
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}/issues/{number}', async (ctx, _req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const issue = repo.issues.get(Number(params.number));
    if (issue === undefined) {
      notFound(ctx, res);
      return;
    }
    sendJson(res, 200, issueJson(repo, issue), headers(ctx));
  }),
  route('PATCH', '/api/v3/repos/{owner}/{repo}/issues/{number}', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const issue = repo.issues.get(Number(params.number));
    if (issue === undefined) {
      notFound(ctx, res);
      return;
    }
    const body = await readJsonBody(ctx, req, res);
    if (body === 'bad') return;
    if (body !== null) {
      if (typeof body.title === 'string') issue.title = body.title;
      if (typeof body.body === 'string') issue.body = body.body;
      if (body.state === 'open' || body.state === 'closed') issue.state = body.state;
    }
    sendJson(res, 200, issueJson(repo, issue), headers(ctx));
  }),
  route('POST', '/api/v3/repos/{owner}/{repo}/issues/{number}/comments', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const issue = repo.issues.get(Number(params.number));
    if (issue === undefined) {
      notFound(ctx, res);
      return;
    }
    const body = await readJsonBody(ctx, req, res);
    if (body === 'bad' || body === null) {
      if (body === null) badRequest(ctx, res, 'body is required');
      return;
    }
    const text = typeof body.body === 'string' ? body.body : '';
    if (text === '') {
      unprocessable(ctx, res, 'body is required');
      return;
    }
    const comment = { id: repo.nextCommentId++, body: text, createdAt: FIXED_ISO, updatedAt: FIXED_ISO };
    issue.comments.push(comment);
    sendJson(res, 201, comment, headers(ctx));
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}/issues/{number}/comments', async (ctx, _req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const issue = repo.issues.get(Number(params.number));
    if (issue === undefined) {
      notFound(ctx, res);
      return;
    }
    sendJson(res, 200, issue.comments, headers(ctx));
  }),
  route('POST', '/api/v3/repos/{owner}/{repo}/pulls', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const body = await readJsonBody(ctx, req, res);
    if (body === 'bad' || body === null) {
      if (body === null) badRequest(ctx, res, 'title, head and base are required');
      return;
    }
    const title = typeof body.title === 'string' ? body.title : '';
    const head = typeof body.head === 'string' ? body.head : '';
    const base = typeof body.base === 'string' ? body.base : '';
    if (title === '' || head === '' || base === '') {
      unprocessable(ctx, res, 'title, head and base are required');
      return;
    }
    const headTip = branchTip(repo, head);
    const baseTip = branchTip(repo, base);
    if (headTip === null || baseTip === null) {
      unprocessable(ctx, res, 'head and base branches must exist');
      return;
    }
    for (const existing of repo.pulls.values()) {
      if (existing.state === 'open' && existing.headRef === head && existing.baseRef === base) {
        unprocessable(ctx, res, 'a pull request is already open');
        return;
      }
    }
    const number = repo.nextPullNumber++;
    const pull: PullRequest = {
      id: repo.id * 1_000_000 + number,
      number,
      title,
      body: typeof body.body === 'string' ? body.body : '',
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      headRef: head,
      baseRef: base,
      headSha: headTip,
      baseSha: baseTip,
      createdAt: FIXED_ISO,
      updatedAt: FIXED_ISO,
    };
    repo.pulls.set(number, pull);
    repo.pullOrder.push(number);
    sendJson(res, 201, pullJson(repo, pull), headers(ctx));
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}/pulls', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const query = queryOf(req);
    const state = query.get('state') ?? 'open';
    const numbers = repo.pullOrder.filter((number) => {
      const pull = repo.pulls.get(number)!;
      return state === 'all' ? true : pull.state === state;
    });
    sendList(ctx, req, res, numbers.map((number) => pullJson(repo, repo.pulls.get(number)!)));
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}/pulls/{number}', async (ctx, _req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const pull = repo.pulls.get(Number(params.number));
    if (pull === undefined) {
      notFound(ctx, res);
      return;
    }
    sendJson(res, 200, pullJson(repo, pull), headers(ctx));
  }),
  route('GET', '/api/v3/repos/{owner}/{repo}/pulls/{number}/files', async (ctx, _req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const pull = repo.pulls.get(Number(params.number));
    if (pull === undefined) {
      notFound(ctx, res);
      return;
    }
    sendJson(res, 200, pullFiles(repo, pull), headers(ctx));
  }),
  route('PUT', '/api/v3/repos/{owner}/{repo}/pulls/{number}/merge', async (ctx, req, res, params) => {
    const repo = resolveRepo(ctx, params.owner, params.repo);
    if (repo === null) {
      notFound(ctx, res);
      return;
    }
    const pull = repo.pulls.get(Number(params.number));
    if (pull === undefined) {
      notFound(ctx, res);
      return;
    }
    const body = await readJsonBody(ctx, req, res);
    if (body === 'bad') return;
    if (pull.merged) {
      sendJson(res, 200, { merged: true, sha: pull.mergeCommitSha, message: 'Pull request already merged' }, headers(ctx));
      return;
    }
    if (pullConflicts(repo, pull)) {
      sendJson(res, 405, { message: 'Merge conflict' }, headers(ctx));
      return;
    }
    const sha = squashMerge(repo, ctx, pull);
    pull.merged = true;
    pull.state = 'closed';
    pull.mergedAt = FIXED_ISO;
    pull.mergeCommitSha = sha;
    sendJson(res, 200, { merged: true, sha, message: pull.title }, headers(ctx));
  }),
];

function commitJson(repo: Repo, sha: string): Record<string, unknown> {
  const object = repo.objects.get(sha);
  if (object === undefined || object.type !== 'commit') return { sha, commit: { message: '' } };
  const commit = parseCommit(object.data);
  return { sha, commit: { message: commit.message } };
}

/** Answer one REST request. Returns true when the path was a REST route. */
export async function handleRest(ctx: RestContext, req: IncomingMessage, res: ServerResponse): Promise<boolean> {
  const path = pathOf(req);
  if (!path.startsWith('/api/v3')) return false;
  for (const entry of routes) {
    if (entry.method !== req.method) continue;
    const match = entry.pattern.exec(path);
    if (match === null) continue;
    const params: Record<string, string> = {};
    entry.keys.forEach((key, index) => {
      params[key] = decodeURIComponent(match[index + 1]);
    });
    await entry.handler(ctx, req, res, params);
    return true;
  }
  notFound(ctx, res);
  return true;
}
