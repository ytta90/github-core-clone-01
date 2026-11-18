// Entry lifecycle: spawn the Go forge binary, register its port with the env,
// poll /done until the trial is over, then exit 0.
//
//   1. MAKEFUN_ENV_URL names the env's origin. Without it there is nothing to
//      talk to, so the program exits 1 at once.
//   2. Spawn ./forge (the static Go core, in the same directory as this file).
//      It binds an ephemeral port on 127.0.0.1 and prints "listening on <port>".
//      Never bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address
//      the request came from plus N as the base URL. 200 is normal; 409 means
//      another entry already registered, and this process keeps waiting.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}.
//   5. Kill the forge child and exit 0.

import { spawn, type ChildProcess } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

/** How long one register or done request may take before it is abandoned and retried. */
const REQUEST_TIMEOUT_MS = 30_000;
/** The first wait between retries, and the longest. */
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** The wait after `attempt` failed requests: exponential from RETRY_INITIAL_MS, capped. */
function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

/**
 * POST the bound port until the env accepts it. 200 is the normal answer; 409 means a first register
 * already won, which is not this process's to fix. 400 and 405 are contract errors and end the program;
 * a network error or another status is retried, since the env may still be starting.
 */
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

/** Poll GET /done until it answers {"done":true}; never return before that. */
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = await res.json();
        if (body !== null && typeof body === 'object' && 'done' in body && body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

/** Spawn ./forge and resolve with its bound port once it prints "listening on <port>". */
function startForge(): Promise<{ child: ChildProcess; port: number }> {
  const dir = dirname(fileURLToPath(import.meta.url));
  const bin = join(dir, 'forge');
  const child = spawn(bin, [], { stdio: ['ignore', 'pipe', 'inherit'], env: process.env });
  return new Promise((resolve, reject) => {
    let buf = '';
    const onData = (chunk: Buffer): void => {
      buf += chunk.toString('utf8');
      const m = /listening on (\d+)/.exec(buf);
      if (m) {
        child.stdout?.off('data', onData);
        resolve({ child, port: Number(m[1]) });
      }
    };
    child.stdout?.on('data', onData);
    child.once('error', (err) => reject(err));
    child.once('exit', (code) => {
      if (code !== null) reject(new Error(`forge exited early with code ${code}`));
    });
  });
}

/** Spawn the forge, register it, then wait for the env: the whole life of the process. */
async function main(): Promise<void> {
  const { child, port } = await startForge();
  process.stdout.write(`[entry] listening on port ${port}\n`);

  await register(port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await waitForDone();
  child.kill('SIGTERM');
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
