// Independent verification harness for git-forge.
//
// Boots the server on an ephemeral port and probes REST, GraphQL, MCP and git
// smart HTTP directly, outside the sealed suite. The git probes build their
// own packs and recompute every object hash, so a pass here means the wire
// really matches git, not just that the server agrees with itself.
//
// Run: node --experimental-strip-types tests/verify.mjs
// Exit code: 0 when every probe passes, 1 otherwise.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

process.env.MAKEFUN_SHOP_TOKEN = 'verify-token';
const { createApp } = await import('../app.ts');
const { VIEWER_LOGIN } = await import('../scenario.ts');

const TOKEN = 'verify-token';
const LOGIN = VIEWER_LOGIN;

let passed = 0;
let failed = 0;
const failures = [];

function check(name, ok, detail = '') {
  if (ok) {
    passed += 1;
    console.log(`  ok   ${name}`);
  } else {
    failed += 1;
    failures.push(name);
    console.log(`  FAIL ${name}${detail === '' ? '' : ` — ${detail}`}`);
  }
}

function eq(actual, expected) {
  return JSON.stringify(actual) === JSON.stringify(expected);
}

// -------------------------------------------------------------------------------------------
// http helpers
// -------------------------------------------------------------------------------------------

let base = '';
const defaultHeaders = { authorization: `Bearer ${TOKEN}` };

async function request(method, path, options = {}) {
  // Auth by default; a probe that passes headers explicitly (even {}) opts out.
  const headers = options.headers === undefined ? { ...defaultHeaders } : { ...(options.headers ?? {}) };
  let body;
  if (options.json !== undefined) {
    headers['content-type'] = 'application/json';
    body = JSON.stringify(options.json);
  } else if (options.raw !== undefined) {
    body = options.raw;
  }
  const res = await fetch(`${base}${path}`, { method, headers, body });
  const text = await res.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {
    // Not JSON (a packfile stream, for example): the caller reads the text.
  }
  return { status: res.status, headers: res.headers, text, json };
}

async function get(path, headers = defaultHeaders) {
  return request('GET', path, { headers });
}

async function post(path, json, headers = defaultHeaders) {
  return request('POST', path, { json, headers });
}

// -------------------------------------------------------------------------------------------
// git helpers (an independent object model, recomputed here)
// -------------------------------------------------------------------------------------------

function hashObject(type, data) {
  const hash = createHash('sha1');
  hash.update(Buffer.from(`${type} ${data.length}\0`, 'utf8'));
  hash.update(data);
  return hash.digest('hex');
}

function buildTree(entries) {
  const sorted = [...entries].sort((a, b) => {
    const left = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
    const right = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
    return Buffer.compare(left, right);
  });
  const parts = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

function buildCommit({ tree, parents = [], message, ts = 1735689600 }) {
  const who = `${LOGIN} <${LOGIN}@example.invalid> ${ts} +0000`;
  const lines = [`tree ${tree}`];
  for (const parent of parents) lines.push(`parent ${parent}`);
  lines.push(`author ${who}`, `committer ${who}`, '');
  return Buffer.from(`${lines.join('\n')}\n${message}`, 'utf8');
}

function writePack(objects) {
  const code = { commit: 1, tree: 2, blob: 3, tag: 4 };
  const parts = [];
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const object of objects) {
    const bytes = [];
    let remaining = object.data.length;
    let first = (code[object.type] << 4) | (remaining & 0x0f);
    remaining = Math.floor(remaining / 16);
    while (remaining > 0) {
      bytes.push(first | 0x80);
      first = remaining & 0x7f;
      remaining = Math.floor(remaining / 128);
    }
    bytes.push(first);
    parts.push(Buffer.from(bytes));
    parts.push(deflateSync(object.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

function pktLine(payload) {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const header = Buffer.allocUnsafe(4);
  header.write((data.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([header, data]);
}

const PKT_FLUSH = Buffer.from('0000', 'ascii');

/** Split a buffer into pkt-line payloads ('flush' for 0000, 'delim' for 0001). */
function decodePktLines(bytes) {
  const out = [];
  let offset = 0;
  while (offset + 4 <= bytes.length) {
    const length = Number.parseInt(bytes.subarray(offset, offset + 4).toString('ascii'), 16);
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
    if (length < 4 || offset + length > bytes.length) throw new Error('truncated pkt-line in test helper');
    out.push(bytes.subarray(offset + 4, offset + length));
    offset += length;
  }
  if (offset !== bytes.length) throw new Error('trailing bytes in test helper');
  return out;
}

/** Parse a v2 fetch response into its sections, returning the pack bytes or null. */
function parseFetchResponse(bytes) {
  const lines = decodePktLines(bytes);
  let section = null;
  const sections = { acknowledgments: [], 'shallow-info': [], packfile: [] };
  const packChunks = [];
  let inPack = false;
  let afterHeader = false;
  for (const line of lines) {
    if (line === 'flush') {
      // The flush right after a section header is only a separator; the flush
      // after the pack data is what closes the packfile section.
      if (afterHeader) {
        afterHeader = false;
        continue;
      }
      if (inPack) inPack = false;
      continue;
    }
    if (line === 'delim') continue;
    const text = line.toString('utf8');
    if (text === 'acknowledgments\n' || text === 'shallow-info\n' || text === 'packfile\n') {
      section = text.trim();
      afterHeader = true;
      if (section === 'packfile') inPack = true;
      continue;
    }
    afterHeader = false;
    if (inPack) {
      const band = line[0];
      if (band !== 1) throw new Error(`sideband band ${band}, expected 1`);
      packChunks.push(line.subarray(1));
      continue;
    }
    if (section !== null && sections[section] !== undefined) sections[section].push(text.trim());
  }
  return { sections, pack: packChunks.length === 0 ? null : Buffer.concat(packChunks) };
}

/** Read a packfile's objects (plain entries only; the harness never sends deltas). */
function readPackObjects(pack) {
  const count = pack.readUInt32BE(8);
  const objects = new Map();
  let offset = 12;
  for (let i = 0; i < count; i += 1) {
    let byte = pack[offset];
    const typeCode = (byte >> 4) & 0x07;
    let size = byte & 0x0f;
    let shift = 4;
    offset += 1;
    while (byte & 0x80) {
      byte = pack[offset++];
      size += (byte & 0x7f) * 2 ** shift;
      shift += 7;
    }
    const type = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' }[typeCode];
    const inflated = inflateSync(pack.subarray(offset), { info: true });
    const written = inflated.engine.bytesWritten;
    if (inflated.buffer.length !== size) throw new Error('pack entry size mismatch in test helper');
    const oid = hashObject(type, inflated.buffer);
    objects.set(oid, { type, data: inflated.buffer });
    offset += written;
  }
  return objects;
}

async function gitRequest(method, path, body, extraHeaders = {}) {
  const headers = { ...defaultHeaders, ...extraHeaders };
  const res = await fetch(`${base}${path}`, { method, headers, body });
  const buffer = Buffer.from(await res.arrayBuffer());
  return { status: res.status, headers: res.headers, buffer };
}

// -------------------------------------------------------------------------------------------
// the probes
// -------------------------------------------------------------------------------------------

async function probeRest() {
  console.log('REST');
  let res = await get('/healthz', {});
  check('healthz 200 {ok:true} without a token', res.status === 200 && res.json?.ok === true);
  res = await get('/api/v3/user', {});
  check('missing token is 401', res.status === 401 && typeof res.json?.message === 'string');
  res = await get('/api/v3/user', { authorization: 'Bearer wrong' });
  check('wrong token is 401', res.status === 401);
  res = await get('/api/v3/user');
  check('GET /user returns the login', res.status === 200 && res.json?.login === LOGIN);
  res = await get('/api/v3/');
  check('GET /api/v3/ 200 + X-Oauth-Scopes', res.status === 200 && (res.headers.get('x-oauth-scopes') ?? '').includes('repo'));
  res = await get('/api/v3/meta');
  check('GET /meta has a non-empty installed_version', res.status === 200 && typeof res.json?.installed_version === 'string' && res.json.installed_version !== '');
  res = await get('/api/v3/repos/nope/nope');
  check('unknown repo is 404 JSON', res.status === 404 && typeof res.json?.message === 'string');
  res = await get('/api/v3/user/repos');
  check('every REST reply carries a request id', typeof res.headers.get('x-github-request-id') === 'string' && res.headers.get('x-github-request-id') !== '');
  res = await request('GET', '/api/v3/definitely-not-a-route');
  check('unknown route is 404 JSON', res.status === 404 && typeof res.json?.message === 'string');
  res = await request('POST', '/api/v3/user/repos', { raw: '{not json', headers: { ...defaultHeaders, 'content-type': 'application/json' } });
  check('malformed JSON body is 400', res.status === 400);

  res = await post('/api/v3/user/repos', { name: 'probe-repo', description: 'probe' });
  check('create repo 201', res.status === 201 && res.json?.name === 'probe-repo' && res.json?.default_branch === 'main');
  res = await post('/api/v3/user/repos', { name: 'probe-repo' });
  check('duplicate repo 422', res.status === 422);
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo`);
  check('get repo 200', res.status === 200 && res.json?.description === 'probe');
  res = await get('/api/v3/repos/someone-else/probe-repo');
  check('same name under another owner is 404, never 403', res.status === 404);
  res = await request('PATCH', `/api/v3/repos/${LOGIN}/probe-repo`, { json: { description: 'changed' } });
  check('patch repo description', res.status === 200 && res.json?.description === 'changed');

  const content = Buffer.from('hello world\n', 'utf8').toString('base64');
  res = await request('PUT', `/api/v3/repos/${LOGIN}/probe-repo/contents/docs/readme.txt`, { json: { message: 'add readme', content } });
  check('PUT contents create 201', res.status === 201 && res.json?.content?.encoding === 'base64');
  const blobSha = res.json?.content?.sha;
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/contents/docs/readme.txt`);
  check('GET contents returns the bytes', res.status === 200 && res.json?.content === content && res.json?.sha === blobSha && res.json?.size === 12);
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/contents/docs`);
  check('GET directory lists children', res.status === 200 && Array.isArray(res.json) && res.json.some((entry) => entry.name === 'readme.txt'));
  res = await request('PUT', `/api/v3/repos/${LOGIN}/probe-repo/contents/docs/readme.txt`, { json: { message: 'stale', content, sha: '0'.repeat(40) } });
  check('PUT contents with a stale sha is 409', res.status === 409);
  const content2 = Buffer.from('hello world v2\n', 'utf8').toString('base64');
  res = await request('PUT', `/api/v3/repos/${LOGIN}/probe-repo/contents/docs/readme.txt`, { json: { message: 'update readme', content: content2, sha: blobSha } });
  check('PUT contents with the current sha is 200', res.status === 200);
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/contents/docs/readme.txt`);
  check('GET contents after update returns v2', res.status === 200 && res.json?.content === content2);
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/commits`);
  check('GET commits lists the contents commits', res.status === 200 && Array.isArray(res.json) && res.json.length === 2 && res.json[0].commit.message === 'update readme');
  res = await request('DELETE', `/api/v3/repos/${LOGIN}/probe-repo/contents/docs/readme.txt`, { json: { message: 'remove readme', sha: res.json?.[0]?.sha } });
  // The delete needs the blob sha, not the commit sha: redo with the right one.
  if (res.status !== 200) {
    const current = await get(`/api/v3/repos/${LOGIN}/probe-repo/contents/docs/readme.txt`);
    res = await request('DELETE', `/api/v3/repos/${LOGIN}/probe-repo/contents/docs/readme.txt`, { json: { message: 'remove readme', sha: current.json?.sha } });
  }
  check('DELETE contents 200', res.status === 200 && typeof res.json?.commit?.sha === 'string');
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/contents/docs/readme.txt`);
  check('GET contents after delete is 404', res.status === 404);

  res = await post(`/api/v3/repos/${LOGIN}/probe-repo/labels`, { name: 'bug', color: 'ff0000' });
  check('create label 201', res.status === 201 && res.json?.name === 'bug');
  res = await post(`/api/v3/repos/${LOGIN}/probe-repo/issues`, { title: 'bad label', labels: ['nope'] });
  check('issue with an unknown label is 422', res.status === 422);
  res = await post(`/api/v3/repos/${LOGIN}/probe-repo/issues`, { title: 'first', body: 'b1', labels: ['bug'] });
  check('create issue 201 open', res.status === 201 && res.json?.number === 1 && res.json?.state === 'open' && res.json?.labels?.[0]?.name === 'bug');
  res = await post(`/api/v3/repos/${LOGIN}/probe-repo/issues`, { title: 'second' });
  check('issue numbers increment', res.status === 201 && res.json?.number === 2);
  res = await request('PATCH', `/api/v3/repos/${LOGIN}/probe-repo/issues/1`, { json: { state: 'closed' } });
  check('close issue persists', res.status === 200 && res.json?.state === 'closed');
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/issues?state=open`);
  check('open filter omits the closed issue', res.status === 200 && res.json?.length === 1 && res.json[0].number === 2);
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/issues?state=all&sort=created&direction=asc`);
  check('asc order is creation order', res.status === 200 && eq(res.json.map((issue) => issue.number), [1, 2]));
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/issues?state=all&sort=created&direction=desc`);
  check('desc order is reverse', res.status === 200 && eq(res.json.map((issue) => issue.number), [2, 1]));
  res = await post(`/api/v3/repos/${LOGIN}/probe-repo/issues/2/comments`, { body: 'a comment' });
  check('create comment 201 with an int id', res.status === 201 && typeof res.json?.id === 'number' && res.json.id >= 1);
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/issues/2/comments`);
  check('list comments includes it', res.status === 200 && res.json?.length === 1 && res.json[0].body === 'a comment');

  // Pagination: 35 repos, 10 per page, Link next, no duplicates.
  for (let i = 0; i < 35; i += 1) await post('/api/v3/user/repos', { name: `page-repo-${i}` });
  const seen = new Set();
  let url = '/api/v3/user/repos?per_page=10';
  let pages = 0;
  for (;;) {
    res = await get(url);
    if (res.status !== 200 || !Array.isArray(res.json)) break;
    pages += 1;
    for (const repo of res.json) seen.add(repo.name);
    const link = res.headers.get('link') ?? '';
    const next = /<([^>]+)>; rel="next"/.exec(link);
    if (next === null) break;
    url = new URL(next[1]).pathname + new URL(next[1]).search;
    if (pages > 20) break;
  }
  check('pagination walks 4 pages with no duplicates', pages === 4 && seen.size === 36);
}

async function probeGit() {
  console.log('git smart HTTP');
  const repo = 'git-repo';
  await post('/api/v3/user/repos', { name: repo });
  let res = await gitRequest('GET', `/${LOGIN}/${repo}.git/info/refs?service=git-upload-pack`, null, { 'git-protocol': 'version=2' });
  const adV2 = res.buffer.toString('utf8');
  check('v2 advertisement: content type + version 2 + caps', res.status === 200
    && (res.headers.get('content-type') ?? '').includes('application/x-git-upload-pack-advertisement')
    && adV2.includes('version 2\n') && adV2.includes('fetch=shallow') && adV2.includes('ls-refs=unborn') && adV2.includes('object-format=sha1'));
  res = await gitRequest('GET', `/${LOGIN}/${repo}.git/info/refs?service=git-upload-pack`);
  const adV0 = res.buffer.toString('utf8');
  check('v0 advertisement opens with the # service preamble + symref HEAD', res.status === 200 && adV0.includes('# service=git-upload-pack') && adV0.includes('symref=HEAD:refs/heads/main'));
  res = await gitRequest('GET', `/${LOGIN}/${repo}.git/info/refs?service=git-receive-pack`);
  const adRecv = res.buffer.toString('utf8');
  check('receive-pack advertisement carries report-status, side-band-64k, delete-refs', res.status === 200 && adRecv.includes('report-status') && adRecv.includes('side-band-64k') && adRecv.includes('delete-refs'));
  res = await gitRequest('GET', `/${LOGIN}/missing.git/info/refs?service=git-upload-pack`, null, { 'git-protocol': 'version=2' });
  check('missing repo is 404 on info/refs', res.status === 404);
  res = await gitRequest('GET', `/${LOGIN}/${repo}.git/info/refs?service=git-upload-pack`, null, { 'git-protocol': 'version=2' });
  check('server stays up after the 404', res.status === 200);

  // Build a two-commit history and push it.
  const blob1 = Buffer.from('one\n', 'utf8');
  const blob1Oid = hashObject('blob', blob1);
  const tree1 = buildTree([{ mode: '100644', name: 'a.txt', oid: blob1Oid }]);
  const tree1Oid = hashObject('tree', tree1);
  const commit1 = buildCommit({ tree: tree1Oid, message: 'first\n' });
  const commit1Oid = hashObject('commit', commit1);
  const blob2 = Buffer.from('two\n', 'utf8');
  const blob2Oid = hashObject('blob', blob2);
  const tree2 = buildTree([{ mode: '100644', name: 'a.txt', oid: blob2Oid }]);
  const tree2Oid = hashObject('tree', tree2);
  const commit2 = buildCommit({ tree: tree2Oid, parents: [commit1Oid], message: 'second\n' });
  const commit2Oid = hashObject('commit', commit2);
  const pack = writePack([
    { type: 'blob', data: blob1 }, { type: 'tree', data: tree1 }, { type: 'commit', data: commit1 },
    { type: 'blob', data: blob2 }, { type: 'tree', data: tree2 }, { type: 'commit', data: commit2 },
  ]);
  const pushBody = Buffer.concat([
    pktLine(`${'0'.repeat(40)} ${commit2Oid} refs/heads/main\0report-status side-band-64k ofs-delta agent=probe\n`),
    PKT_FLUSH,
    pack,
  ]);
  res = await gitRequest('POST', `/${LOGIN}/${repo}.git/git-receive-pack`, pushBody);
  const report = res.buffer.toString('utf8');
  check('push first commit: unpack ok + ok main', res.status === 200 && report.includes('unpack ok') && report.includes('ok refs/heads/main'));
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/branches/main`);
  check('branch tip is the pushed commit', res.status === 200 && res.json?.commit?.sha === commit2Oid);

  // ls-refs v2.
  const lsBody = Buffer.concat([
    pktLine('command=ls-refs\n'), pktLine('agent=probe\n'), pktLine('object-format=sha1\n'), Buffer.from('0001', 'ascii'),
    pktLine('peel\n'), pktLine('symrefs\n'), pktLine('unborn\n'), PKT_FLUSH,
  ]);
  res = await gitRequest('POST', `/${LOGIN}/${repo}.git/git-upload-pack`, lsBody, { 'git-protocol': 'version=2' });
  const lsText = res.buffer.toString('utf8');
  check('ls-refs lists HEAD symref + main', lsText.includes(`${commit2Oid} HEAD symref-target:refs/heads/main`) && lsText.includes(`${commit2Oid} refs/heads/main`));

  // Fetch the whole history (v2), then verify every object hash and the blob bytes.
  const fetchBody = Buffer.concat([
    pktLine('command=fetch\n'), pktLine('agent=probe\n'), pktLine('object-format=sha1\n'), Buffer.from('0001', 'ascii'),
    pktLine('thin-pack\n'), pktLine('no-progress\n'), pktLine('ofs-delta\n'),
    pktLine(`want ${commit2Oid}\n`), pktLine('done\n'), PKT_FLUSH,
  ]);
  res = await gitRequest('POST', `/${LOGIN}/${repo}.git/git-upload-pack`, fetchBody, { 'git-protocol': 'version=2' });
  const fetched = parseFetchResponse(res.buffer);
  const objects = fetched.pack === null ? new Map() : readPackObjects(fetched.pack);
  check('fetch returns all 6 objects with matching hashes', objects.size === 6
    && objects.get(commit2Oid)?.data.equals(commit2) && objects.get(blob2Oid)?.data.equals(blob2));
  check('fetch ack section is NAK with no haves', fetched.sections.acknowledgments.includes('NAK'));

  // Fetch again with the tip as a have: nothing new, so no packfile section.
  const fetchNothingBody = Buffer.concat([
    pktLine('command=fetch\n'), pktLine('agent=probe\n'), pktLine('object-format=sha1\n'), Buffer.from('0001', 'ascii'),
    pktLine('thin-pack\n'), pktLine('no-progress\n'), pktLine('ofs-delta\n'),
    pktLine(`want ${commit2Oid}\n`), pktLine(`have ${commit2Oid}\n`), pktLine('done\n'), PKT_FLUSH,
  ]);
  res = await gitRequest('POST', `/${LOGIN}/${repo}.git/git-upload-pack`, fetchNothingBody, { 'git-protocol': 'version=2' });
  const fetchedNothing = parseFetchResponse(res.buffer);
  check('fetch with everything had sends no packfile', fetchedNothing.pack === null && !res.buffer.toString('utf8').includes('packfile'));

  // Shallow depth-1 fetch of the tip.
  const fetchShallowBody = Buffer.concat([
    pktLine('command=fetch\n'), pktLine('agent=probe\n'), pktLine('object-format=sha1\n'), Buffer.from('0001', 'ascii'),
    pktLine('thin-pack\n'), pktLine('no-progress\n'), pktLine('ofs-delta\n'),
    pktLine(`want ${commit2Oid}\n`), pktLine('deepen 1\n'), pktLine('done\n'), PKT_FLUSH,
  ]);
  res = await gitRequest('POST', `/${LOGIN}/${repo}.git/git-upload-pack`, fetchShallowBody, { 'git-protocol': 'version=2' });
  const fetchedShallow = parseFetchResponse(res.buffer);
  const shallowObjects = fetchedShallow.pack === null ? new Map() : readPackObjects(fetchedShallow.pack);
  check('shallow depth-1 delivers only tip + tree + blob', shallowObjects.size === 3 && shallowObjects.has(commit2Oid) && !shallowObjects.has(commit1Oid));
  check('shallow-info names the tip as shallow', fetchedShallow.sections['shallow-info'].includes(`shallow ${commit2Oid}`));

  // Non-fast-forward push is refused and the ref does not move.
  const badPack = writePack([{ type: 'blob', data: blob1 }, { type: 'tree', data: tree1 }, { type: 'commit', data: commit1 }]);
  const badPushBody = Buffer.concat([
    pktLine(`${commit2Oid} ${commit1Oid} refs/heads/main\0report-status side-band-64k ofs-delta agent=probe\n`),
    PKT_FLUSH,
    badPack,
  ]);
  res = await gitRequest('POST', `/${LOGIN}/${repo}.git/git-receive-pack`, badPushBody);
  check('non-fast-forward push is refused per ref', res.buffer.toString('utf8').includes('ng refs/heads/main non-fast-forward'));
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/branches/main`);
  check('ref did not move after the refused push', res.json?.commit?.sha === commit2Oid);

  // Mixed push: one fast-forward branch create + one refused update.
  const mixedPack = writePack([{ type: 'blob', data: blob1 }, { type: 'tree', data: tree1 }, { type: 'commit', data: commit1 }]);
  const mixedBody = Buffer.concat([
    pktLine(`${'0'.repeat(40)} ${commit1Oid} refs/heads/topic\0report-status side-band-64k ofs-delta agent=probe\n`),
    pktLine(`${commit2Oid} ${commit1Oid} refs/heads/main\n`),
    PKT_FLUSH,
    mixedPack,
  ]);
  res = await gitRequest('POST', `/${LOGIN}/${repo}.git/git-receive-pack`, mixedBody);
  const mixedReport = res.buffer.toString('utf8');
  check('mixed push applies the good ref and refuses the bad one', mixedReport.includes('ok refs/heads/topic') && mixedReport.includes('ng refs/heads/main non-fast-forward'));
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/branches/topic`);
  check('the created branch exists', res.status === 200 && res.json?.commit?.sha === commit1Oid);

  // Delete the topic branch.
  const deleteBody = Buffer.concat([
    pktLine(`${commit1Oid} ${'0'.repeat(40)} refs/heads/topic\0report-status side-band-64k ofs-delta agent=probe\n`),
    PKT_FLUSH,
  ]);
  res = await gitRequest('POST', `/${LOGIN}/${repo}.git/git-receive-pack`, deleteBody);
  check('delete branch answers ok', res.buffer.toString('utf8').includes('ok refs/heads/topic'));
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/branches/topic`);
  check('deleted branch is gone', res.status === 404);

  // Malformed push body is a 400 and the server stays up.
  res = await gitRequest('POST', `/${LOGIN}/${repo}.git/git-receive-pack`, Buffer.from('zzzznot-a-pkt-line', 'utf8'));
  check('malformed receive-pack body is 400 JSON', res.status === 400 && res.buffer.toString('utf8').includes('message'));
  res = await get('/healthz', {});
  check('server stays up after the malformed body', res.status === 200);
}

async function probePulls() {
  console.log('pull requests (REST)');
  const repo = 'pulls-repo';
  await post('/api/v3/user/repos', { name: repo });
  const b64 = (text) => Buffer.from(text, 'utf8').toString('base64');
  let res = await request('PUT', `/api/v3/repos/${LOGIN}/${repo}/contents/a.txt`, { json: { message: 'base file', content: b64('v1\n') } });
  check('contents write creates the default branch', res.status === 201);
  const baseCommit = res.json?.commit?.sha;
  res = await post(`/api/v3/repos/${LOGIN}/${repo}/git/refs`, { ref: 'refs/heads/feature', sha: baseCommit });
  check('create branch via git/refs 201', res.status === 201 && res.json?.object?.sha === baseCommit);
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/branches`);
  check('list branches includes main and feature', res.status === 200 && res.json?.some((branch) => branch.name === 'feature'));
  res = await request('PUT', `/api/v3/repos/${LOGIN}/${repo}/contents/a.txt`, { json: { message: 'feature change', content: b64('v2\n'), branch: 'feature', sha: undefined } });
  // The update needs the current blob sha: fetch it first.
  if (res.status !== 200) {
    const current = await get(`/api/v3/repos/${LOGIN}/${repo}/contents/a.txt?ref=feature`);
    res = await request('PUT', `/api/v3/repos/${LOGIN}/${repo}/contents/a.txt`, { json: { message: 'feature change', content: b64('v2\n'), branch: 'feature', sha: current.json?.sha } });
  }
  check('contents write on the feature branch 200', res.status === 200);
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/commits?sha=feature`);
  check('branch commits list only the branch commits', res.status === 200 && res.json?.length === 2 && res.json[0].commit.message === 'feature change');
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/commits`);
  check('default branch commits exclude the feature commit', res.status === 200 && res.json?.length === 1);

  res = await post(`/api/v3/repos/${LOGIN}/${repo}/pulls`, { title: 'feature pr', head: 'feature', base: 'main' });
  check('create pull request 201 with head/base shas', res.status === 201 && res.json?.number === 1 && res.json?.state === 'open' && typeof res.json?.head?.sha === 'string' && typeof res.json?.base?.sha === 'string');
  res = await post(`/api/v3/repos/${LOGIN}/${repo}/pulls`, { title: 'duplicate', head: 'feature', base: 'main' });
  check('duplicate open pull request is 422', res.status === 422);
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/pulls/1/files`);
  check('pull files show the modified file with additions', res.status === 200 && Array.isArray(res.json) && res.json.some((file) => file.filename === 'a.txt' && file.status === 'modified' && file.additions >= 1));
  res = await request('PUT', `/api/v3/repos/${LOGIN}/${repo}/pulls/1/merge`, { json: { merge_method: 'squash' } });
  check('squash merge 200 merged:true', res.status === 200 && res.json?.merged === true && typeof res.json?.sha === 'string');
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/pulls/1`);
  check('merged pull reads closed with merged_at', res.status === 200 && res.json?.state === 'closed' && res.json?.merged === true && typeof res.json?.merged_at === 'string');
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/pulls?state=all`);
  check('pulls state=all lists the merged one', res.status === 200 && res.json?.some((pull) => pull.number === 1));
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/pulls`);
  check('pulls default lists open only', res.status === 200 && res.json?.length === 0);
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/contents/a.txt`);
  check('base branch now has the merged content', res.status === 200 && res.json?.content === b64('v2\n'));
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/commits`);
  check('merge commit is on the default branch', res.status === 200 && res.json?.[0]?.commit?.message === 'feature pr');
  res = await request('PUT', `/api/v3/repos/${LOGIN}/${repo}/pulls/999/merge`, { json: { merge_method: 'squash' } });
  check('merge of an unknown pull is 404', res.status === 404);

  // A conflicted merge: both sides change the same file from the same base.
  await post(`/api/v3/repos/${LOGIN}/${repo}/git/refs`, { ref: 'refs/heads/conflict', sha: (await get(`/api/v3/repos/${LOGIN}/${repo}/branches/main`)).json?.commit?.sha });
  const mainCurrent = await get(`/api/v3/repos/${LOGIN}/${repo}/contents/a.txt`);
  await request('PUT', `/api/v3/repos/${LOGIN}/${repo}/contents/a.txt`, { json: { message: 'main moves', content: b64('v3\n'), sha: mainCurrent.json?.sha } });
  const conflictCurrent = await get(`/api/v3/repos/${LOGIN}/${repo}/contents/a.txt?ref=conflict`);
  await request('PUT', `/api/v3/repos/${LOGIN}/${repo}/contents/a.txt`, { json: { message: 'conflict change', content: b64('v4\n'), branch: 'conflict', sha: conflictCurrent.json?.sha } });
  res = await post(`/api/v3/repos/${LOGIN}/${repo}/pulls`, { title: 'conflict pr', head: 'conflict', base: 'main' });
  check('conflicted pull request opens', res.status === 201 && res.json?.number === 2);
  res = await request('PUT', `/api/v3/repos/${LOGIN}/${repo}/pulls/2/merge`, { json: { merge_method: 'squash' } });
  check('conflicted merge is 405', res.status === 405);

  // A merge that adds a nested file: the merged tree must keep the directory.
  const nestedBase = (await get(`/api/v3/repos/${LOGIN}/${repo}/branches/main`)).json?.commit?.sha;
  await post(`/api/v3/repos/${LOGIN}/${repo}/git/refs`, { ref: 'refs/heads/nested', sha: nestedBase });
  res = await request('PUT', `/api/v3/repos/${LOGIN}/${repo}/contents/config/nested.txt`, { json: { message: 'add nested', content: b64('nested\n'), branch: 'nested' } });
  check('contents write on a new nested path is 201', res.status === 201);
  res = await post(`/api/v3/repos/${LOGIN}/${repo}/pulls`, { title: 'nested pr', head: 'nested', base: 'main' });
  check('nested pull request opens', res.status === 201 && typeof res.json?.number === 'number');
  const nestedNumber = res.json?.number;
  res = await request('PUT', `/api/v3/repos/${LOGIN}/${repo}/pulls/${nestedNumber}/merge`, { json: { merge_method: 'squash' } });
  check('nested merge succeeds', res.status === 200 && res.json?.merged === true);
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/contents/config/nested.txt`);
  check('merged nested file is readable on the base branch', res.status === 200 && res.json?.content === b64('nested\n'));
  res = await get(`/api/v3/repos/${LOGIN}/${repo}/contents/config`);
  check('merged directory lists the nested file', res.status === 200 && Array.isArray(res.json) && res.json.some((entry) => entry.name === 'nested.txt'));
}

async function gql(query, variables) {
  // The probes name the viewer login in their documents; substitute the
  // scenario-derived login so the documents always address the real owner.
  return post('/api/graphql', { query: query.replaceAll('forge-user', LOGIN), variables });
}

async function probeGraphql() {
  console.log('GraphQL');
  let res = await gql('query { viewer { login } }');
  check('viewer login', res.status === 200 && res.json?.data?.viewer?.login === LOGIN);
  res = await gql('query ($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id databaseId name nameWithOwner owner { login } defaultBranchRef { name } viewerPermission hasIssuesEnabled } }', { owner: LOGIN, name: 'probe-repo' });
  const repoData = res.json?.data?.repository;
  check('repository fields + opaque id + positive databaseId', res.status === 200
    && repoData?.name === 'probe-repo' && repoData?.nameWithOwner === `${LOGIN}/probe-repo`
    && repoData?.owner?.login === LOGIN && repoData?.defaultBranchRef?.name === 'main'
    && repoData?.viewerPermission === 'ADMIN' && repoData?.hasIssuesEnabled === true
    && typeof repoData?.id === 'string' && repoData.id !== String(repoData?.databaseId)
    && typeof repoData?.databaseId === 'number' && repoData.databaseId > 0);
  const repoId = repoData?.id;
  res = await gql('query { repository(owner: "forge-user", name: "missing") { name } }');
  check('unknown repo is data.repository null with no errors', res.status === 200 && res.json?.data?.repository === null && res.json?.errors === undefined);
  res = await gql('query { repository(owner: "forge-user", name: "probe-repo") { issues(first: 10, states: [OPEN], orderBy: { field: CREATED_AT, direction: DESC }) { nodes { number } pageInfo { hasNextPage endCursor } } } }');
  check('issues OPEN + DESC returns only the open issue', res.status === 200 && eq(res.json?.data?.repository?.issues?.nodes?.map((issue) => issue.number), [2]));
  res = await gql(`query { node(id: "${repoId}") { ... on Repository { name nameWithOwner } } }`);
  check('node(id) round-trips the repository', res.status === 200 && res.json?.data?.node?.name === 'probe-repo');

  // Cursor pagination over 7 issues.
  await post('/api/v3/user/repos', { name: 'gql-page-repo' });
  for (let i = 1; i <= 7; i += 1) await post(`/api/v3/repos/${LOGIN}/gql-page-repo/issues`, { title: `issue ${i}` });
  res = await gql('query { repository(owner: "forge-user", name: "gql-page-repo") { issues(first: 5) { nodes { number } pageInfo { hasNextPage endCursor } } } }');
  const firstPage = res.json?.data?.repository?.issues;
  check('first page: 5 issues, hasNextPage true', eq(firstPage?.nodes?.map((issue) => issue.number), [1, 2, 3, 4, 5]) && firstPage?.pageInfo?.hasNextPage === true && typeof firstPage?.pageInfo?.endCursor === 'string');
  res = await gql(`query { repository(owner: "forge-user", name: "gql-page-repo") { issues(first: 5, after: "${firstPage?.pageInfo?.endCursor}") { nodes { number } pageInfo { hasNextPage endCursor } } } }`);
  const secondPage = res.json?.data?.repository?.issues;
  check('second page: 2 issues, hasNextPage false, no duplicates', eq(secondPage?.nodes?.map((issue) => issue.number), [6, 7]) && secondPage?.pageInfo?.hasNextPage === false);

  // Mutations, read back through REST.
  res = await gql(`mutation { createIssue(input: { repositoryId: "${repoId}", title: "gql issue", body: "via graphql" }) { issue { number id } } }`);
  const createdNumber = res.json?.data?.createIssue?.issue?.number;
  check('createIssue mutation returns number + id', res.status === 200 && typeof createdNumber === 'number');
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/issues/${createdNumber}`);
  check('created issue is visible through REST', res.status === 200 && res.json?.title === 'gql issue');
  const issueId = (await gql(`query { repository(owner: "forge-user", name: "probe-repo") { issue(number: ${createdNumber}) { id } } }`)).json?.data?.repository?.issue?.id;
  res = await gql(`mutation { closeIssue(input: { issueId: "${issueId}" }) { issue { number state } } }`);
  check('closeIssue mutation returns CLOSED', res.status === 200 && res.json?.data?.closeIssue?.issue?.state === 'CLOSED');
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/issues/${createdNumber}`);
  check('closed issue reads closed through REST', res.status === 200 && res.json?.state === 'closed');
  res = await gql(`mutation { addComment(input: { subjectId: "${issueId}", body: "gql comment" }) { commentEdge { node { id body } } } }`);
  check('addComment mutation returns the comment node', res.status === 200 && res.json?.data?.addComment?.commentEdge?.node?.body === 'gql comment');
  res = await get(`/api/v3/repos/${LOGIN}/probe-repo/issues/${createdNumber}/comments`);
  check('added comment is visible through REST', res.status === 200 && res.json?.some((comment) => comment.body === 'gql comment'));

  // Pull request through GraphQL on the pulls repo.
  const pullsRepoId = (await gql('query { repository(owner: "forge-user", name: "pulls-repo") { id } }')).json?.data?.repository?.id;
  await post(`/api/v3/repos/${LOGIN}/pulls-repo/git/refs`, { ref: 'refs/heads/gql-feature', sha: (await get(`/api/v3/repos/${LOGIN}/pulls-repo/branches/main`)).json?.commit?.sha });
  const gqlFeatureCurrent = await get(`/api/v3/repos/${LOGIN}/pulls-repo/contents/a.txt?ref=gql-feature`);
  await request('PUT', `/api/v3/repos/${LOGIN}/pulls-repo/contents/a.txt`, { json: { message: 'gql feature', content: Buffer.from('v5\n', 'utf8').toString('base64'), branch: 'gql-feature', sha: gqlFeatureCurrent.json?.sha } });
  res = await gql(`mutation { createPullRequest(input: { repositoryId: "${pullsRepoId}", baseRefName: "main", headRefName: "gql-feature", title: "gql pr" }) { pullRequest { number id state headRefName baseRefName } } }`);
  const gqlPull = res.json?.data?.createPullRequest?.pullRequest;
  check('createPullRequest mutation opens with head/base', res.status === 200 && gqlPull?.state === 'OPEN' && gqlPull?.headRefName === 'gql-feature' && gqlPull?.baseRefName === 'main');
  res = await get(`/api/v3/repos/${LOGIN}/pulls-repo/pulls/${gqlPull?.number}`);
  check('GraphQL pull request is visible through REST', res.status === 200 && res.json?.title === 'gql pr');
  res = await gql(`mutation { mergePullRequest(input: { pullRequestId: "${gqlPull?.id}", mergeMethod: SQUASH }) { pullRequest { number merged state } } }`);
  check('mergePullRequest mutation merges', res.status === 200 && res.json?.data?.mergePullRequest?.pullRequest?.merged === true);
  res = await get(`/api/v3/repos/${LOGIN}/pulls-repo/pulls/${gqlPull?.number}`);
  check('merged GraphQL pull reads merged through REST', res.status === 200 && res.json?.merged === true && res.json?.state === 'closed');

  // Introspection.
  res = await gql('query { __type(name: "Issue") { fields { name } } }');
  const issueFields = res.json?.data?.__type?.fields?.map((field) => field.name) ?? [];
  check('__type Issue lists id/number/title/state/body', ['id', 'number', 'title', 'state', 'body'].every((name) => issueFields.includes(name)));
  res = await gql('query { __type(name: "PullRequest") { fields { name } } }');
  const pullFields = res.json?.data?.__type?.fields?.map((field) => field.name) ?? [];
  check('__type PullRequest lists the required fields', ['id', 'number', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files'].every((name) => pullFields.includes(name)));
  res = await gql('query { __type(name: "Repository") { fields { name } } }');
  const repoFields = res.json?.data?.__type?.fields?.map((field) => field.name) ?? [];
  check('__type Repository lists the required fields', ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'].every((name) => repoFields.includes(name)));
  res = await gql('query { __type(name: "SearchType") { enumValues { name } } }');
  const searchValues = res.json?.data?.__type?.enumValues?.map((value) => value.name) ?? [];
  check('__type SearchType includes ISSUE', searchValues.includes('ISSUE'));

  // Errors and batching.
  res = await gql('query { viewer { noSuchFieldOnViewer } }');
  check('unknown field is HTTP 200 with errors naming it', res.status === 200 && Array.isArray(res.json?.errors) && res.json.errors.some((error) => error.message.includes('noSuchFieldOnViewer')));
  res = await post('/api/graphql', [{ query: 'query { viewer { login } }' }, { query: `query { repository(owner: "${LOGIN}", name: "probe-repo") { name } }` }]);
  check('a batch body answers an array in order', res.status === 200 && Array.isArray(res.json) && res.json.length === 2 && res.json[0]?.data?.viewer?.login === LOGIN && res.json[1]?.data?.repository?.name === 'probe-repo');
  res = await request('POST', '/api/graphql', { raw: '{broken', headers: { ...defaultHeaders, 'content-type': 'application/json' } });
  check('malformed GraphQL body is 400', res.status === 400);
}

async function mcpCall(body, headers = defaultHeaders) {
  return request('POST', '/mcp', { json: body, headers });
}

/** The JSON a tool result carries, or null when the result is an error text. */
function toolText(res) {
  const text = res.json?.result?.content?.[0]?.text ?? '';
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

async function probeMcp() {
  console.log('MCP');
  let res = await request('POST', '/mcp', { json: { jsonrpc: '2.0', id: 1, method: 'server/discover', params: {} }, headers: {} });
  check('MCP without a token is 401', res.status === 401);
  res = await mcpCall({ jsonrpc: '2.0', id: 1, method: 'server/discover', params: {} });
  check('server/discover announces 2026-07-28', res.status === 200 && res.json?.result?.supportedVersions?.includes('2026-07-28') && res.json?.id === 1);
  res = await mcpCall({ jsonrpc: '2.0', id: 2, method: 'initialize', params: { protocolVersion: '2026-07-28', capabilities: {}, clientInfo: { name: 'probe', version: '0' } } });
  check('initialize echoes the offered protocol version', res.status === 200 && res.json?.result?.protocolVersion === '2026-07-28' && res.json?.id === 2);
  res = await mcpCall({ jsonrpc: '2.0', method: 'notifications/initialized', params: {} });
  check('notifications/initialized is 202 with no body', res.status === 202 && res.text === '');
  const list1 = await mcpCall({ jsonrpc: '2.0', id: 3, method: 'tools/list', params: {} });
  const list2 = await mcpCall({ jsonrpc: '2.0', id: 4, method: 'tools/list', params: {} });
  check('tools/list is byte-identical across calls with 6 tools', list1.status === 200 && JSON.stringify(list1.json?.result) === JSON.stringify(list2.json?.result) && list1.json?.result?.tools?.length === 6);
  check('every tool has a description and an object inputSchema', list1.json?.result?.tools?.every((tool) => typeof tool.description === 'string' && tool.description !== '' && tool.inputSchema?.type === 'object'));

  res = await mcpCall({ jsonrpc: '2.0', id: 5, method: 'tools/call', params: { name: 'create_repo', arguments: { name: 'mcp-repo' } } });
  check('create_repo returns full_name', res.status === 200 && toolText(res)?.full_name === `${LOGIN}/mcp-repo`);
  res = await mcpCall({ jsonrpc: '2.0', id: 6, method: 'tools/call', params: { name: 'get_repo', arguments: { owner: LOGIN, repo: 'mcp-repo' } } });
  check('get_repo text includes the repo name', res.status === 200 && res.json?.result?.content?.[0]?.text?.includes('mcp-repo'));
  res = await mcpCall({ jsonrpc: '2.0', id: 7, method: 'tools/call', params: { name: 'create_issue', arguments: { owner: LOGIN, repo: 'mcp-repo', title: 'mcp issue' } } });
  check('create_issue result includes a number', res.status === 200 && typeof toolText(res)?.number === 'number');
  res = await mcpCall({ jsonrpc: '2.0', id: 8, method: 'tools/call', params: { name: 'list_issues', arguments: { owner: LOGIN, repo: 'mcp-repo' } } });
  check('list_issues text includes the issue title', res.status === 200 && res.json?.result?.content?.[0]?.text?.includes('mcp issue'));
  // A pull request through MCP: build branches on mcp-repo first.
  await request('PUT', `/api/v3/repos/${LOGIN}/mcp-repo/contents/a.txt`, { json: { message: 'base', content: Buffer.from('v1\n', 'utf8').toString('base64') } });
  const mcpBase = (await get(`/api/v3/repos/${LOGIN}/mcp-repo/branches/main`)).json?.commit?.sha;
  await post(`/api/v3/repos/${LOGIN}/mcp-repo/git/refs`, { ref: 'refs/heads/mcp-feature', sha: mcpBase });
  const mcpFeatureCurrent = await get(`/api/v3/repos/${LOGIN}/mcp-repo/contents/a.txt?ref=mcp-feature`);
  await request('PUT', `/api/v3/repos/${LOGIN}/mcp-repo/contents/a.txt`, { json: { message: 'mcp feature', content: Buffer.from('v2\n', 'utf8').toString('base64'), branch: 'mcp-feature', sha: mcpFeatureCurrent.json?.sha } });
  res = await mcpCall({ jsonrpc: '2.0', id: 9, method: 'tools/call', params: { name: 'create_pull_request', arguments: { owner: LOGIN, repo: 'mcp-repo', title: 'mcp pr', head: 'mcp-feature', base: 'main' } } });
  const mcpPullNumber = toolText(res)?.number;
  check('create_pull_request result includes a number', res.status === 200 && typeof mcpPullNumber === 'number');
  res = await mcpCall({ jsonrpc: '2.0', id: 10, method: 'tools/call', params: { name: 'merge_pull_request', arguments: { owner: LOGIN, repo: 'mcp-repo', pull_number: mcpPullNumber } } });
  check('merge_pull_request merges', res.status === 200 && toolText(res)?.merged === true);
  res = await mcpCall({ jsonrpc: '2.0', id: 11, method: 'tools/call', params: { name: 'no_such_tool', arguments: {} } });
  check('unknown tool is -32601 on HTTP 200', res.status === 200 && res.json?.error?.code === -32601);
  res = await mcpCall({ jsonrpc: '2.0', id: 12, method: 'tools/call', params: { name: 'create_issue', arguments: { owner: LOGIN, repo: 'mcp-repo' } } });
  check('missing required argument is an isError tool result', res.status === 200 && res.json?.result?.isError === true && res.json?.result?.content?.[0]?.text?.includes('Missing required argument'));
  res = await request('POST', '/mcp', { raw: '{"jsonrpc":"2.0","id":99,"method":', headers: { ...defaultHeaders, 'content-type': 'application/json' } });
  check('malformed MCP body is 400 with -32700 and the id', res.status === 400 && res.json?.error?.code === -32700 && res.json?.id === 99);
  // Concurrent calls keep their own ids.
  const [a, b] = await Promise.all([
    mcpCall({ jsonrpc: '2.0', id: 101, method: 'tools/list', params: {} }),
    mcpCall({ jsonrpc: '2.0', id: 102, method: 'tools/list', params: {} }),
  ]);
  check('concurrent calls echo their own ids', a.json?.id === 101 && b.json?.id === 102);
}

// -------------------------------------------------------------------------------------------
// run
// -------------------------------------------------------------------------------------------

const server = createApp();
await new Promise((resolve) => server.listen(0, resolve));
const address = server.address();
base = `http://127.0.0.1:${address.port}`;
try {
  await probeRest();
  await probeGit();
  await probePulls();
  await probeGraphql();
  await probeMcp();
} finally {
  server.close();
}
console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) {
  console.log('failed probes:');
  for (const name of failures) console.log(`  - ${name}`);
  process.exit(1);
}
