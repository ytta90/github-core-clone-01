// Git over smart HTTP: protocol v2 upload-pack, v0 receive-pack, ls-refs and fetch.
//
// The wire contract, in one place:
// - info/refs?service=git-upload-pack: v2 when the client sends
//   "Git-Protocol: version=2", v0 otherwise. v2 opens with the "# service"
//   preamble, a flush, "version 2", the capability lines, a flush. v0 opens
//   with the preamble, a flush, then ref lines (the first carrying the caps
//   after a NUL), then a flush.
// - info/refs?service=git-receive-pack: v0 only, same shape as v0 above, with
//   the receive-pack caps (report-status, side-band-64k, delete-refs).
// - POST git-upload-pack with "Git-Protocol: version=2": a ls-refs or a fetch
//   command, answered in sections (acknowledgments, shallow-info, packfile),
//   each headed by one pkt-line and closed by a flush.
// - POST git-receive-pack: v0 only. Command pkt-lines, a flush, then the pack.
//   Answered with a sideband report: "unpack ok" plus one ok/ng line per ref.
//   A refused update still unpacks and still leaves the ref where it was.
// - A missing repo is a 404 on info/refs for either service, and the server
//   stays up. A body that is not pkt-lines is a 400 JSON reply.

import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  PKT_DELIM,
  PKT_FLUSH,
  decodePktLines,
  isObjectId,
  pktLine,
  readPack,
  verifyPackChecksum,
  writePack,
  ZERO_OID,
  collectReachable,
  isAncestor,
  peelToCommit,
  parseCommit,
  parseTag,
  parseTree,
  putObject,
} from './store.ts';
import type { GitObject, Repo } from './store.ts';
import { readBody, sendJson, sendRaw } from './http.ts';

const UPLOAD_AD_CONTENT = 'application/x-git-upload-pack-advertisement';
const RECEIVE_AD_CONTENT = 'application/x-git-receive-pack-advertisement';
const UPLOAD_RESULT_CONTENT = 'application/x-git-upload-pack-result';
const RECEIVE_RESULT_CONTENT = 'application/x-git-receive-pack-result';
const AGENT = 'git-forge/1.0';
const MAX_BODY = 64 * 1024 * 1024;

/** The ref names a repo advertises, in advertisement order. */
function refNames(repo: Repo): string[] {
  const heads = [...repo.refs.keys()].filter((ref) => ref.startsWith('refs/heads/')).sort();
  const tags = [...repo.refs.keys()].filter((ref) => ref.startsWith('refs/tags/')).sort();
  const other = [...repo.refs.keys()]
    .filter((ref) => !ref.startsWith('refs/heads/') && !ref.startsWith('refs/tags/'))
    .sort();
  return [...heads, ...tags, ...other];
}

/** The tip of the default branch, or null for an unborn repo. */
function headTip(repo: Repo): string | null {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
}

/** The peeled commit of a ref (an annotated tag peels to its target commit). */
function peeledOf(repo: Repo, ref: string): string | null {
  const tip = repo.refs.get(ref);
  if (tip === undefined) return null;
  const object = repo.objects.get(tip);
  if (object === undefined) return null;
  if (object.type === 'commit') return tip;
  if (object.type === 'tag') return peelToCommit(repo.objects, tip);
  return null;
}

function uploadPackV0Caps(): string {
  return `symref=HEAD:refs/heads/${'main'} side-band-64k ofs-delta agent=${AGENT}`;
}

function receivePackV0Caps(): string {
  return `report-status side-band-64k delete-refs ofs-delta agent=${AGENT}`;
}

/** v0 ref lines: the first line carries the caps after a NUL byte. */
function v0RefLines(repo: Repo, caps: string): Buffer[] {
  const parts: Buffer[] = [];
  const names = refNames(repo);
  const tip = headTip(repo);
  if (names.length === 0 || tip === null) {
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
    parts.push(PKT_FLUSH);
    return parts;
  }
  const head = `${tip} HEAD\0${caps}\n`;
  parts.push(pktLine(head));
  for (const ref of names) {
    const oid = repo.refs.get(ref)!;
    parts.push(pktLine(`${oid} ${ref}\n`));
    const peeled = peeledOf(repo, ref);
    if (peeled !== null && peeled !== oid) parts.push(pktLine(`${peeled} ${ref}^{}\n`));
  }
  parts.push(PKT_FLUSH);
  return parts;
}

function advertiseUploadPackV0(repo: Repo): Buffer {
  return Buffer.concat([
    pktLine('# service=git-upload-pack\n'),
    PKT_FLUSH,
    ...v0RefLines(repo, uploadPackV0Caps()),
  ]);
}

function advertiseReceivePackV0(repo: Repo): Buffer {
  return Buffer.concat([
    pktLine('# service=git-receive-pack\n'),
    PKT_FLUSH,
    ...v0RefLines(repo, receivePackV0Caps()),
  ]);
}

function advertiseUploadPackV2(repo: Repo): Buffer {
  return Buffer.concat([
    pktLine('# service=git-upload-pack\n'),
    PKT_FLUSH,
    pktLine('version 2\n'),
    pktLine(`agent=${AGENT}\n`),
    pktLine('ls-refs=unborn\n'),
    pktLine('fetch=shallow wait-for-done\n'),
    pktLine('object-format=sha1\n'),
    PKT_FLUSH,
  ]);
}

// ---------------------------------------------------------------------------------------------
// ls-refs (protocol v2)
// ---------------------------------------------------------------------------------------------

interface LsRefsArgs {
  readonly peel: boolean;
  readonly symrefs: boolean;
  readonly unborn: boolean;
  readonly prefixes: string[];
}

function handleLsRefs(repo: Repo, args: LsRefsArgs): Buffer {
  const parts: Buffer[] = [];
  const tip = headTip(repo);
  const wanted = (ref: string): boolean =>
    args.prefixes.length === 0 || args.prefixes.some((prefix) => ref.startsWith(prefix));
  if (tip === null) {
    if (args.unborn && wanted('HEAD')) {
      parts.push(pktLine(`unborn HEAD symref-target:refs/heads/${repo.defaultBranch}\n`));
    }
    parts.push(PKT_FLUSH);
    return Buffer.concat(parts);
  }
  if (args.prefixes.length === 0) {
    let head = `${tip} HEAD`;
    if (args.symrefs) head += ` symref-target:refs/heads/${repo.defaultBranch}`;
    if (args.peel) head += ` peeled:${tip}`;
    parts.push(pktLine(`${head}\n`));
  }
  for (const ref of refNames(repo)) {
    if (!wanted(ref)) continue;
    const oid = repo.refs.get(ref)!;
    let line = `${oid} ${ref}`;
    const object = repo.objects.get(oid);
    if (args.peel && object !== undefined && object.type === 'tag') {
      const peeled = peelToCommit(repo.objects, oid);
      if (peeled !== null) line += ` peeled:${peeled}`;
    }
    parts.push(pktLine(`${line}\n`));
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------------------------
// fetch (protocol v2)
// ---------------------------------------------------------------------------------------------

interface FetchArgs {
  readonly wants: string[];
  readonly haves: string[];
  readonly shallows: string[];
  readonly deepen: number | null;
}

/** Commits within `depth` generations of a want (the want itself is generation 1). */
function depthLimitedCommits(repo: Repo, wants: readonly string[], depth: number): Set<string> {
  const out = new Set<string>();
  const queue: Array<{ oid: string; generation: number }> = wants.map((oid) => ({ oid, generation: 1 }));
  const seen = new Set<string>();
  while (queue.length > 0) {
    const { oid, generation } = queue.shift()!;
    if (seen.has(oid) || generation > depth) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    out.add(oid);
    for (const parent of parseCommit(object.data).parents) {
      queue.push({ oid: parent, generation: generation + 1 });
    }
  }
  return out;
}

/** Every object reachable from `haves`, without walking past a shallow boundary. */
function haveReachable(repo: Repo, haves: readonly string[], shallows: readonly string[]): Set<string> {
  const shallowSet = new Set(shallows);
  const out = new Set<string>();
  const queue: string[] = [...haves];
  while (queue.length > 0) {
    const oid = queue.pop()!;
    if (out.has(oid)) continue;
    const object = repo.objects.get(oid);
    if (object === undefined) continue;
    out.add(oid);
    if (object.type === 'commit') {
      if (shallowSet.has(oid)) continue;
      const commit = parseCommit(object.data);
      queue.push(commit.tree);
      for (const parent of commit.parents) queue.push(parent);
    } else if (object.type === 'tag') {
      queue.push(parseTag(object.data).object);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) {
        if (entry.mode !== '160000') queue.push(entry.oid);
      }
    }
  }
  return out;
}

/**
 * Every object a depth-limited fetch sends: the included commits plus the
 * trees and blobs reachable from them, without ever following a parent link.
 * Following parents would walk the whole history past the depth limit.
 */
function depthLimitedReachable(repo: Repo, wants: readonly string[], depth: number): Set<string> {
  const commits = depthLimitedCommits(repo, wants, depth);
  const out = new Set<string>();
  const queue: string[] = [...commits];
  while (queue.length > 0) {
    const oid = queue.pop()!;
    if (out.has(oid)) continue;
    const object = repo.objects.get(oid);
    if (object === undefined) continue;
    out.add(oid);
    if (object.type === 'commit') {
      queue.push(parseCommit(object.data).tree);
    } else if (object.type === 'tag') {
      queue.push(parseTag(object.data).object);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) {
        if (entry.mode !== '160000') queue.push(entry.oid);
      }
    }
  }
  return out;
}

function handleFetch(repo: Repo, args: FetchArgs): Buffer {
  const wantReachable = args.deepen === null
    ? collectReachable(args.wants, (oid) => repo.objects.get(oid), { allowMissing: true })
    : depthLimitedReachable(repo, args.wants, args.deepen);
  const haveSet = haveReachable(repo, args.haves, args.shallows);
  const toSend = new Set<string>();
  for (const oid of wantReachable) if (!haveSet.has(oid)) toSend.add(oid);

  const parts: Buffer[] = [];
  // acknowledgments: the header line, then its content lines, then a flush.
  // (A flush between the header and the content would end the section.)
  parts.push(pktLine('acknowledgments\n'));
  if (args.haves.length === 0) {
    parts.push(pktLine('NAK\n'));
  } else {
    let acknowledged = 0;
    for (const have of args.haves) {
      if (repo.objects.has(have)) {
        parts.push(pktLine(`ACK ${have}\n`));
        acknowledged += 1;
      }
    }
    if (acknowledged === 0) parts.push(pktLine('NAK\n'));
    else parts.push(pktLine('ready\n'));
  }
  parts.push(PKT_FLUSH);
  // shallow-info
  const shallowLines: string[] = [];
  const unshallowLines: string[] = [];
  if (args.deepen !== null || args.shallows.length > 0) {
    const sentCommits = new Set<string>();
    for (const oid of toSend) {
      const object = repo.objects.get(oid);
      if (object !== undefined && object.type === 'commit') sentCommits.add(oid);
    }
    const complete = new Set<string>([...sentCommits, ...haveSet]);
    for (const oid of sentCommits) {
      const object = repo.objects.get(oid)!;
      const parents = parseCommit(object.data).parents;
      if (parents.some((parent) => !complete.has(parent))) shallowLines.push(`shallow ${oid}`);
    }
    for (const shallow of args.shallows) {
      const object = repo.objects.get(shallow);
      if (object === undefined || object.type !== 'commit') continue;
      const parents = parseCommit(object.data).parents;
      if (parents.every((parent) => complete.has(parent))) unshallowLines.push(`unshallow ${shallow}`);
    }
    if (shallowLines.length > 0 || unshallowLines.length > 0) {
      parts.push(pktLine('shallow-info\n'));
      for (const line of [...shallowLines, ...unshallowLines]) parts.push(pktLine(`${line}\n`));
      parts.push(PKT_FLUSH);
    }
  }
  // packfile (omitted entirely when there is nothing to send)
  if (toSend.size > 0) {
    const objects: GitObject[] = [];
    for (const oid of toSend) {
      const object = repo.objects.get(oid);
      if (object !== undefined) objects.push(object);
    }
    const pack = writePack(objects);
    parts.push(pktLine('packfile\n'));
    const CHUNK = 65515;
    for (let offset = 0; offset < pack.length; offset += CHUNK) {
      const slice = pack.subarray(offset, Math.min(pack.length, offset + CHUNK));
      const banded = Buffer.concat([Buffer.from([1]), slice]);
      parts.push(pktLine(banded));
    }
    parts.push(PKT_FLUSH);
  }
  return Buffer.concat(parts);
}

/** Parse a v2 upload-pack request body into its command and arguments. */
function parseV2Request(body: Buffer): { command: string; args: string[] } {
  const lines = decodePktLines(body);
  const commandLines: string[] = [];
  const args: string[] = [];
  let pastDelim = false;
  for (const line of lines) {
    if (line === 'flush') break;
    if (line === 'delim') {
      pastDelim = true;
      continue;
    }
    const text = line.toString('utf8');
    if (pastDelim) args.push(text);
    else commandLines.push(text);
  }
  let command = '';
  for (const line of commandLines) {
    if (line.startsWith('command=')) command = line.slice('command='.length).trim();
  }
  return { command, args };
}

function parseLsRefsArgs(args: readonly string[]): LsRefsArgs {
  const prefixes: string[] = [];
  let peel = false;
  let symrefs = false;
  let unborn = false;
  for (const arg of args) {
    const text = arg.trim();
    if (text === 'peel') peel = true;
    else if (text === 'symrefs') symrefs = true;
    else if (text === 'unborn') unborn = true;
    else if (text.startsWith('ref-prefix ')) prefixes.push(text.slice('ref-prefix '.length));
  }
  return { peel, symrefs, unborn, prefixes };
}

function parseFetchArgs(args: readonly string[]): FetchArgs {
  const wants: string[] = [];
  const haves: string[] = [];
  const shallows: string[] = [];
  let deepen: number | null = null;
  for (const arg of args) {
    const text = arg.trim();
    if (text.startsWith('want ')) wants.push(text.slice('want '.length));
    else if (text.startsWith('have ')) haves.push(text.slice('have '.length));
    else if (text.startsWith('shallow ')) shallows.push(text.slice('shallow '.length));
    else if (text.startsWith('deepen ')) deepen = Number.parseInt(text.slice('deepen '.length), 10);
  }
  return { wants, haves, shallows, deepen };
}

// ---------------------------------------------------------------------------------------------
// receive-pack (v0)
// ---------------------------------------------------------------------------------------------

interface PushCommand {
  readonly oldOid: string;
  readonly newOid: string;
  readonly ref: string;
}

function handleReceivePack(repo: Repo, body: Buffer): Buffer {
  const commands: PushCommand[] = [];
  let packOffset = -1;
  // The command section ends at the first flush packet; the pack follows it.
  let offset = 0;
  let first = true;
  while (offset + 4 <= body.length) {
    const header = body.subarray(offset, offset + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(header)) throw new Error('invalid pkt-line length in receive-pack');
    const length = Number.parseInt(header, 16);
    if (length === 0) {
      packOffset = offset + 4;
      break;
    }
    if (length < 4 || offset + length > body.length) throw new Error('truncated pkt-line in receive-pack');
    let text = body.subarray(offset + 4, offset + length).toString('utf8');
    if (first) {
      const nul = text.indexOf('\0');
      if (nul >= 0) text = text.slice(0, nul);
      first = false;
    }
    const match = /^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)\n?$/.exec(text.trim());
    if (match === null) throw new Error(`malformed push command ${JSON.stringify(text)}`);
    commands.push({ oldOid: match[1], newOid: match[2], ref: match[3] });
    offset += length;
  }
  if (packOffset < 0) throw new Error('receive-pack has no flush after its commands');

  const report: string[] = [];
  const packBytes = body.subarray(packOffset);
  if (packBytes.length > 0) {
    if (packBytes.length < 32 || packBytes.subarray(0, 4).toString('ascii') !== 'PACK' || !verifyPackChecksum(packBytes)) {
      report.push('unpack error');
      return sidebandReport(report);
    }
    let objects: Map<string, GitObject>;
    try {
      objects = readPack(packBytes, (oid) => repo.objects.get(oid));
    } catch {
      report.push('unpack error');
      return sidebandReport(report);
    }
    for (const [oid, object] of objects) {
      if (!repo.objects.has(oid)) repo.objects.set(oid, object);
    }
  }
  report.push('unpack ok');
  for (const command of commands) {
    const current = repo.refs.get(command.ref) ?? null;
    if (command.newOid === ZERO_OID) {
      // delete
      if (current === null) {
        report.push(`ng ${command.ref} ref does not exist`);
      } else if (command.oldOid !== ZERO_OID && current !== command.oldOid) {
        report.push(`ng ${command.ref} non-fast-forward`);
      } else {
        repo.refs.delete(command.ref);
        report.push(`ok ${command.ref}`);
      }
      continue;
    }
    if (!repo.objects.has(command.newOid)) {
      report.push(`ng ${command.ref} object not found`);
      continue;
    }
    if (command.oldOid === ZERO_OID) {
      if (current !== null) {
        report.push(`ng ${command.ref} non-fast-forward`);
        continue;
      }
    } else {
      if (current === null || current !== command.oldOid || !isAncestor(repo.objects, command.oldOid, command.newOid)) {
        report.push(`ng ${command.ref} non-fast-forward`);
        continue;
      }
    }
    repo.refs.set(command.ref, command.newOid);
    report.push(`ok ${command.ref}`);
  }
  return sidebandReport(report);
}

function sidebandReport(lines: readonly string[]): Buffer {
  const parts: Buffer[] = [];
  for (const line of lines) {
    parts.push(pktLine(Buffer.concat([Buffer.from([1]), Buffer.from(`${line}\n`, 'utf8')])));
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------------------------
// entry point
// ---------------------------------------------------------------------------------------------

/**
 * Answer one git smart HTTP request. `repo` is already resolved (a 404 was sent
 * when it did not exist). Returns true when the request was a git request.
 */
export async function handleGit(req: IncomingMessage, res: ServerResponse, repo: Repo): Promise<boolean> {
  const url = new URL(req.url ?? '/', 'http://entry');
  const path = url.pathname;
  const match = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(path);
  if (match === null) return false;
  const service = url.searchParams.get('service');
  const isV2 = (req.headers['git-protocol'] ?? '') === 'version=2';
  if (req.method === 'GET' && match[3] === 'info/refs') {
    if (service === 'git-upload-pack') {
      sendRaw(res, 200, isV2 ? advertiseUploadPackV2(repo) : advertiseUploadPackV0(repo), UPLOAD_AD_CONTENT);
      return true;
    }
    if (service === 'git-receive-pack') {
      sendRaw(res, 200, advertiseReceivePackV0(repo), RECEIVE_AD_CONTENT);
      return true;
    }
    sendJson(res, 404, { message: 'unknown service' });
    return true;
  }
  if (req.method === 'POST' && match[3] === 'git-upload-pack') {
    let body: Buffer;
    try {
      body = await readBody(req, MAX_BODY);
    } catch {
      sendJson(res, 400, { message: 'request body too large' });
      return true;
    }
    try {
      if (isV2) {
        const { command, args } = parseV2Request(body);
        if (command === 'ls-refs') {
          sendRaw(res, 200, handleLsRefs(repo, parseLsRefsArgs(args)), UPLOAD_RESULT_CONTENT);
        } else if (command === 'fetch') {
          sendRaw(res, 200, handleFetch(repo, parseFetchArgs(args)), UPLOAD_RESULT_CONTENT);
        } else {
          sendJson(res, 400, { message: `unknown command ${command}` });
        }
      } else {
        // v0 fetch: "want <oid>" lines, "have <oid>" lines, "done".
        const lines = decodePktLines(body);
        const wants: string[] = [];
        const haves: string[] = [];
        for (const line of lines) {
          if (line === 'flush' || line === 'delim' || line === 'response-end') continue;
          const text = line.toString('utf8').trim();
          if (text.startsWith('want ')) wants.push(text.slice(5));
          else if (text.startsWith('have ')) haves.push(text.slice(5));
        }
        const toSend = collectReachable(wants, (oid) => repo.objects.get(oid), { allowMissing: true });
        const haveSet = collectReachable(haves, (oid) => repo.objects.get(oid), { allowMissing: true });
        const objects: GitObject[] = [];
        for (const oid of toSend) {
          if (haveSet.has(oid)) continue;
          const object = repo.objects.get(oid);
          if (object !== undefined) objects.push(object);
        }
        const pack = writePack(objects);
        const parts: Buffer[] = [pktLine('NAK\n'), pack];
        sendRaw(res, 200, Buffer.concat(parts), UPLOAD_RESULT_CONTENT);
      }
    } catch {
      sendJson(res, 400, { message: 'malformed pkt-line request' });
    }
    return true;
  }
  if (req.method === 'POST' && match[3] === 'git-receive-pack') {
    if (isV2) {
      sendJson(res, 400, { message: 'receive-pack does not speak protocol v2' });
      return true;
    }
    let body: Buffer;
    try {
      body = await readBody(req, MAX_BODY);
    } catch {
      sendJson(res, 400, { message: 'request body too large' });
      return true;
    }
    try {
      sendRaw(res, 200, handleReceivePack(repo, body), RECEIVE_RESULT_CONTENT);
    } catch {
      sendJson(res, 400, { message: 'malformed pkt-line request' });
    }
    return true;
  }
  return false;
}

/** True when a path is a git smart HTTP path (used by the router for auth/404). */
export function isGitPath(path: string): boolean {
  return /^\/[^/]+\/[^/]+\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.test(path);
}

/** The owner and repo name a git path names, or null when it is not one. */
export function gitPathParts(path: string): { owner: string; repo: string } | null {
  const match = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(path);
  if (match === null) return null;
  return { owner: match[1], repo: match[2] };
}
