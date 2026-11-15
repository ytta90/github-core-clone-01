// MCP over POST /mcp: streamable HTTP, the modern 2026-07-28 era.
//
// The client speaks JSON-RPC 2.0 over plain JSON bodies (no SSE needed here).
// `server/discover` announces the era and the server; `initialize` echoes the
// offered protocol revision back; `notifications/initialized` is a no-id
// notification answered with 202 and nothing else; `tools/list` returns the
// same six tools in the same order on every call; `tools/call` runs them.
// A malformed body is HTTP 400 with JSON-RPC -32700 (the id is pulled out of
// the raw text with a regex when it is there). An unknown tool is -32601 on
// HTTP 200, never a 500. A missing required argument is a tool result with
// isError, not a JSON-RPC error. Every response echoes the request's id, so
// concurrent calls keep their own answers.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { FIXED_ISO } from './store.ts';
import type { Issue, PullRequest, Repo, Store } from './store.ts';
import { isRecord, parseJsonBody, readBody, sendJson } from './http.ts';
import { pullConflicts, squashMerge } from './rest.ts';

const MAX_BODY = 16 * 1024 * 1024;
const SERVER_NAME = 'git-forge';
const SERVER_VERSION = '1.0.0';
const PROTOCOL_VERSION = '2026-07-28';

interface ToolDefinition {
  readonly name: string;
  readonly description: string;
  readonly inputSchema: Record<string, unknown>;
}

const TOOLS: readonly ToolDefinition[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Read one repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Open an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'open, closed or all. Defaults to open.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch onto a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request onto its base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

interface McpContext {
  readonly store: Store;
  readonly login: string;
}

type ToolResult = {
  content: Array<{ type: 'text'; text: string }>;
  structuredContent?: Record<string, unknown>;
  isError?: boolean;
};

function toolOk(value: Record<string, unknown>): ToolResult {
  const text = JSON.stringify(value);
  return { content: [{ type: 'text', text }], structuredContent: value };
}

function toolError(message: string): ToolResult {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
  };
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels,
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
    head: { ref: pull.headRef, sha: pull.headSha },
    base: { ref: pull.baseRef, sha: pull.baseSha },
  };
}

/** The repo a tool call names, or null when the owner or the repo is wrong. */
function resolveRepo(ctx: McpContext, args: Record<string, unknown>): Repo | null {
  const owner = typeof args.owner === 'string' ? args.owner : '';
  const repoName = typeof args.repo === 'string' ? args.repo : '';
  if (owner !== ctx.login) return null;
  return ctx.store.getRepo(owner, repoName) ?? null;
}

function missingArg(args: Record<string, unknown>, name: string): ToolResult | null {
  const value = args[name];
  if (value === undefined || value === null || value === '') {
    return toolError(`Missing required argument: ${name}`);
  }
  return null;
}

const TOOL_HANDLERS: Readonly<Record<string, (ctx: McpContext, args: Record<string, unknown>) => ToolResult>> = {
  create_repo: (ctx, args) => {
    const missing = missingArg(args, 'name');
    if (missing !== null) return missing;
    const name = String(args.name);
    const repo = ctx.store.createRepo(
      ctx.login,
      name,
      args.private === true,
      typeof args.description === 'string' ? args.description : '',
    );
    if (repo === null) return toolError(`a repo named ${name} already exists`);
    return toolOk(repoJson(repo));
  },
  get_repo: (ctx, args) => {
    for (const name of ['owner', 'repo']) {
      const missing = missingArg(args, name);
      if (missing !== null) return missing;
    }
    const repo = resolveRepo(ctx, args);
    if (repo === null) return toolError('repository not found');
    return toolOk(repoJson(repo));
  },
  create_issue: (ctx, args) => {
    for (const name of ['owner', 'repo', 'title']) {
      const missing = missingArg(args, name);
      if (missing !== null) return missing;
    }
    const repo = resolveRepo(ctx, args);
    if (repo === null) return toolError('repository not found');
    const number = repo.nextIssueNumber++;
    const issue: Issue = {
      id: repo.id * 1_000_000 + number,
      number,
      title: String(args.title),
      body: typeof args.body === 'string' ? args.body : '',
      state: 'open',
      labels: [],
      comments: [],
      createdAt: FIXED_ISO,
      updatedAt: FIXED_ISO,
    };
    repo.issues.set(number, issue);
    repo.issueOrder.push(number);
    return toolOk(issueJson(repo, issue));
  },
  list_issues: (ctx, args) => {
    for (const name of ['owner', 'repo']) {
      const missing = missingArg(args, name);
      if (missing !== null) return missing;
    }
    const repo = resolveRepo(ctx, args);
    if (repo === null) return toolError('repository not found');
    const state = typeof args.state === 'string' ? args.state : 'open';
    const issues = repo.issueOrder
      .map((number) => repo.issues.get(number)!)
      .filter((issue) => (state === 'all' ? true : issue.state === state))
      .map((issue) => issueJson(repo, issue));
    return toolOk({ issues });
  },
  create_pull_request: (ctx, args) => {
    for (const name of ['owner', 'repo', 'title', 'head', 'base']) {
      const missing = missingArg(args, name);
      if (missing !== null) return missing;
    }
    const repo = resolveRepo(ctx, args);
    if (repo === null) return toolError('repository not found');
    const head = String(args.head);
    const base = String(args.base);
    const headTip = repo.refs.get(`refs/heads/${head}`) ?? null;
    const baseTip = repo.refs.get(`refs/heads/${base}`) ?? null;
    if (headTip === null || baseTip === null) return toolError('head and base branches must exist');
    for (const existing of repo.pulls.values()) {
      if (existing.state === 'open' && existing.headRef === head && existing.baseRef === base) {
        return toolError('a pull request is already open');
      }
    }
    const number = repo.nextPullNumber++;
    const pull: PullRequest = {
      id: repo.id * 1_000_000 + number,
      number,
      title: String(args.title),
      body: typeof args.body === 'string' ? args.body : '',
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
    return toolOk(pullJson(repo, pull));
  },
  merge_pull_request: (ctx, args) => {
    for (const name of ['owner', 'repo', 'pull_number']) {
      const missing = missingArg(args, name);
      if (missing !== null) return missing;
    }
    const repo = resolveRepo(ctx, args);
    if (repo === null) return toolError('repository not found');
    const pull = repo.pulls.get(Number(args.pull_number));
    if (pull === undefined) return toolError('pull request not found');
    if (pull.merged) return toolOk({ merged: true, sha: pull.mergeCommitSha, message: 'Pull request already merged' });
    if (pullConflicts(repo, pull)) return toolError('merge conflict');
    const sha = squashMerge(repo, ctx, pull);
    pull.merged = true;
    pull.state = 'closed';
    pull.mergedAt = FIXED_ISO;
    pull.mergeCommitSha = sha;
    return toolOk({ merged: true, sha, message: pull.title });
  },
};

function jsonRpcResult(id: unknown, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, result };
}

function jsonRpcError(id: unknown, code: number, message: string): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, error: { code, message } };
}

/** Pull an id out of raw request text (for -32700 replies when JSON is broken). */
function extractId(raw: string): unknown {
  const match = /"id"\s*:\s*("[^"]*"|-?\d+|null)/.exec(raw);
  if (match === null) return null;
  try {
    return JSON.parse(match[1]) as unknown;
  } catch {
    return null;
  }
}

/** Answer one JSON-RPC request object. Returns null for a notification (202). */
function handleRequest(ctx: McpContext, request: unknown): Record<string, unknown> | null {
  if (!isRecord(request)) return jsonRpcError(null, -32600, 'Invalid Request');
  const hasId = Object.prototype.hasOwnProperty.call(request, 'id');
  if (!hasId) return null;
  const id = request.id ?? null;
  const method = typeof request.method === 'string' ? request.method : '';
  const params = isRecord(request.params) ? (request.params as Record<string, unknown>) : {};
  if (method === 'server/discover') {
    return jsonRpcResult(id, {
      supportedVersions: [PROTOCOL_VERSION, '2025-06-18', '2025-03-26'],
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
    });
  }
  if (method === 'initialize') {
    const offered = typeof params.protocolVersion === 'string' ? params.protocolVersion : PROTOCOL_VERSION;
    return jsonRpcResult(id, {
      protocolVersion: offered,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    });
  }
  if (method === 'tools/list') {
    return jsonRpcResult(id, { tools: TOOLS });
  }
  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const handler = TOOL_HANDLERS[name];
    if (handler === undefined) {
      return jsonRpcError(id, -32601, `unknown tool ${name}`);
    }
    const args = isRecord(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
    try {
      return jsonRpcResult(id, handler(ctx, args));
    } catch (error) {
      return jsonRpcError(id, -32603, error instanceof Error ? error.message : 'the tool failed');
    }
  }
  if (method === 'ping') {
    return jsonRpcResult(id, {});
  }
  if (method.startsWith('notifications/')) {
    return null;
  }
  return jsonRpcError(id, -32601, `unknown method ${method}`);
}

/** Answer one POST /mcp request. Auth is checked by the router before this runs. */
export async function handleMcp(store: Store, login: string, req: IncomingMessage, res: ServerResponse): Promise<void> {
  let body: Buffer;
  try {
    body = await readBody(req, MAX_BODY);
  } catch {
    sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
    return;
  }
  const raw = body.toString('utf8');
  let parsed: unknown;
  try {
    parsed = parseJsonBody(body);
  } catch {
    sendJson(res, 400, { jsonrpc: '2.0', id: extractId(raw), error: { code: -32700, message: 'Parse error' } });
    return;
  }
  const ctx: McpContext = { store, login };
  if (Array.isArray(parsed)) {
    const responses: Array<Record<string, unknown>> = [];
    for (const request of parsed) {
      const response = handleRequest(ctx, request);
      if (response !== null) responses.push(response);
    }
    if (responses.length === 0) {
      res.writeHead(202);
      res.end();
      return;
    }
    sendJson(res, 200, responses);
    return;
  }
  const response = handleRequest(ctx, parsed);
  if (response === null) {
    res.writeHead(202);
    res.end();
    return;
  }
  sendJson(res, 200, response);
}
