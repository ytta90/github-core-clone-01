package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var errExists = errors.New("already exists")
var errNotFound = errors.New("not found")
var errStaleSHA = errors.New("stale sha")
var errConflict = errors.New("conflict")
var errBadLabel = errors.New("unknown label")

type label struct {
	name  string
	color string
}

type comment struct {
	id   int
	body string
}

type issue struct {
	id       int
	number   int
	title    string
	body     string
	state    string // open | closed
	labels   []*label
	comments []*comment
	seq      int64 // creation order
	// cached serialized JSON of this issue (invalidated on PATCH)
	jsonCache []byte
}

type pr struct {
	id             int
	number         int
	title          string
	body           string
	state          string // open | closed
	merged         bool
	mergedAt       string
	mergeCommitSHA string
	headRef        string
	headSHA        string
	baseRef        string
	baseSHA        string
	seq            int64
}

type fileEntry struct {
	oid  string
	mode string
}

type repo struct {
	id            int
	owner         string
	name          string
	description   string
	private       bool
	defaultBranch string
	refs          map[string]string // ref name -> oid
	objects       map[string]*gitObject
	issues        map[int]*issue
	prs           map[int]*pr
	labels        map[string]*label
	nextNum       int
	nextCommentID int
	// cached serialized JSON of this repo (invalidated on PATCH)
	jsonCache []byte
}

type packCacheEntry struct {
	pack      []byte
	shallow   []string
	unshallow []string
}

type store struct {
	mu        sync.RWMutex
	repos     map[string]*repo
	nextID    int
	tsCounter int64 // monotonic commit timestamp
	seq       int64 // creation sequence
	token     string
	login     string
	name      string
	email     string
	packCache map[string]*packCacheEntry
	packMu    sync.Mutex // guards packCache only
}

func newStore(token string) *store {
	login, name, email := viewerIdentity()
	return &store{
		repos:     make(map[string]*repo),
		nextID:    1,
		tsCounter: 1700000000,
		token:     token,
		login:     login,
		name:      name,
		email:     email,
		packCache: make(map[string]*packCacheEntry),
	}
}

func repoKey(owner, name string) string { return owner + "/" + name }

func (s *store) nextTS() int64 { return atomic.AddInt64(&s.tsCounter, 1) }

func (s *store) nextSeq() int64 { return atomic.AddInt64(&s.seq, 1) }

// ---- repo CRUD ----

func (s *store) createRepo(owner, name, desc string, priv bool) (*repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if _, ok := s.repos[key]; ok {
		return nil, errExists
	}
	r := &repo{
		id:            s.nextID,
		owner:         owner,
		name:          name,
		description:   desc,
		private:       priv,
		defaultBranch: "main",
		refs:          make(map[string]string),
		objects:       make(map[string]*gitObject),
		issues:        make(map[int]*issue),
		prs:           make(map[int]*pr),
		labels:        make(map[string]*label),
		nextNum:       1,
		nextCommentID: 1,
	}
	s.nextID++
	s.repos[key] = r
	return r, nil
}

func (s *store) getRepo(owner, name string) *repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repos[repoKey(owner, name)]
}

func (s *store) deleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	return true
}

// userRepos returns all repos owned by login, sorted by name, for GET /user/repos.
func (s *store) userRepos(login string) []*repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*repo
	for _, r := range s.repos {
		if r.owner == login {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// ---- git object helpers (caller holds the store lock) ----

func (s *store) putObject(r *repo, typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &gitObject{typ: typ, data: data}
	}
	return oid
}

func (s *store) commitObject(r *repo, tree string, parents []string, message string) string {
	var b strings.Builder
	b.WriteString("tree ")
	b.WriteString(tree)
	b.WriteByte('\n')
	for _, p := range parents {
		b.WriteString("parent ")
		b.WriteString(p)
		b.WriteByte('\n')
	}
	ts := s.nextTS()
	b.WriteString("author ")
	b.WriteString(s.name)
	b.WriteString(" <")
	b.WriteString(s.email)
	b.WriteString("> ")
	b.WriteString(strconv.FormatInt(ts, 10))
	b.WriteString(" +0000\n")
	b.WriteString("committer ")
	b.WriteString(s.name)
	b.WriteString(" <")
	b.WriteString(s.email)
	b.WriteString("> ")
	b.WriteString(strconv.FormatInt(ts, 10))
	b.WriteString(" +0000\n\n")
	b.WriteString(message)
	return s.putObject(r, "commit", []byte(b.String()))
}

// flattenTree returns path -> fileEntry for the whole tree.
func (s *store) flattenTree(r *repo, treeOID string) map[string]fileEntry {
	out := make(map[string]fileEntry)
	s.flattenInto(r, treeOID, "", out)
	return out
}

func (s *store) flattenInto(r *repo, treeOID, prefix string, out map[string]fileEntry) {
	obj := r.objects[treeOID]
	if obj == nil || obj.typ != "tree" {
		return
	}
	for _, e := range parseTree(obj.data) {
		path := e.name
		if prefix != "" {
			path = prefix + "/" + e.name
		}
		if e.mode == "40000" {
			s.flattenInto(r, e.oid, path, out)
		} else {
			out[path] = fileEntry{oid: e.oid, mode: e.mode}
		}
	}
}

// rebuildTree builds (and stores) nested trees for a flat path map.
func (s *store) rebuildTree(r *repo, flat map[string]fileEntry) string {
	return s.buildTreeRec(r, flat, "")
}

func (s *store) buildTreeRec(r *repo, flat map[string]fileEntry, prefix string) string {
	// collect direct children of this directory
	dirs := map[string]map[string]fileEntry{}
	var files []treeEntry
	for path, fe := range flat {
		rel := path
		if prefix != "" {
			if !strings.HasPrefix(path, prefix+"/") {
				continue
			}
			rel = path[len(prefix)+1:]
		}
		if idx := strings.IndexByte(rel, '/'); idx >= 0 {
			dir := rel[:idx]
			sub, ok := dirs[dir]
			if !ok {
				sub = make(map[string]fileEntry)
				dirs[dir] = sub
			}
			sub[path] = fe
		} else {
			files = append(files, treeEntry{mode: fe.mode, name: rel, oid: fe.oid})
		}
	}
	for dir, sub := range dirs {
		subPrefix := dir
		if prefix != "" {
			subPrefix = prefix + "/" + dir
		}
		oid := s.buildTreeRec(r, sub, subPrefix)
		files = append(files, treeEntry{mode: "40000", name: dir, oid: oid})
	}
	return s.putObject(r, "tree", buildTree(files))
}

// commitHistory returns commit oids reachable from tip, newest first
// (BFS discovery order; linear histories are exactly newest-first).
func (s *store) commitHistory(r *repo, tip string) []string {
	if tip == "" || tip == zeroOID {
		return nil
	}
	var out []string
	seen := map[string]bool{tip: true}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		obj := r.objects[oid]
		if obj == nil || obj.typ != "commit" {
			continue
		}
		out = append(out, oid)
		cv := parseCommit(obj.data)
		for _, p := range cv.parents {
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return out
}

// isAncestor reports whether anc is reachable from desc (including equality).
func (s *store) isAncestor(r *repo, anc, desc string) bool {
	if anc == desc {
		return true
	}
	if anc == "" || desc == "" {
		return false
	}
	seen := map[string]bool{desc: true}
	queue := []string{desc}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		obj := r.objects[oid]
		if obj == nil {
			continue
		}
		var parents []string
		switch obj.typ {
		case "commit":
			parents = parseCommit(obj.data).parents
		case "tag":
			parents = []string{parseTag(obj.data).object}
		}
		for _, p := range parents {
			if p == anc {
				return true
			}
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return false
}

// mergeBase returns the newest common ancestor of a and b (BFS from a).
func (s *store) mergeBase(r *repo, a, b string) string {
	if a == "" || b == "" || a == zeroOID || b == zeroOID {
		return ""
	}
	// ancestor set of b
	ancB := map[string]bool{}
	queue := []string{b}
	ancB[b] = true
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		obj := r.objects[oid]
		if obj == nil || obj.typ != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.data).parents {
			if !ancB[p] {
				ancB[p] = true
				queue = append(queue, p)
			}
		}
	}
	// BFS from a
	seen := map[string]bool{a: true}
	queue = []string{a}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if ancB[oid] {
			return oid
		}
		obj := r.objects[oid]
		if obj == nil || obj.typ != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.data).parents {
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return ""
}

// refTarget resolves a ref name to a commit oid, following annotated tags.
func (s *store) refTarget(r *repo, ref string) string {
	oid := r.refs[ref]
	if oid == "" {
		return ""
	}
	// peel tags
	for {
		obj := r.objects[oid]
		if obj == nil || obj.typ != "tag" {
			return oid
		}
		oid = parseTag(obj.data).object
	}
}

// ---- pack generation ----

// packFor computes the pack for a fetch. Caller must hold the store lock
// (read or write); the pack cache has its own mutex. wants/haves are commit
// oids. depth 0 = unlimited. shallows = client's previous shallow boundaries.
func (s *store) packFor(r *repo, wants, haves []string, depth int, shallows []string) ([]byte, []string, []string) {
	// cache key
	var kb strings.Builder
	kb.WriteString(fmt.Sprintf("%d|", r.id))
	for _, w := range wants {
		kb.WriteString(w)
		kb.WriteByte(',')
	}
	kb.WriteByte('|')
	for _, h := range haves {
		kb.WriteString(h)
		kb.WriteByte(',')
	}
	kb.WriteString(fmt.Sprintf("|%d|", depth))
	for _, sh := range shallows {
		kb.WriteString(sh)
		kb.WriteByte(',')
	}
	key := kb.String()

	s.packMu.Lock()
	if ent, ok := s.packCache[key]; ok {
		s.packMu.Unlock()
		return ent.pack, ent.shallow, ent.unshallow
	}
	s.packMu.Unlock()

	// walk
	send := map[string]bool{} // object oids to send
	dist := map[string]int{}  // commit distance from a want
	type item struct {
		oid string
		d   int
	}
	queue := []item{}
	for _, w := range wants {
		if w != "" && w != zeroOID {
			if _, ok := dist[w]; !ok {
				dist[w] = 0
				queue = append(queue, item{w, 0})
			}
		}
	}
	// closure of haves (full, unlimited) — but a shallow boundary held by the
	// client has no parents on the client, so the closure stops there: the
	// client does NOT have the boundary's ancestors.
	shallowSet := map[string]bool{}
	for _, sh := range shallows {
		shallowSet[sh] = true
	}
	haveSet := map[string]bool{}
	{
		hq := []string{}
		for _, h := range haves {
			if h != "" && h != zeroOID && !haveSet[h] {
				haveSet[h] = true
				hq = append(hq, h)
			}
		}
		for len(hq) > 0 {
			oid := hq[0]
			hq = hq[1:]
			obj := r.objects[oid]
			if obj == nil {
				continue
			}
			switch obj.typ {
			case "commit":
				cv := parseCommit(obj.data)
				if !haveSet[cv.tree] {
					haveSet[cv.tree] = true
					hq = append(hq, cv.tree)
				}
				if !shallowSet[oid] {
					for _, p := range cv.parents {
						if !haveSet[p] {
							haveSet[p] = true
							hq = append(hq, p)
						}
					}
				}
			case "tag":
				t := parseTag(obj.data).object
				if !haveSet[t] {
					haveSet[t] = true
					hq = append(hq, t)
				}
			case "tree":
				for _, e := range parseTree(obj.data) {
					if !haveSet[e.oid] {
						haveSet[e.oid] = true
						hq = append(hq, e.oid)
					}
				}
			}
		}
	}
	var addObj func(oid string)
	addObj = func(oid string) {
		if oid == "" || haveSet[oid] || send[oid] {
			return
		}
		obj := r.objects[oid]
		if obj == nil {
			return
		}
		send[oid] = true
		if obj.typ == "commit" {
			cv := parseCommit(obj.data)
			addObj(cv.tree)
		} else if obj.typ == "tree" {
			for _, e := range parseTree(obj.data) {
				addObj(e.oid)
			}
		} else if obj.typ == "tag" {
			addObj(parseTag(obj.data).object)
		}
	}
	var shallowList []string
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if depth > 0 && it.d >= depth {
			continue
		}
		obj := r.objects[it.oid]
		if obj == nil {
			continue
		}
		if obj.typ != "commit" {
			// tags / stray objects: send them (unless held), no traversal
			addObj(it.oid)
			continue
		}
		cv := parseCommit(obj.data)
		sent := !haveSet[it.oid]
		if sent {
			addObj(it.oid)
			if depth > 0 && it.d == depth-1 {
				// boundary: parents stay server-side
				if len(cv.parents) > 0 {
					shallowList = append(shallowList, it.oid)
				}
				continue
			}
		}
		// traverse parents even for held commits: a shallow boundary held by
		// the client has parents the client does not have (deepen case)
		for _, p := range cv.parents {
			if _, ok := dist[p]; !ok {
				dist[p] = it.d + 1
				queue = append(queue, item{p, it.d + 1})
			}
		}
	}
	// unshallow: old shallow boundaries whose parent is now sent
	var unshallowList []string
	for _, sh := range shallows {
		obj := r.objects[sh]
		if obj == nil || obj.typ != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.data).parents {
			if send[p] {
				unshallowList = append(unshallowList, sh)
				break
			}
		}
	}
	// order objects: commits first (in walk order), then the rest
	var objs []*gitObject
	for _, w := range wants {
		if send[w] {
			if o := r.objects[w]; o != nil {
				objs = append(objs, o)
			}
		}
	}
	// collect remaining in deterministic order: iterate send map via sorted oids
	var rest []string
	for oid := range send {
		if o := r.objects[oid]; o != nil && o.typ != "commit" {
			rest = append(rest, oid)
		}
	}
	sort.Strings(rest)
	for _, oid := range rest {
		objs = append(objs, r.objects[oid])
	}
	// commits not in wants (ancestors) — append in dist order
	var extraCommits []string
	for oid := range send {
		if o := r.objects[oid]; o != nil && o.typ == "commit" {
			found := false
			for _, w := range wants {
				if w == oid {
					found = true
					break
				}
			}
			if !found {
				extraCommits = append(extraCommits, oid)
			}
		}
	}
	sort.Slice(extraCommits, func(i, j int) bool { return dist[extraCommits[i]] < dist[extraCommits[j]] })
	// rebuild: wants-commits, extra commits, then trees/blobs
	var final []*gitObject
	for _, o := range objs {
		if o.typ == "commit" {
			final = append(final, o)
		}
	}
	for _, oid := range extraCommits {
		final = append(final, r.objects[oid])
	}
	for _, o := range objs {
		if o.typ != "commit" {
			final = append(final, o)
		}
	}
	pack := writePack(final)

	s.packMu.Lock()
	if len(s.packCache) > 512 {
		s.packCache = make(map[string]*packCacheEntry)
	}
	s.packCache[key] = &packCacheEntry{pack: pack, shallow: shallowList, unshallow: unshallowList}
	s.packMu.Unlock()
	return pack, shallowList, unshallowList
}

// ---- issues / PRs / labels (caller holds the write lock) ----

func (s *store) createIssueLocked(r *repo, title, body string, labelNames []string) (*issue, error) {
	var labs []*label
	for _, n := range labelNames {
		l, ok := r.labels[n]
		if !ok {
			return nil, errBadLabel
		}
		labs = append(labs, l)
	}
	is := &issue{
		id:     s.nextID,
		number: r.nextNum,
		title:  title,
		body:   body,
		state:  "open",
		labels: labs,
		seq:    s.nextSeq(),
	}
	s.nextID++
	r.nextNum++
	r.issues[is.number] = is
	return is, nil
}

func (s *store) createPRLocked(r *repo, head, base, title, body string) (*pr, error) {
	headOID := s.refTarget(r, "refs/heads/"+head)
	baseOID := s.refTarget(r, "refs/heads/"+base)
	for _, p := range r.prs {
		if p.state == "open" && p.headRef == head && p.baseRef == base {
			return nil, errExists
		}
	}
	p := &pr{
		id:      s.nextID,
		number:  r.nextNum,
		title:   title,
		body:    body,
		state:   "open",
		headRef: head,
		headSHA: headOID,
		baseRef: base,
		baseSHA: baseOID,
		seq:     s.nextSeq(),
	}
	s.nextID++
	r.nextNum++
	r.prs[p.number] = p
	return p, nil
}

// prConflict reports whether a PR's head/base conflict (3-way).
func (s *store) prConflictLocked(r *repo, p *pr) bool {
	headTip := s.refTarget(r, "refs/heads/"+p.headRef)
	baseTip := s.refTarget(r, "refs/heads/"+p.baseRef)
	if headTip == "" || baseTip == "" {
		return false
	}
	mb := s.mergeBase(r, headTip, baseTip)
	if mb == "" {
		return false
	}
	mbObj := r.objects[mb]
	if mbObj == nil {
		return false
	}
	baseFlat := s.flattenTree(r, parseCommit(r.objects[baseTip].data).tree)
	headFlat := s.flattenTree(r, parseCommit(r.objects[headTip].data).tree)
	mbFlat := s.flattenTree(r, parseCommit(mbObj.data).tree)
	for path, mbFE := range mbFlat {
		headFE, inHead := headFlat[path]
		baseFE, inBase := baseFlat[path]
		headChanged := !inHead || headFE.oid != mbFE.oid
		baseChanged := !inBase || baseFE.oid != mbFE.oid
		if headChanged && baseChanged {
			return true
		}
	}
	// paths added on both sides
	for path, headFE := range headFlat {
		if _, inMB := mbFlat[path]; !inMB {
			if baseFE, inBase := baseFlat[path]; inBase && baseFE.oid != headFE.oid {
				return true
			}
		}
	}
	return false
}

// mergePRLocked performs a squash merge. Returns error errConflict on conflict.
func (s *store) mergePRLocked(r *repo, p *pr) error {
	headTip := s.refTarget(r, "refs/heads/"+p.headRef)
	baseTip := s.refTarget(r, "refs/heads/"+p.baseRef)
	if headTip == "" || baseTip == "" {
		return errNotFound
	}
	if s.prConflictLocked(r, p) {
		return errConflict
	}
	mb := s.mergeBase(r, headTip, baseTip)
	var merged map[string]fileEntry
	baseFlat := s.flattenTree(r, parseCommit(r.objects[baseTip].data).tree)
	if mb == "" {
		merged = s.flattenTree(r, parseCommit(r.objects[headTip].data).tree)
	} else {
		headFlat := s.flattenTree(r, parseCommit(r.objects[headTip].data).tree)
		mbFlat := s.flattenTree(r, parseCommit(r.objects[mb].data).tree)
		merged = make(map[string]fileEntry, len(baseFlat))
		for k, v := range baseFlat {
			merged[k] = v
		}
		// apply head's changes vs merge-base
		for path, headFE := range headFlat {
			mbFE, inMB := mbFlat[path]
			if !inMB || headFE.oid != mbFE.oid {
				merged[path] = headFE
			}
		}
		for path := range mbFlat {
			if _, inHead := headFlat[path]; !inHead {
				if mbFE, inMB := mbFlat[path]; inMB {
					if baseFE, inBase := baseFlat[path]; inBase && baseFE.oid == mbFE.oid {
						delete(merged, path)
					}
				}
			}
		}
	}
	treeOID := s.rebuildTree(r, merged)
	msg := fmt.Sprintf("Merge pull request #%d from %s", p.number, p.headRef)
	mergeOID := s.commitObject(r, treeOID, []string{baseTip}, msg)
	r.refs["refs/heads/"+p.baseRef] = mergeOID
	p.state = "closed"
	p.merged = true
	p.mergedAt = "2026-11-18T00:00:00Z"
	p.mergeCommitSHA = mergeOID
	return nil
}

// putContentsLocked creates/updates a file. Returns the new commit oid.
func (s *store) putContentsLocked(r *repo, path, message string, content []byte, branch, sha string) (string, error) {
	if branch == "" {
		branch = r.defaultBranch
	}
	branchRef := "refs/heads/" + branch
	tip := s.refTarget(r, branchRef)
	var flat map[string]fileEntry
	if tip == "" {
		// branch does not exist: start from the default branch tip (or empty)
		defTip := s.refTarget(r, "refs/heads/"+r.defaultBranch)
		if defTip == "" {
			flat = make(map[string]fileEntry)
		} else {
			flat = s.flattenTree(r, parseCommit(r.objects[defTip].data).tree)
		}
		tip = defTip
	} else {
		flat = s.flattenTree(r, parseCommit(r.objects[tip].data).tree)
	}
	if sha != "" {
		cur, ok := flat[path]
		if !ok || cur.oid != sha {
			return "", errStaleSHA
		}
	}
	blobOID := s.putObject(r, "blob", content)
	flat[path] = fileEntry{oid: blobOID, mode: "100644"}
	treeOID := s.rebuildTree(r, flat)
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	commitOID := s.commitObject(r, treeOID, parents, message)
	r.refs[branchRef] = commitOID
	return commitOID, nil
}

func (s *store) deleteContentsLocked(r *repo, path, message, sha, branch string) (string, error) {
	if branch == "" {
		branch = r.defaultBranch
	}
	branchRef := "refs/heads/" + branch
	tip := s.refTarget(r, branchRef)
	if tip == "" {
		return "", errNotFound
	}
	flat := s.flattenTree(r, parseCommit(r.objects[tip].data).tree)
	cur, ok := flat[path]
	if !ok {
		return "", errNotFound
	}
	if sha != "" && cur.oid != sha {
		return "", errStaleSHA
	}
	delete(flat, path)
	treeOID := s.rebuildTree(r, flat)
	commitOID := s.commitObject(r, treeOID, []string{tip}, message)
	r.refs[branchRef] = commitOID
	return commitOID, nil
}

// ---- serializers (cached where hot) ----

func (s *store) repoJSON(r *repo) []byte {
	if r.jsonCache != nil {
		return r.jsonCache
	}
	var j jb
	j.raw(`{"id":`)
	j.num(r.id)
	j.raw(`,"name":`)
	j.str(r.name)
	j.raw(`,"full_name":`)
	j.str(r.owner + "/" + r.name)
	j.raw(`,"owner":{"login":`)
	j.str(r.owner)
	j.raw(`,"id":`)
	j.num(r.id)
	j.raw(`},"private":`)
	j.boolean(r.private)
	j.raw(`,"description":`)
	if r.description == "" {
		j.raw(`null`)
	} else {
		j.str(r.description)
	}
	j.raw(`,"default_branch":`)
	j.str(r.defaultBranch)
	j.raw(`}`)
	r.jsonCache = j.b
	return r.jsonCache
}

func (s *store) issueJSON(is *issue) []byte {
	if is.jsonCache != nil {
		return is.jsonCache
	}
	var j jb
	j.raw(`{"id":`)
	j.num(is.id)
	j.raw(`,"number":`)
	j.num(is.number)
	j.raw(`,"title":`)
	j.str(is.title)
	j.raw(`,"body":`)
	if is.body == "" {
		j.raw(`""`)
	} else {
		j.str(is.body)
	}
	j.raw(`,"state":`)
	j.str(is.state)
	j.raw(`,"labels":[`)
	for i, l := range is.labels {
		if i > 0 {
			j.raw(`,`)
		}
		j.raw(`{"name":`)
		j.str(l.name)
		j.raw(`,"color":`)
		j.str(l.color)
		j.raw(`}`)
	}
	j.raw(`],"user":{"login":`)
	j.str(s.login)
	j.raw(`,"id":1000}`)
	j.raw(`}`)
	is.jsonCache = j.b
	return is.jsonCache
}

func (s *store) prJSON(p *pr) []byte {
	var j jb
	j.raw(`{"id":`)
	j.num(p.id)
	j.raw(`,"number":`)
	j.num(p.number)
	j.raw(`,"title":`)
	j.str(p.title)
	j.raw(`,"body":`)
	j.str(p.body)
	j.raw(`,"state":`)
	j.str(p.state)
	j.raw(`,"head":{"ref":`)
	j.str(p.headRef)
	j.raw(`,"sha":`)
	j.str(p.headSHA)
	j.raw(`,"label":`)
	j.str(s.login + ":" + p.headRef)
	j.raw(`},"base":{"ref":`)
	j.str(p.baseRef)
	j.raw(`,"sha":`)
	j.str(p.baseSHA)
	j.raw(`,"label":`)
	j.str(s.login + ":" + p.baseRef)
	j.raw(`},"merged":`)
	j.boolean(p.merged)
	j.raw(`,"merged_at":`)
	if p.mergedAt == "" {
		j.raw(`null`)
	} else {
		j.str(p.mergedAt)
	}
	j.raw(`,"merge_commit_sha":`)
	if p.mergeCommitSHA == "" {
		j.raw(`null`)
	} else {
		j.str(p.mergeCommitSHA)
	}
	j.raw(`,"user":{"login":`)
	j.str(s.login)
	j.raw(`,"id":1000}}`)
	return j.b
}

// prFiles returns the diff file list between merge-base and head.
func (s *store) prFiles(r *repo, p *pr) []map[string]any {
	headTip := s.refTarget(r, "refs/heads/"+p.headRef)
	baseTip := s.refTarget(r, "refs/heads/"+p.baseRef)
	if headTip == "" || baseTip == "" {
		return nil
	}
	mb := s.mergeBase(r, headTip, baseTip)
	var baseFlat map[string]fileEntry
	if mb == "" {
		baseFlat = make(map[string]fileEntry)
	} else {
		baseFlat = s.flattenTree(r, parseCommit(r.objects[mb].data).tree)
	}
	headFlat := s.flattenTree(r, parseCommit(r.objects[headTip].data).tree)
	var paths []string
	seen := map[string]bool{}
	for path := range baseFlat {
		paths = append(paths, path)
		seen[path] = true
	}
	for path := range headFlat {
		if !seen[path] {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	var out []map[string]any
	for _, path := range paths {
		baseFE, inBase := baseFlat[path]
		headFE, inHead := headFlat[path]
		if inBase && inHead && baseFE.oid == headFE.oid {
			continue
		}
		status := "modified"
		add, del := 0, 0
		if !inBase && inHead {
			status = "added"
			add = countLines(r.objects[headFE.oid].data)
		} else if inBase && !inHead {
			status = "removed"
			del = countLines(r.objects[baseFE.oid].data)
		} else {
			add = countLines(r.objects[headFE.oid].data)
			del = countLines(r.objects[baseFE.oid].data)
		}
		if add == 0 {
			add = 1
		}
		out = append(out, map[string]any{
			"filename":  path,
			"status":    status,
			"additions": add,
			"deletions": del,
			"changes":   add + del,
		})
	}
	return out
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte("\n"))
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// decodeB64 decodes standard base64 (with or without whitespace/newlines).
func decodeB64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, s)
	return base64.StdEncoding.DecodeString(s)
}

// encodeB64 encodes without line wrapping.
func encodeB64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
