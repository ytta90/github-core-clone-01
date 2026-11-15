// The forge's HTTP server: routing and auth. The surfaces live in their own
// modules (store.ts, git.ts, rest.ts, graphql.ts, mcp.ts); this file wires
// them together and owns the discipline every surface shares: every reply is
// application/json, every reply carries a non-empty X-GitHub-Request-Id, an
// unknown route is a JSON 404, a malformed JSON body is a 400, and a handler
// that throws answers a 500 instead of taking the server down. Only GET
// /healthz answers without a bearer token.
//
// The login the token names is the scenario's viewer (scenario.ts): the
// harness learns it from GET /api/v3/user and uses it for every repo it
// creates, so the server and the harness can never disagree about it.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { createStore, type Store } from './store.ts';
import { VIEWER_LOGIN } from './scenario.ts';
import { gitPathParts, handleGit, isGitPath } from './git.ts';
import { handleRest } from './rest.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { bearerOf, nextRequestId, pathOf, sendJson } from './http.ts';

/** The bearer token this process answers to (MAKEFUN_SHOP_TOKEN in a trial). */
export function tokenOf(env: Record<string, string | undefined>): string {
  return env.MAKEFUN_SHOP_TOKEN ?? '';
}

/** Build the server. It binds nothing until the caller listens. */
export function createApp(): Server {
  // The login the token names comes from the contest's fixed scenario seed
  // (scenario.ts); the token itself is opaque and only gates access.
  const store: Store = createStore(VIEWER_LOGIN);
  const token = tokenOf(process.env);
  const login = store.viewerLogin;

  const onRequest = (req: IncomingMessage, res: ServerResponse): void => {
    const requestId = nextRequestId();
    const path = pathOf(req);
    const method = req.method ?? 'GET';
    const extra: Record<string, string> = { 'x-github-request-id': requestId };

    const finish = async (): Promise<void> => {
      // The env's boot check calls this twice: 200 {"ok":true}, no token needed.
      if (method === 'GET' && path === '/healthz') {
        sendJson(res, 200, { ok: true }, extra);
        return;
      }

      // Everything else needs the bearer token.
      const bearer = bearerOf(req);
      if (token === '' || bearer === null || bearer !== token) {
        sendJson(res, 401, { message: 'Bad credentials' }, extra);
        return;
      }

      // Git smart HTTP: the repo must exist under the token's login.
      if (isGitPath(path)) {
        const parts = gitPathParts(path);
        if (parts === null || parts.owner !== login) {
          sendJson(res, 404, { message: 'Not Found' }, extra);
          return;
        }
        const repo = store.getRepo(parts.owner, parts.repo);
        if (repo === undefined) {
          sendJson(res, 404, { message: 'Not Found' }, extra);
          return;
        }
        await handleGit(req, res, repo);
        return;
      }

      // GraphQL.
      if (method === 'POST' && path === '/api/graphql') {
        await handleGraphql(store, login, req, res);
        return;
      }

      // MCP.
      if (method === 'POST' && path === '/mcp') {
        await handleMcp(store, login, req, res);
        return;
      }

      // REST v3.
      if (path.startsWith('/api/v3')) {
        const handled = await handleRest({ store, login, requestId }, req, res);
        if (handled) return;
      }

      // Everything else, every method: GitHub's 404 body.
      sendJson(res, 404, { message: 'Not Found' }, extra);
    };

    finish().catch((error: unknown) => {
      if (!res.headersSent) {
        sendJson(res, 500, { message: error instanceof Error ? error.message : 'internal error' }, extra);
      }
    });
  };

  return createServer(onRequest);
}
