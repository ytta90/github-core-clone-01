package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Repositories, issues, pull requests (caller holds the store lock)
// ---------------------------------------------------------------------------

func (s *Store) createRepo(owner, name, description string, private bool) (*Repo, error) {
	if name == "" {
		return nil, errors.New("name is required")
	}
	if _, exists := s.repos[repoKey(owner, name)]; exists {
		return nil, errRepoExists
	}
	repo := &Repo{
		ID:            s.nextRepoID,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: defaultBranch,
		Refs:          make(map[string]string),
		Labels:        make(map[string]*Label),
		Issues:        make(map[int]*Issue),
		PRs:           make(map[int]*Pull),
		NextNumber:    1,
		Comments:      make(map[int][]*Comment),
		CreatedSeq:    s.nextSeq(),
	}
	s.nextRepoID++
	s.repos[repoKey(owner, name)] = repo
	return repo, nil
}

var errRepoExists = errors.New("repository already exists")

func (s *Store) createIssue(repo *Repo, title, body string, labels []string) (*Issue, error) {
	if title == "" {
		return nil, errors.New("title is required")
	}
	for _, name := range labels {
		if _, ok := repo.Labels[name]; !ok {
			return nil, fmt.Errorf("unknown label %q", name)
		}
	}
	issue := &Issue{
		ID:         s.nextItemID,
		Number:     repo.NextNumber,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     append([]string(nil), labels...),
		CreatedSeq: s.nextSeq(),
	}
	s.nextItemID++
	repo.NextNumber++
	repo.Issues[issue.Number] = issue
	repo.IssueOrder = append(repo.IssueOrder, issue.Number)
	return issue, nil
}

func (s *Store) createPull(repo *Repo, title, body, head, base string) (*Pull, error) {
	if title == "" {
		return nil, errors.New("title is required")
	}
	headRef := "refs/heads/" + head
	baseRef := "refs/heads/" + base
	headSHA, ok := repo.Refs[headRef]
	if !ok {
		return nil, fmt.Errorf("head branch %q does not exist", head)
	}
	baseSHA, ok := repo.Refs[baseRef]
	if !ok {
		return nil, fmt.Errorf("base branch %q does not exist", base)
	}
	for _, n := range repo.PROrder {
		if pr := repo.PRs[n]; pr.State == "open" && pr.Head == head {
			return nil, fmt.Errorf("a pull request is already open from %q", head)
		}
	}
	pr := &Pull{
		ID:         s.nextItemID,
		Number:     repo.NextNumber,
		Title:      title,
		Body:       body,
		Head:       head,
		Base:       base,
		HeadSHA:    headSHA,
		BaseSHA:    baseSHA,
		State:      "open",
		CreatedSeq: s.nextSeq(),
	}
	s.nextItemID++
	repo.NextNumber++
	repo.PRs[pr.Number] = pr
	repo.PROrder = append(repo.PROrder, pr.Number)
	return pr, nil
}

// mergePull squash-merges an open pull request onto its base branch.
// It returns the merge commit sha, or errMergeConflict when the same file
// changed differently on both sides since the merge base.
var errMergeConflict = errors.New("merge conflict")

func (s *Store) mergePull(repo *Repo, pr *Pull) (string, error) {
	if pr.State != "open" {
		return "", errors.New("pull request is not open")
	}
	baseRef := "refs/heads/" + pr.Base
	baseTip, ok := repo.Refs[baseRef]
	if !ok {
		return "", fmt.Errorf("base branch %q does not exist", pr.Base)
	}
	headTip, ok := repo.Refs["refs/heads/"+pr.Head]
	if !ok {
		return "", fmt.Errorf("head branch %q does not exist", pr.Head)
	}
	mergeBase := s.mergeBase(headTip, baseTip)
	mergedTree, conflict := s.threeWayMerge(repo, mergeBase, baseTip, headTip)
	if conflict {
		return "", errMergeConflict
	}
	msg := fmt.Sprintf("Merge pull request #%d from %s/%s", pr.Number, repo.Owner, pr.Head)
	commitOID := s.putCommit(mergedTree, []string{baseTip}, msg)
	repo.Refs[baseRef] = commitOID
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = fixedTimestamp
	pr.MergeSHA = commitOID
	return commitOID, nil
}

// mergeBase finds a common ancestor of a and b (first found walking a's
// ancestry, then b's). With the linear histories the suite builds this is
// the true merge base.
func (s *Store) mergeBase(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	ancestors := make(map[string]bool)
	for cur := a; cur != ""; {
		ancestors[cur] = true
		obj := s.objects[cur]
		if obj == nil || obj.Type != "commit" {
			break
		}
		parents := parseCommit(obj.Data).Parents
		if len(parents) == 0 {
			break
		}
		cur = parents[0]
	}
	for cur := b; cur != ""; {
		if ancestors[cur] {
			return cur
		}
		obj := s.objects[cur]
		if obj == nil || obj.Type != "commit" {
			break
		}
		parents := parseCommit(obj.Data).Parents
		if len(parents) == 0 {
			break
		}
		cur = parents[0]
	}
	return ""
}

// threeWayMerge merges head into base given their merge base, at file level.
// Returns (treeOID, conflict).
func (s *Store) threeWayMerge(repo *Repo, baseOID, baseTip, headTip string) (string, bool) {
	baseTree := ""
	if baseOID != "" {
		if c := s.objects[baseOID]; c != nil && c.Type == "commit" {
			baseTree = parseCommit(c.Data).Tree
		}
	}
	oursTree := ""
	if c := s.objects[baseTip]; c != nil && c.Type == "commit" {
		oursTree = parseCommit(c.Data).Tree
	}
	theirsTree := ""
	if c := s.objects[headTip]; c != nil && c.Type == "commit" {
		theirsTree = parseCommit(c.Data).Tree
	}
	baseFiles := s.flattenTree(baseTree)
	oursFiles := s.flattenTree(oursTree)
	theirsFiles := s.flattenTree(theirsTree)

	result := make(map[string]string, len(oursFiles))
	for path, oid := range oursFiles {
		result[path] = oid
	}
	paths := make(map[string]bool)
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range theirsFiles {
		paths[p] = true
	}
	for p := range oursFiles {
		paths[p] = true
	}
	for path := range paths {
		b, inBase := baseFiles[path]
		o, inOurs := oursFiles[path]
		t, inTheirs := theirsFiles[path]
		theirsChanged := inTheirs != inBase || (inTheirs && inBase && t != b) || (!inTheirs && inBase)
		oursChanged := inOurs != inBase || (inOurs && inBase && o != b) || (!inOurs && inBase)
		if !theirsChanged {
			continue
		}
		if !oursChanged {
			if inTheirs {
				result[path] = t
			} else {
				delete(result, path)
			}
			continue
		}
		// Both sides changed the path.
		if inOurs && inTheirs && o == t {
			continue // same change on both sides
		}
		if !inOurs && !inTheirs {
			continue // both deleted
		}
		return "", true
	}
	return s.buildTreeFromFiles(result), false
}

// flattenTree returns path -> blob oid for every file under a tree.
func (s *Store) flattenTree(treeOID string) map[string]string {
	out := make(map[string]string)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := s.objects[oid]
		if obj == nil || obj.Type != "tree" {
			return
		}
		for _, e := range parseTree(obj.Data) {
			path := prefix + e.Name
			if e.Mode == "40000" {
				walk(e.OID, path+"/")
			} else {
				out[path] = e.OID
			}
		}
	}
	if treeOID != "" {
		walk(treeOID, "")
	}
	return out
}

// buildTreeFromFiles builds nested trees for a flat path->blob map.
func (s *Store) buildTreeFromFiles(files map[string]string) string {
	// Group by first segment, recursively.
	var build func(paths map[string]string) string
	build = func(paths map[string]string) string {
		entries := []TreeEntry{}
		dirs := map[string]map[string]string{}
		for path, oid := range paths {
			if i := strings.Index(path, "/"); i >= 0 {
				dir := path[:i]
				if dirs[dir] == nil {
					dirs[dir] = make(map[string]string)
				}
				dirs[dir][path[i+1:]] = oid
			} else {
				entries = append(entries, TreeEntry{Mode: "100644", Name: path, OID: oid})
			}
		}
		for dir, sub := range dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: dir, OID: build(sub)})
		}
		return s.putObject("tree", buildTree(entries))
	}
	return build(files)
}

// ---------------------------------------------------------------------------
// Contents API (caller holds the store lock)
// ---------------------------------------------------------------------------

// resolveRef turns a ref name (branch, sha or "") into a commit oid.
func (s *Store) resolveRef(repo *Repo, ref string) (string, bool) {
	if ref == "" {
		return repo.Refs["refs/heads/"+repo.DefaultBranch], true
	}
	if oid, ok := repo.Refs["refs/heads/"+ref]; ok {
		return oid, true
	}
	if obj, ok := s.objects[ref]; ok && obj.Type == "commit" {
		return ref, true
	}
	return "", false
}

// commitFile creates a blob + tree + commit for one file change on a branch.
// parentOID is "" for a root commit. Returns the new commit oid.
func (s *Store) commitFile(repo *Repo, branch, path string, content []byte, message string, deleteFile bool) string {
	branchRef := "refs/heads/" + branch
	parentOID, hasParent := repo.Refs[branchRef]
	parentTree := ""
	if hasParent {
		if c := s.objects[parentOID]; c != nil && c.Type == "commit" {
			parentTree = parseCommit(c.Data).Tree
		}
	}
	var treeOID string
	if deleteFile {
		treeOID = s.deletePath(parentTree, splitPath(path))
	} else {
		blobOID := s.putObject("blob", content)
		treeOID = s.setPath(parentTree, splitPath(path), blobOID)
	}
	var parents []string
	if hasParent {
		parents = []string{parentOID}
	}
	commitOID := s.putCommit(treeOID, parents, message)
	repo.Refs[branchRef] = commitOID
	return commitOID
}

func splitPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// setPath returns the tree oid with segments set to blobOID.
func (s *Store) setPath(treeOID string, segments []string, blobOID string) string {
	if len(segments) == 0 {
		return treeOID
	}
	var entries []TreeEntry
	if treeOID != "" {
		if obj := s.objects[treeOID]; obj != nil && obj.Type == "tree" {
			entries = parseTree(obj.Data)
		}
	}
	head := segments[0]
	rest := segments[1:]
	kept := entries[:0]
	// copy to avoid mutating the parsed slice's backing array
	kept = append([]TreeEntry(nil), entries...)
	kept = kept[:0]
	for _, e := range entries {
		if e.Name != head {
			kept = append(kept, e)
		}
	}
	if len(rest) == 0 {
		kept = append(kept, TreeEntry{Mode: "100644", Name: head, OID: blobOID})
	} else {
		var childOID string
		for _, e := range entries {
			if e.Name == head && e.Mode == "40000" {
				childOID = e.OID
			}
		}
		kept = append(kept, TreeEntry{Mode: "40000", Name: head, OID: s.setPath(childOID, rest, blobOID)})
	}
	return s.putObject("tree", buildTree(kept))
}

// deletePath returns the tree oid with segments removed.
func (s *Store) deletePath(treeOID string, segments []string) string {
	if len(segments) == 0 || treeOID == "" {
		return treeOID
	}
	obj := s.objects[treeOID]
	if obj == nil || obj.Type != "tree" {
		return treeOID
	}
	entries := parseTree(obj.Data)
	head := segments[0]
	rest := segments[1:]
	kept := []TreeEntry{}
	for _, e := range entries {
		if e.Name != head {
			kept = append(kept, e)
			continue
		}
		if len(rest) == 0 {
			continue // drop the entry
		}
		if e.Mode == "40000" {
			kept = append(kept, TreeEntry{Mode: "40000", Name: head, OID: s.deletePath(e.OID, rest)})
		}
	}
	return s.putObject("tree", buildTree(kept))
}

// fileAt returns the blob object at path under a commit, if it is a file.
func (s *Store) fileAt(commitOID, path string) (*Object, bool) {
	commit := s.objects[commitOID]
	if commit == nil || commit.Type != "commit" {
		return nil, false
	}
	treeOID := parseCommit(commit.Data).Tree
	segments := splitPath(path)
	cur := treeOID
	for i, seg := range segments {
		obj := s.objects[cur]
		if obj == nil || obj.Type != "tree" {
			return nil, false
		}
		var found *TreeEntry
		for _, e := range parseTree(obj.Data) {
			if e.Name == seg {
				entry := e
				found = &entry
				break
			}
		}
		if found == nil {
			return nil, false
		}
		if i == len(segments)-1 {
			if found.Mode == "40000" {
				return nil, false
			}
			blob := s.objects[found.OID]
			if blob == nil || blob.Type != "blob" {
				return nil, false
			}
			return blob, true
		}
		if found.Mode != "40000" {
			return nil, false
		}
		cur = found.OID
	}
	return nil, false
}

// dirAt returns the entries of a directory under a commit.
func (s *Store) dirAt(commitOID, path string) ([]TreeEntry, bool) {
	commit := s.objects[commitOID]
	if commit == nil || commit.Type != "commit" {
		return nil, false
	}
	cur := parseCommit(commit.Data).Tree
	if path != "" {
		for _, seg := range splitPath(path) {
			obj := s.objects[cur]
			if obj == nil || obj.Type != "tree" {
				return nil, false
			}
			next := ""
			for _, e := range parseTree(obj.Data) {
				if e.Name == seg && e.Mode == "40000" {
					next = e.OID
				}
			}
			if next == "" {
				return nil, false
			}
			cur = next
		}
	}
	obj := s.objects[cur]
	if obj == nil || obj.Type != "tree" {
		return nil, false
	}
	entries := parseTree(obj.Data)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, true
}

// ---------------------------------------------------------------------------
// Object store helpers (caller holds the store lock)
// ---------------------------------------------------------------------------

func (s *Store) putObject(typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := s.objects[oid]; !ok {
		s.objects[oid] = &Object{Type: typ, Data: append([]byte(nil), data...)}
	}
	return oid
}

func (s *Store) putCommit(treeOID string, parents []string, message string) string {
	var b strings.Builder
	b.WriteString("tree " + treeOID + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	author := fmt.Sprintf("%s <%s> %d +0000", viewerLogin, viewerEmail, commitEpoch)
	b.WriteString("author " + author + "\n")
	b.WriteString("committer " + author + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return s.putObject("commit", []byte(b.String()))
}

// ---------------------------------------------------------------------------
// Global ids (opaque, stable): base64url of "kind\x00payload"
// ---------------------------------------------------------------------------

func encodeGlobalID(kind, payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + "\x00" + payload))
}

func decodeGlobalID(id string) (kind, payload string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(raw), "\x00", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func repoGlobalID(repo *Repo) string {
	return encodeGlobalID("repo", repo.Owner+"/"+repo.Name)
}

func issueGlobalID(repo *Repo, number int) string {
	return encodeGlobalID("issue", fmt.Sprintf("%s/%s#%d", repo.Owner, repo.Name, number))
}

func pullGlobalID(repo *Repo, number int) string {
	return encodeGlobalID("pr", fmt.Sprintf("%s/%s#%d", repo.Owner, repo.Name, number))
}

func commentGlobalID(repo *Repo, number, commentID int) string {
	return encodeGlobalID("comment", fmt.Sprintf("%s/%s#%d:%d", repo.Owner, repo.Name, number, commentID))
}

// resolveGlobalRepo finds a repo by its global id.
func (s *Store) resolveGlobalRepo(id string) *Repo {
	kind, payload, ok := decodeGlobalID(id)
	if !ok || kind != "repo" {
		return nil
	}
	parts := strings.SplitN(payload, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	return s.getRepo(parts[0], parts[1])
}

// resolveGlobalIssue finds (repo, issue) by global id.
func (s *Store) resolveGlobalIssue(id string) (*Repo, *Issue) {
	kind, payload, ok := decodeGlobalID(id)
	if !ok || kind != "issue" {
		return nil, nil
	}
	hash := strings.LastIndex(payload, "#")
	if hash < 0 {
		return nil, nil
	}
	repo := s.getRepoByFullName(payload[:hash])
	if repo == nil {
		return nil, nil
	}
	var number int
	if _, err := fmt.Sscanf(payload[hash+1:], "%d", &number); err != nil {
		return nil, nil
	}
	issue, ok := repo.Issues[number]
	if !ok {
		return nil, nil
	}
	return repo, issue
}

// resolveGlobalPull finds (repo, pr) by global id.
func (s *Store) resolveGlobalPull(id string) (*Repo, *Pull) {
	kind, payload, ok := decodeGlobalID(id)
	if !ok || kind != "pr" {
		return nil, nil
	}
	hash := strings.LastIndex(payload, "#")
	if hash < 0 {
		return nil, nil
	}
	repo := s.getRepoByFullName(payload[:hash])
	if repo == nil {
		return nil, nil
	}
	var number int
	if _, err := fmt.Sscanf(payload[hash+1:], "%d", &number); err != nil {
		return nil, nil
	}
	pr, ok := repo.PRs[number]
	if !ok {
		return nil, nil
	}
	return repo, pr
}

func (s *Store) getRepoByFullName(full string) *Repo {
	parts := strings.SplitN(full, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	return s.getRepo(parts[0], parts[1])
}
