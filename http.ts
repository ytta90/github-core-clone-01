// Shared HTTP plumbing for every surface of the forge: JSON replies, request ids,
// body reading and bearer auth. No surface imports app.ts (which would be a cycle);
// app.ts and the handlers all import this module.

import type { IncomingMessage, ServerResponse } from 'node:http';

export const JSON_TYPE = 'application/json; charset=utf-8';

let requestCounter = 0;

/** A deterministic per-request id: unique inside one run, no clock or randomness. */
export function nextRequestId(): string {
  requestCounter += 1;
  return `req-${String(requestCounter).padStart(6, '0')}`;
}

/** Answer one request with a JSON body; `headers` go beside the JSON ones. */
export function sendJson(
  res: ServerResponse,
  status: number,
  body: unknown,
  headers: Readonly<Record<string, string>> = {},
): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    ...headers,
  });
  res.end(text);
}

/** Answer with an empty body (204): no content, no JSON. */
export function sendEmpty(res: ServerResponse, status: number, headers: Readonly<Record<string, string>> = {}): void {
  res.writeHead(status, { ...headers });
  res.end();
}

/** Answer with raw bytes (a packfile sideband stream, a pkt-line stream). */
export function sendRaw(res: ServerResponse, status: number, body: Buffer, contentType: string): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': body.length });
  res.end(body);
}

/** The path of a request URL, or '/' when it does not parse (never a throw). */
export function pathOf(req: IncomingMessage): string {
  try {
    return new URL(req.url ?? '/', 'http://entry').pathname;
  } catch {
    return '/';
  }
}

/** The query of a request URL as URLSearchParams, or empty when it does not parse. */
export function queryOf(req: IncomingMessage): URLSearchParams {
  try {
    return new URL(req.url ?? '/', 'http://entry').searchParams;
  } catch {
    return new URLSearchParams();
  }
}

/** Read a request body whole, up to `maxBytes`. */
export function readBody(req: IncomingMessage, maxBytes: number): Promise<Buffer> {
  return new Promise<Buffer>((resolve, reject) => {
    const chunks: Buffer[] = [];
    let length = 0;
    let failed = false;
    req.on('data', (chunk: Buffer) => {
      if (failed) return;
      length += chunk.length;
      if (length > maxBytes) {
        failed = true;
        reject(new Error('request body too large'));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (!failed) resolve(Buffer.concat(chunks, length));
    });
    req.on('error', (err) => {
      if (!failed) {
        failed = true;
        reject(err);
      }
    });
  });
}

/** Parse a JSON body; null when the body is empty, a throw when it is malformed. */
export function parseJsonBody(body: Buffer): unknown {
  const text = body.toString('utf8').trim();
  if (text === '') return null;
  return JSON.parse(text) as unknown;
}

/** The bearer token a request carries, or null when it carries none. */
export function bearerOf(req: IncomingMessage): string | null {
  const header = req.headers.authorization;
  if (typeof header !== 'string') return null;
  if (!header.startsWith('Bearer ')) return null;
  return header.slice('Bearer '.length);
}

/** True when `value` is a plain record (an object, not an array, not null). */
export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
