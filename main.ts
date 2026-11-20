// Entry <-> env lifecycle for the Go forge: bind an ephemeral port, spawn the
// statically linked ./forge binary on it, register with the env, then poll
// until the env says the trial is over and exit 0.
//
// The forge binary does all the work (git smart HTTP, REST v3, GraphQL v4,
// MCP); this file is only the harness the starter's main.ts describes:
//   1. MAKEFUN_ENV_URL names the env's origin. Without it, exit 1 at once.
//   2. Pick a free loopback port (never 8080 — the env owns it).
//   3. Spawn ./forge with FORGE_PORT set, wait for /healthz to answer 200.
//   4. POST $MAKEFUN_ENV_URL/register {"port": N}. The first register wins;
//      a 409 means another entry already registered: keep waiting.
//   5. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}, then
//      stop the forge and exit 0.

import { spawn, type ChildProcess } from 'node:child_process';
import { createServer as createNetServer } from 'node:net';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

/** A free loopback port: bind port 0, read the port, close. */
function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = createNetServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      const port = typeof address === 'object' && address !== null ? address.port : 0;
      server.close(() => resolve(port));
    });
  });
}

async function requestJson(url: string, init: RequestInit, timeoutMs: number): Promise<{ status: number; body: unknown }> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const response = await fetch(url, { ...init, signal: controller.signal });
    const text = await response.text();
    let body: unknown = null;
    try {
      body = JSON.parse(text) as unknown;
    } catch {
      body = null;
    }
    return { status: response.status, body };
  } finally {
    clearTimeout(timer);
  }
}

async function waitHealthz(port: number, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let attempt = 0;
  for (;;) {
    try {
      const { status } = await requestJson(`http://127.0.0.1:${port}/healthz`, {}, 2_000);
      if (status === 200) return;
    } catch {
      // Not up yet.
    }
    if (Date.now() > deadline) throw new Error(`forge did not answer /healthz within ${timeoutMs} ms`);
    await sleep(retryDelay(attempt));
    attempt += 1;
  }
}

async function register(port: number): Promise<void> {
  let attempt = 0;
  for (;;) {
    try {
      const { status } = await requestJson(
        `${ENV_URL}/register`,
        {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ port }),
        },
        REQUEST_TIMEOUT_MS,
      );
      if (status === 200) return;
      if (status === 409) {
        process.stderr.write('[entry] register: another entry already registered, waiting\n');
      } else {
        process.stderr.write(`[entry] register: HTTP ${status}, retrying\n`);
      }
    } catch (err) {
      process.stderr.write(`[entry] register: ${err instanceof Error ? err.message : String(err)}, retrying\n`);
    }
    await sleep(retryDelay(attempt));
    attempt += 1;
  }
}

async function waitDone(): Promise<void> {
  let attempt = 0;
  for (;;) {
    try {
      const { status, body } = await requestJson(`${ENV_URL}/done`, {}, REQUEST_TIMEOUT_MS);
      if (status === 200 && typeof body === 'object' && body !== null && (body as { done?: unknown }).done === true) {
        return;
      }
    } catch (err) {
      process.stderr.write(`[entry] done poll: ${err instanceof Error ? err.message : String(err)}, retrying\n`);
    }
    await sleep(retryDelay(attempt));
    attempt += 1;
  }
}

async function main(): Promise<number> {
  const dir = dirname(fileURLToPath(import.meta.url));
  const binary = join(dir, 'forge');
  let port = 0;
  let forge: ChildProcess | null = null;
  // The forge may fail to bind if the port was taken between picking and
  // spawning; retry a few times with fresh ports.
  for (let attempt = 0; attempt < 5; attempt += 1) {
    port = await freePort();
    forge = spawn(binary, [], {
      env: { ...process.env, FORGE_PORT: String(port) },
      stdio: ['ignore', 'inherit', 'inherit'],
    });
    try {
      await waitHealthz(port, 10_000);
      break;
    } catch {
      forge.kill('SIGKILL');
      forge = null;
      if (attempt === 4) throw new Error('forge did not start after 5 attempts');
    }
  }
  if (forge === null) return 1;
  const child = forge;
  let done = false;
  const stop = (): void => {
    if (done) return;
    done = true;
    try {
      child.kill('SIGTERM');
    } catch {
      // Already gone.
    }
    setTimeout(() => {
      try {
        child.kill('SIGKILL');
      } catch {
        // Already gone.
      }
    }, 2_000).unref();
  };
  process.on('SIGTERM', stop);
  process.on('SIGINT', stop);
  await register(port);
  await waitDone();
  stop();
  return 0;
}

main()
  .then((code) => process.exit(code))
  .catch((err: unknown) => {
    process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
    process.exit(1);
  });
