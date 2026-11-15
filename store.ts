// The forge's in-memory store and the git object model.
//
// One Store holds every repository, its objects and refs, and its issues, pull
// requests, comments and labels. Everything is a Map lookup: the benchmark's read
// workload (2,000 sequential GETs of one issue) must not scan. No clock, no
// randomness: commit timestamps the server writes are one fixed epoch, ids are
// counters, request ids are counters, so a replay leaves the same state.
//
// The git object model mirrors git's own bytes exactly (SHA-1 over
// "<type> <size>\0" + content; trees sorted git's way; commits with
// "tree/parent/author/committer" headers), because the suite verifies objects
// by recomputing their hashes and comparing bytes.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO_OID = '0'.repeat(40);
export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  readonly type: GitObjectType;
  readonly data: Buffer;
}

export interface TreeEntry {
  readonly mode: string;
  readonly name: string;
  readonly oid: string;
}

export interface CommitView {
  readonly tree: string;
  readonly parents: string[];
  readonly author: string;
  readonly committer: string;
  readonly message: string;
}

export interface TagView {
  readonly object: string;
  readonly type: GitObjectType;
  readonly tag: string;
  readonly tagger: string;
  readonly message: string;
}

const HEX40 = /^[0-9a-f]{40}$/;
export function isObjectId(value: string): boolean {
  return HEX40.test(value);
}

/** SHA-1 over "<type> <size>\0" + bytes: git's object id. */
export function hashObject(type: GitObjectType, data: Buffer): string {
  const hash = createHash('sha1');
  hash.update(Buffer.from(`${type} ${data.length}\0`, 'utf8'));
  hash.update(data);
  return hash.digest('hex');
}

/** git's tree-entry sort: byte order of the name, a directory compared as if it ended in '/'. */
function compareTreeNames(a: { name: string; mode: string }, b: { name: string; mode: string }): number {
  const left = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
  const right = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
  return Buffer.compare(left, right);
}

/** Build a tree object's bytes from entries, sorted the way git sorts them. */
export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort(compareTreeNames);
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

/** Parse a tree object into its entries, in the object's stored order. */
export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space < 0) throw new Error('tree entry has no mode separator');
    const nul = data.indexOf(0x00, space + 1);
    if (nul < 0 || nul + 21 > data.length) throw new Error('tree entry is truncated');
    entries.push({
      mode: data.subarray(offset, space).toString('ascii'),
      name: data.subarray(space + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    offset = nul + 21;
  }
  return entries;
}

export interface BuildCommitFields {
  readonly tree: string;
  readonly parents?: readonly string[];
  readonly message: string;
  readonly author: string;
  readonly committer: string;
  readonly timestamp: number;
  readonly timezone?: string;
}

/** Build a commit object's bytes. */
export function buildCommit(fields: BuildCommitFields): Buffer {
  const timezone = fields.timezone ?? '+0000';
  const who = `${fields.author} ${fields.timestamp} ${timezone}`;
  const lines = [`tree ${fields.tree}`];
  for (const parent of fields.parents ?? []) lines.push(`parent ${parent}`);
  lines.push(`author ${who}`, `committer ${fields.committer} ${fields.timestamp} ${timezone}`, '');
  return Buffer.from(`${lines.join('\n')}\n${fields.message}`, 'utf8');
}

/** Parse a commit object. */
export function parseCommit(data: Buffer): CommitView {
  const blank = data.indexOf('\n\n');
  const headerEnd = blank < 0 ? data.length : blank;
  const message = blank < 0 ? '' : data.subarray(blank + 2).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of data.subarray(0, headerEnd).toString('utf8').split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    const key = space < 0 ? line : line.slice(0, space);
    const value = space < 0 ? '' : line.slice(space + 1);
    if (key === 'tree') tree = value;
    else if (key === 'parent') parents.push(value);
    else if (key === 'author') author = value;
    else if (key === 'committer') committer = value;
  }
  return { tree, parents, author, committer, message };
}

export interface BuildTagFields {
  readonly object: string;
  readonly type: GitObjectType;
  readonly tag: string;
  readonly message: string;
  readonly tagger: string;
  readonly timestamp: number;
  readonly timezone?: string;
}

/** Build an annotated tag object's bytes. */
export function buildTag(fields: BuildTagFields): Buffer {
  const timezone = fields.timezone ?? '+0000';
  return Buffer.from(
    `object ${fields.object}\ntype ${fields.type}\ntag ${fields.tag}\ntagger ${fields.tagger} ${fields.timestamp} ${timezone}\n\n${fields.message}`,
    'utf8',
  );
}

/** Parse an annotated tag object. */
export function parseTag(data: Buffer): TagView {
  const blank = data.indexOf('\n\n');
  const headerEnd = blank < 0 ? data.length : blank;
  const headers = new Map<string, string>();
  for (const line of data.subarray(0, headerEnd).toString('utf8').split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    if (space < 0) continue;
    headers.set(line.slice(0, space), line.slice(space + 1));
  }
  const type = headers.get('type') ?? '';
  return {
    object: headers.get('object') ?? '',
    type: type === 'commit' || type === 'tree' || type === 'blob' || type === 'tag' ? type : 'commit',
    tag: headers.get('tag') ?? '',
    tagger: headers.get('tagger') ?? '',
    message: blank < 0 ? '' : data.subarray(blank + 2).toString('utf8'),
  };
}

/** Peel an annotated tag to the commit it points at (through nested tags). */
export function peelToCommit(objects: Map<string, GitObject>, oid: string): string | null {
  let current = oid;
  for (let depth = 0; depth < 10; depth += 1) {
    const object = objects.get(current);
    if (object === undefined) return null;
    if (object.type === 'commit') return current;
    if (object.type !== 'tag') return null;
    current = parseTag(object.data).object;
  }
  return null;
}

/**
 * Every object reachable from `roots`: commit goes to its tree and parents, tree
 * to its entries (gitlinks skipped), tag to its target, blob nowhere. Missing
 * objects are skipped when `allowMissing` is set, else an error is thrown.
 */
export function collectReachable(
  roots: Iterable<string>,
  lookup: (oid: string) => GitObject | undefined,
  options: { allowMissing?: boolean } = {},
): Set<string> {
  const seen = new Set<string>();
  const queue: string[] = [];
  for (const root of roots) if (isObjectId(root)) queue.push(root);
  while (queue.length > 0) {
    const oid = queue.pop()!;
    if (seen.has(oid)) continue;
    const object = lookup(oid);
    if (object === undefined) {
      if (options.allowMissing) continue;
      throw new Error(`object ${oid} is missing from the store`);
    }
    seen.add(oid);
    if (object.type === 'commit') {
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
  return seen;
}

/** True when `ancestor` is reachable from `tip` (or equal to it). */
export function isAncestor(objects: Map<string, GitObject>, ancestor: string, tip: string): boolean {
  if (ancestor === tip) return true;
  const seen = new Set<string>();
  const queue: string[] = [tip];
  while (queue.length > 0) {
    const oid = queue.pop()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) {
      if (parent === ancestor) return true;
      queue.push(parent);
    }
  }
  return false;
}

/** The first common ancestor of two tips (both histories are walked breadth-first). */
export function mergeBase(objects: Map<string, GitObject>, a: string, b: string): string | null {
  if (a === b) return a;
  const fromA = new Set<string>();
  const queueA: string[] = [a];
  while (queueA.length > 0) {
    const oid = queueA.pop()!;
    if (fromA.has(oid)) continue;
    fromA.add(oid);
    const object = objects.get(oid);
    if (object !== undefined && object.type === 'commit') {
      for (const parent of parseCommit(object.data).parents) queueA.push(parent);
    }
  }
  const queueB: string[] = [b];
  const seenB = new Set<string>();
  while (queueB.length > 0) {
    const oid = queueB.pop()!;
    if (seenB.has(oid)) continue;
    seenB.add(oid);
    if (fromA.has(oid)) return oid;
    const object = objects.get(oid);
    if (object !== undefined && object.type === 'commit') {
      for (const parent of parseCommit(object.data).parents) queueB.push(parent);
    }
  }
  return null;
}

// ---------------------------------------------------------------------------------------------
// packfile read/write
// ---------------------------------------------------------------------------------------------

const TYPE_CODE: Readonly<Record<GitObjectType, number>> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Readonly<Record<number, GitObjectType>> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

function readPackSize(buffer: Buffer, start: number): { size: number; next: number; first: number } {
  let byte = buffer[start];
  const first = byte;
  let size = byte & 0x0f;
  let shift = 4;
  let offset = start + 1;
  while (byte & 0x80) {
    byte = buffer[offset++];
    size += (byte & 0x7f) * 2 ** shift;
    shift += 7;
  }
  return { size, next: offset, first };
}

function readOfsBase(buffer: Buffer, start: number): { baseOffset: number; next: number } {
  let byte = buffer[start];
  let offset = start + 1;
  let value = byte & 0x7f;
  while (byte & 0x80) {
    byte = buffer[offset++];
    value = ((value + 1) * 128) + (byte & 0x7f);
  }
  return { baseOffset: value, next: offset };
}

function inflateAt(buffer: Buffer, offset: number): { data: Buffer; next: number } {
  const result = inflateSync(buffer.subarray(offset), { info: true }) as unknown as {
    buffer: Buffer;
    engine: { bytesWritten: number };
  };
  const written = result.engine.bytesWritten;
  if (!written || written <= 0) throw new Error(`zlib stream at offset ${offset} consumed no bytes`);
  return { data: result.buffer, next: offset + written };
}

/** Apply a git delta (copy/insert instructions) to a base object's bytes. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readSize = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset++];
      value += (byte & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(byte & 0x80)) break;
    }
    return value;
  };
  const sourceSize = readSize();
  const targetSize = readSize();
  if (sourceSize !== base.length) throw new Error(`delta expects a ${sourceSize}-byte base, got ${base.length}`);
  const out = Buffer.allocUnsafe(targetSize);
  let written = 0;
  while (offset < delta.length) {
    const op = delta[offset++];
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      let shift = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if (op & (1 << bit)) copyOffset += delta[offset++] * 2 ** shift;
        shift += 8;
      }
      shift = 0;
      for (let bit = 4; bit < 7; bit += 1) {
        if (op & (1 << bit)) copySize += delta[offset++] * 2 ** shift;
        shift += 8;
      }
      if (copySize === 0) copySize = 0x10000;
      if (copyOffset + copySize > base.length) throw new Error('delta copy is out of the base');
      base.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      const literal = op & 0x7f;
      delta.copy(out, written, offset, offset + literal);
      offset += literal;
      written += literal;
    } else {
      throw new Error('delta opcode 0 is invalid');
    }
  }
  if (written !== targetSize) throw new Error(`delta produced ${written} of ${targetSize} bytes`);
  return out;
}

/** Read a packfile (version 2 or 3), resolving OFS_DELTA and REF_DELTA entries. */
export function readPack(pack: Buffer, resolveBase?: (oid: string) => GitObject | undefined): Map<string, GitObject> {
  if (pack.length < 32) throw new Error(`pack is ${pack.length} bytes, too short`);
  if (pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('pack does not start with PACK');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const declaredCount = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  interface RawEntry {
    offset: number;
    typeCode: number;
    size: number;
    data: Buffer;
    baseOffset?: number;
    baseOid?: string;
    resolved?: Buffer;
    resolvedType?: GitObjectType;
    oid?: string;
  }
  const raw: RawEntry[] = [];
  const byOffset = new Map<number, RawEntry>();
  let offset = 12;
  for (let index = 0; index < declaredCount; index += 1) {
    const entryOffset = offset;
    const header = readPackSize(pack, offset);
    offset = header.next;
    const typeCode = (header.first >> 4) & 0x07;
    const entry: RawEntry = { offset: entryOffset, typeCode, size: header.size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      const base = readOfsBase(pack, offset);
      offset = base.next;
      entry.baseOffset = entryOffset - base.baseOffset;
    } else if (typeCode === 7) {
      entry.baseOid = pack.subarray(offset, offset + 20).toString('hex');
      offset += 20;
    } else if (CODE_TYPE[typeCode] === undefined) {
      throw new Error(`pack entry ${index} has unknown type code ${typeCode}`);
    }
    const inflated = inflateAt(pack.subarray(0, trailerStart), offset);
    if (inflated.data.length !== header.size) {
      throw new Error(`pack entry at ${entryOffset} declares ${header.size} bytes but inflated to ${inflated.data.length}`);
    }
    entry.data = inflated.data;
    offset = inflated.next;
    raw.push(entry);
    byOffset.set(entryOffset, entry);
  }
  if (offset !== trailerStart) throw new Error(`pack has ${trailerStart - offset} stray bytes before its trailer`);
  const objects = new Map<string, GitObject>();
  const resolvedByOid = new Map<string, RawEntry>();
  const resolving = new Set<number>();
  const resolve = (entry: RawEntry): { type: GitObjectType; data: Buffer; oid: string } => {
    if (entry.resolved !== undefined && entry.resolvedType !== undefined && entry.oid !== undefined) {
      return { type: entry.resolvedType, data: entry.resolved, oid: entry.oid };
    }
    if (resolving.has(entry.offset)) throw new Error(`delta cycle at pack offset ${entry.offset}`);
    resolving.add(entry.offset);
    let type: GitObjectType;
    let data: Buffer;
    if (entry.typeCode === 6) {
      const baseEntry = byOffset.get(entry.baseOffset!);
      if (baseEntry === undefined) throw new Error(`ofs-delta base at offset ${entry.baseOffset} is not in the pack`);
      const base = resolve(baseEntry);
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else if (entry.typeCode === 7) {
      const wanted = entry.baseOid!;
      const baseEntry = resolvedByOid.get(wanted);
      if (baseEntry !== undefined) {
        const base = resolve(baseEntry);
        type = base.type;
        data = applyDelta(base.data, entry.data);
      } else {
        const external = resolveBase?.(wanted);
        if (external === undefined) throw new Error(`ref-delta base ${wanted} is neither in the pack nor in the store`);
        type = external.type;
        data = applyDelta(external.data, entry.data);
      }
    } else {
      type = CODE_TYPE[entry.typeCode];
      data = entry.data;
    }
    const oid = hashObject(type, data);
    entry.resolved = data;
    entry.resolvedType = type;
    entry.oid = oid;
    resolvedByOid.set(oid, entry);
    resolving.delete(entry.offset);
    return { type, data, oid };
  };
  for (const entry of raw) {
    const resolved = resolve(entry);
    if (!objects.has(resolved.oid)) objects.set(resolved.oid, { type: resolved.type, data: resolved.data });
  }
  return objects;
}

/** SHA-1 of every pack byte before the 20-byte trailer. */
export function packChecksum(pack: Buffer): string {
  return createHash('sha1').update(pack.subarray(0, pack.length - 20)).digest('hex');
}

/** True when the pack's trailer matches its bytes. */
export function verifyPackChecksum(pack: Buffer): boolean {
  if (pack.length < 20) return false;
  return pack.subarray(pack.length - 20).toString('hex') === packChecksum(pack);
}

function writePackObjectHeader(type: GitObjectType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let remaining = size;
  let first = (code << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  while (remaining > 0) {
    bytes.push(first | 0x80);
    first = remaining & 0x7f;
    remaining = Math.floor(remaining / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/** Build a packfile from plain objects (no deltas), version 2, trailer included. */
export function writePack(objects: Iterable<GitObject>): Buffer {
  const list = [...objects];
  const parts: Buffer[] = [];
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(list.length, 8);
  parts.push(header);
  for (const object of list) {
    parts.push(writePackObjectHeader(object.type, object.data.length));
    parts.push(deflateSync(object.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

// ---------------------------------------------------------------------------------------------
// pkt-line codec
// ---------------------------------------------------------------------------------------------

/** Encode one data packet: 4 hex length bytes (payload + 4) then the payload. */
export function pktLine(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const header = Buffer.allocUnsafe(4);
  header.write((data.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([header, data]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

/** Decode a whole buffer of pkt-lines into payloads ('flush'/'delim' as markers). */
export function decodePktLines(bytes: Buffer): Array<Buffer | 'flush' | 'delim' | 'response-end'> {
  const out: Array<Buffer | 'flush' | 'delim' | 'response-end'> = [];
  let offset = 0;
  while (offset + 4 <= bytes.length) {
    const header = bytes.subarray(offset, offset + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(header)) throw new Error(`invalid pkt-line length ${JSON.stringify(header)}`);
    const length = Number.parseInt(header, 16);
    if (length === 0) {
      out.push('flush');
      offset += 4;
      continue;
    }
    if (length === 1) {
      out.push('delim');
      offset += 4;
      continue;
    }
    if (length === 2) {
      out.push('response-end');
      offset += 4;
      continue;
    }
    if (length < 4 || offset + length > bytes.length) throw new Error('truncated pkt-line');
    out.push(bytes.subarray(offset + 4, offset + length));
    offset += length;
  }
  if (offset !== bytes.length) throw new Error('trailing bytes after the last pkt-line');
  return out;
}

// ---------------------------------------------------------------------------------------------
// the store
// ---------------------------------------------------------------------------------------------

/** The one fixed timestamp the server writes on its own commits (2025-01-01T00:00:00Z). */
export const FIXED_EPOCH_SEC = 1_735_689_600;
/** The one fixed ISO timestamp the server writes on REST/GraphQL objects. */
export const FIXED_ISO = '2026-11-15T00:00:00Z';

export interface Repo {
  readonly owner: string;
  readonly name: string;
  readonly id: number;
  description: string;
  isPrivate: boolean;
  defaultBranch: string;
  readonly objects: Map<string, GitObject>;
  readonly refs: Map<string, string>;
  readonly labels: Map<string, { name: string; color: string }>;
  readonly issues: Map<number, Issue>;
  readonly issueOrder: number[];
  nextIssueNumber: number;
  readonly pulls: Map<number, PullRequest>;
  readonly pullOrder: number[];
  nextPullNumber: number;
  nextCommentId: number;
  createdAt: string;
  updatedAt: string;
}

export interface Issue {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  readonly labels: string[];
  readonly comments: Comment[];
  createdAt: string;
  updatedAt: string;
}

export interface Comment {
  readonly id: number;
  body: string;
  createdAt: string;
  updatedAt: string;
}

export interface PullRequest {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  readonly headRef: string;
  readonly baseRef: string;
  headSha: string;
  baseSha: string;
  createdAt: string;
  updatedAt: string;
}

export interface Store {
  readonly repos: Map<string, Repo>;
  readonly viewerLogin: string;
  nextRepoId: number;
  nextObjectId: number;
  createRepo(owner: string, name: string, isPrivate: boolean, description: string): Repo | null;
  getRepo(owner: string, name: string): Repo | undefined;
  deleteRepo(owner: string, name: string): boolean;
  listRepos(owner: string): Repo[];
}

function repoKey(owner: string, name: string): string {
  return `${owner}/${name}`;
}

/** Create an empty store. `viewerLogin` is the login the bearer token names. */
export function createStore(viewerLogin = 'forge-user'): Store {
  const repos = new Map<string, Repo>();
  const store: Store = {
    repos,
    viewerLogin,
    nextRepoId: 1,
    nextObjectId: 1,
    createRepo(owner: string, name: string, isPrivate: boolean, description: string): Repo | null {
      const key = repoKey(owner, name);
      if (repos.has(key)) return null;
      const repo: Repo = {
        owner,
        name,
        id: store.nextRepoId++,
        description,
        isPrivate,
        defaultBranch: 'main',
        objects: new Map(),
        refs: new Map(),
        labels: new Map(),
        issues: new Map(),
        issueOrder: [],
        nextIssueNumber: 1,
        pulls: new Map(),
        pullOrder: [],
        nextPullNumber: 1,
        nextCommentId: 1,
        createdAt: FIXED_ISO,
        updatedAt: FIXED_ISO,
      };
      repos.set(key, repo);
      return repo;
    },
    getRepo(owner: string, name: string): Repo | undefined {
      return repos.get(repoKey(owner, name));
    },
    deleteRepo(owner: string, name: string): boolean {
      return repos.delete(repoKey(owner, name));
    },
    listRepos(owner: string): Repo[] {
      const out: Repo[] = [];
      for (const repo of repos.values()) if (repo.owner === owner) out.push(repo);
      return out;
    },
  };
  return store;
}

/** The identity line a server-written commit carries for `login`. */
export function serverIdentity(login: string): string {
  return `${login} <${login}@users.noreply.github.com>`;
}

/** Put an object in the repo's store, returning its id. */
export function putObject(repo: Repo, type: GitObjectType, data: Buffer): string {
  const oid = hashObject(type, data);
  if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
  return oid;
}

/** The tip a ref points at, or null when the ref does not exist. */
export function refTip(repo: Repo, ref: string): string | null {
  return repo.refs.get(ref) ?? null;
}

/** Walk a commit's tree to the blob at `path` (segments), or null when absent. */
export function treeEntryAt(repo: Repo, treeOid: string, segments: readonly string[]): TreeEntry | null {
  let current = treeOid;
  for (let index = 0; index < segments.length; index += 1) {
    const treeObject = repo.objects.get(current);
    if (treeObject === undefined || treeObject.type !== 'tree') return null;
    const entry = parseTree(treeObject.data).find((candidate) => candidate.name === segments[index]);
    if (entry === undefined) return null;
    if (index === segments.length - 1) return entry;
    if (entry.mode !== '40000') return null;
    current = entry.oid;
  }
  return null;
}

/** The flat map of path to blob entry for a whole tree (recursive). */
export function flattenTree(repo: Repo, treeOid: string, prefix = ''): Map<string, TreeEntry> {
  const out = new Map<string, TreeEntry>();
  const walk = (oid: string, base: string): void => {
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'tree') return;
    for (const entry of parseTree(object.data)) {
      const path = base === '' ? entry.name : `${base}/${entry.name}`;
      if (entry.mode === '40000') walk(entry.oid, path);
      else out.set(path, entry);
    }
  };
  walk(treeOid, prefix);
  return out;
}

/**
 * A new tree with `path` set to `blobOid` (or removed when `blobOid` is null),
 * built from `baseTreeOid` (or an empty tree). Returns the new tree's id.
 */
export function treeWithPath(repo: Repo, baseTreeOid: string | null, segments: readonly string[], blobOid: string | null): string {
  if (segments.length === 0) throw new Error('treeWithPath needs a path');
  const build = (treeOid: string | null, depth: number): string => {
    const entries = new Map<string, TreeEntry>();
    if (treeOid !== null) {
      const object = repo.objects.get(treeOid);
      if (object !== undefined && object.type === 'tree') {
        for (const entry of parseTree(object.data)) entries.set(entry.name, entry);
      }
    }
    const name = segments[depth];
    if (depth === segments.length - 1) {
      if (blobOid === null) entries.delete(name);
      else entries.set(name, { mode: '100644', name, oid: blobOid });
    } else {
      const child = entries.get(name);
      const childOid = child !== undefined && child.mode === '40000' ? child.oid : null;
      const next = build(childOid, depth + 1);
      const nextObject = repo.objects.get(next);
      const empty = nextObject !== undefined && nextObject.type === 'tree' && parseTree(nextObject.data).length === 0;
      if (empty && child === undefined) entries.delete(name);
      else if (empty) entries.delete(name);
      else entries.set(name, { mode: '40000', name, oid: next });
    }
    return putObject(repo, 'tree', buildTree([...entries.values()]));
  };
  return build(baseTreeOid, 0);
}

/** The commits reachable from `tip`, newest first (parents after their child). */
export function commitList(repo: Repo, tip: string): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const queue: string[] = [tip];
  while (queue.length > 0) {
    const oid = queue.shift()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    out.push(oid);
    for (const parent of parseCommit(object.data).parents) queue.push(parent);
  }
  return out;
}

/** A line diff between two texts: {additions, deletions} by line count (LCS). */
export function lineDiff(oldText: string, newText: string): { additions: number; deletions: number } {
  const oldLines = oldText === '' ? [] : oldText.split('\n');
  const newLines = newText === '' ? [] : newText.split('\n');
  const m = oldLines.length;
  const n = newLines.length;
  if (m === 0) return { additions: n, deletions: 0 };
  if (n === 0) return { additions: 0, deletions: m };
  // LCS lengths, two rows (files here are small).
  let prev = new Array<number>(n + 1).fill(0);
  let curr = new Array<number>(n + 1).fill(0);
  for (let i = 1; i <= m; i += 1) {
    for (let j = 1; j <= n; j += 1) {
      curr[j] = oldLines[i - 1] === newLines[j - 1] ? prev[j - 1] + 1 : Math.max(prev[j], curr[j - 1]);
    }
    const tmp = prev;
    prev = curr;
    curr = tmp;
    curr.fill(0);
  }
  const common = prev[n];
  return { additions: n - common, deletions: m - common };
}

/** The changed paths between two trees: {path, status, additions, deletions}. */
export function diffTrees(
  repo: Repo,
  baseTreeOid: string | null,
  headTreeOid: string,
): Array<{ path: string; status: 'added' | 'modified' | 'deleted'; additions: number; deletions: number }> {
  const base = baseTreeOid === null ? new Map<string, TreeEntry>() : flattenTree(repo, baseTreeOid);
  const head = flattenTree(repo, headTreeOid);
  const out: Array<{ path: string; status: 'added' | 'modified' | 'deleted'; additions: number; deletions: number }> = [];
  const paths = new Set([...base.keys(), ...head.keys()]);
  for (const path of [...paths].sort()) {
    const b = base.get(path);
    const h = head.get(path);
    if (b !== undefined && h === undefined) {
      const oldBlob = repo.objects.get(b.oid);
      const oldText = oldBlob !== undefined && oldBlob.type === 'blob' ? oldBlob.data.toString('utf8') : '';
      const diff = lineDiff(oldText, '');
      out.push({ path, status: 'deleted', additions: 0, deletions: diff.deletions });
    } else if (b === undefined && h !== undefined) {
      const newBlob = repo.objects.get(h.oid);
      const newText = newBlob !== undefined && newBlob.type === 'blob' ? newBlob.data.toString('utf8') : '';
      const diff = lineDiff('', newText);
      out.push({ path, status: 'added', additions: diff.additions, deletions: 0 });
    } else if (b !== undefined && h !== undefined && b.oid !== h.oid) {
      const oldBlob = repo.objects.get(b.oid);
      const newBlob = repo.objects.get(h.oid);
      const oldText = oldBlob !== undefined && oldBlob.type === 'blob' ? oldBlob.data.toString('utf8') : '';
      const newText = newBlob !== undefined && newBlob.type === 'blob' ? newBlob.data.toString('utf8') : '';
      const diff = lineDiff(oldText, newText);
      out.push({ path, status: 'modified', additions: diff.additions, deletions: diff.deletions });
    }
  }
  return out;
}
