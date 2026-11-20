package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Dispatcher
// ---------------------------------------------------------------------------

func (srv *server) dispatch(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			srv.writeErr(w, r, http.StatusInternalServerError, fmt.Sprintf("internal error: %v", rec))
		}
	}()
	path := r.URL.Path
	method := r.Method

	if method == http.MethodGet && path == "/healthz" {
		srv.writeJSON(w, r, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if path == "/api/graphql" {
		srv.handleGraphQL(w, r)
		return
	}
	if path == "/mcp" {
		srv.handleMCP(w, r)
		return
	}
	if strings.HasPrefix(path, "/api/v3") {
		srv.handleREST(w, r, strings.TrimPrefix(path, "/api/v3"))
		return
	}
	// Git smart HTTP: /{owner}/{repo}.git/...
	if strings.Contains(path, ".git/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), ".git/", 2)
		if len(parts) == 2 {
			ownerRepo := strings.Split(parts[0], "/")
			if len(ownerRepo) == 2 {
				srv.handleGit(w, r, ownerRepo[0], ownerRepo[1], "/"+parts[1])
				return
			}
		}
	}
	srv.writeErr(w, r, http.StatusNotFound, "Not Found")
}

// ---------------------------------------------------------------------------
// REST helpers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// REST response shapes (structs, not maps: the hot paths marshal these
// thousands of times per trial and struct encoding allocates far less).
// ---------------------------------------------------------------------------

type ownerResponse struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type,omitempty"`
}

type repoResponse struct {
	ID            int           `json:"id"`
	Name          string        `json:"name"`
	FullName      string        `json:"full_name"`
	Owner         ownerResponse `json:"owner"`
	Private       bool          `json:"private"`
	Description   string        `json:"description"`
	DefaultBranch string        `json:"default_branch"`
	HTMLURL       string        `json:"html_url"`
	URL           string        `json:"url"`
	CreatedAt     string        `json:"created_at"`
	UpdatedAt     string        `json:"updated_at"`
	PushedAt      string        `json:"pushed_at"`
	HasIssues     bool          `json:"has_issues"`
	HasProjects   bool          `json:"has_projects"`
	HasWiki       bool          `json:"has_wiki"`
	Fork          bool          `json:"fork"`
	Archived      bool          `json:"archived"`
	Disabled      bool          `json:"disabled"`
	Visibility    string        `json:"visibility"`
}

type labelResponse struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type issueResponse struct {
	ID        int             `json:"id"`
	Number    int             `json:"number"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	State     string          `json:"state"`
	Labels    []labelResponse `json:"labels"`
	User      ownerResponse   `json:"user"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	ClosedAt  any             `json:"closed_at"`
	Comments  int             `json:"comments"`
	HTMLURL   string          `json:"html_url"`
}

type pullRefResponse struct {
	Ref   string        `json:"ref"`
	SHA   string        `json:"sha"`
	Label string        `json:"label"`
	User  ownerResponse `json:"user"`
	Repo  struct {
		Name string `json:"name"`
	} `json:"repo"`
}

type pullResponse struct {
	ID             int             `json:"id"`
	Number         int             `json:"number"`
	Title          string          `json:"title"`
	Body           string          `json:"body"`
	State          string          `json:"state"`
	Head           pullRefResponse `json:"head"`
	Base           pullRefResponse `json:"base"`
	Merged         bool            `json:"merged"`
	MergedAt       any             `json:"merged_at"`
	MergeCommitSHA any             `json:"merge_commit_sha"`
	Mergeable      bool            `json:"mergeable"`
	User           ownerResponse   `json:"user"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	ClosedAt       any             `json:"closed_at"`
	HTMLURL        string          `json:"html_url"`
}

// repoJSON renders the public repository object.
func repoJSON(repo *Repo) repoResponse {
	return repoResponse{
		ID:            repo.ID,
		Name:          repo.Name,
		FullName:      repo.Owner + "/" + repo.Name,
		Owner:         ownerResponse{Login: repo.Owner, ID: viewerID, Type: "User"},
		Private:       repo.Private,
		Description:   repo.Description,
		DefaultBranch: repo.DefaultBranch,
		HTMLURL:       "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		URL:           "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
		CreatedAt:     fixedTimestamp,
		UpdatedAt:     fixedTimestamp,
		PushedAt:      fixedTimestamp,
		HasIssues:     true,
		HasProjects:   true,
		HasWiki:       true,
		Visibility:    visibilityOf(repo.Private),
	}
}

func visibilityOf(private bool) string {
	if private {
		return "private"
	}
	return "public"
}

func issueJSON(repo *Repo, issue *Issue) issueResponse {
	labels := make([]labelResponse, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color := "ededed"
		if l, ok := repo.Labels[name]; ok && l.Color != "" {
			color = l.Color
		}
		labels = append(labels, labelResponse{ID: 1, Name: name, Color: color})
	}
	return issueResponse{
		ID:        issue.ID,
		Number:    issue.Number,
		Title:     issue.Title,
		Body:      issue.Body,
		State:     issue.State,
		Labels:    labels,
		User:      ownerResponse{Login: viewerLogin, ID: viewerID},
		CreatedAt: fixedTimestamp,
		UpdatedAt: fixedTimestamp,
		ClosedAt:  nil,
		Comments:  len(repo.Comments[issue.Number]),
		HTMLURL:   "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

func pullJSON(repo *Repo, pr *Pull) pullResponse {
	head := pullRefResponse{Ref: pr.Head, SHA: pr.HeadSHA, Label: repo.Owner + ":" + pr.Head, User: ownerResponse{Login: repo.Owner}}
	head.Repo.Name = repo.Name
	base := pullRefResponse{Ref: pr.Base, SHA: pr.BaseSHA, Label: repo.Owner + ":" + pr.Base, User: ownerResponse{Login: repo.Owner}}
	base.Repo.Name = repo.Name
	out := pullResponse{
		ID:        pr.ID,
		Number:    pr.Number,
		Title:     pr.Title,
		Body:      pr.Body,
		State:     pr.State,
		Head:      head,
		Base:      base,
		Merged:    pr.Merged,
		Mergeable: true,
		User:      ownerResponse{Login: viewerLogin, ID: viewerID},
		CreatedAt: fixedTimestamp,
		UpdatedAt: fixedTimestamp,
		HTMLURL:   "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pr.Number),
	}
	if pr.Merged {
		out.MergedAt = pr.MergedAt
		out.MergeCommitSHA = pr.MergeSHA
	}
	if pr.State == "closed" {
		out.ClosedAt = fixedTimestamp
	}
	return out
}

func nullableString(ok bool, v string) any {
	if !ok {
		return nil
	}
	return v
}

// lookupRepo resolves owner/name for a REST call: 404 when the repo is not
// the viewer's or does not exist; 401 when anonymous on a private repo.
func (srv *server) lookupRepo(w http.ResponseWriter, r *http.Request, owner, name string) *Repo {
	srv.store.mu.RLock()
	repo := srv.store.getRepo(owner, name)
	srv.store.mu.RUnlock()
	if repo == nil {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return nil
	}
	if !srv.store.isViewerToken(r) && repo.Private {
		srv.writeErr(w, r, http.StatusUnauthorized, "Requires authentication")
		return nil
	}
	return repo
}

// requireAuth answers 401 unless the request carries the trial token.
func (srv *server) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if !srv.store.isViewerToken(r) {
		srv.writeErr(w, r, http.StatusUnauthorized, "Requires authentication")
		return false
	}
	return true
}

// paginate slices items per page/per_page query params and returns the page
// plus a GitHub-style Link header value ("" when a single page).
func paginate(r *http.Request, total int) (page, perPage int, link string) {
	q := r.URL.Query()
	page = atoiOr(q.Get("page"), 1)
	perPage = atoiOr(q.Get("per_page"), 30)
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}
	lastPage := (total + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	if total == 0 || lastPage == 1 {
		return page, perPage, ""
	}
	base := "http://" + r.Host + r.URL.Path
	var parts []string
	mk := func(p int, rel string) string {
		v := url.Values{}
		v.Set("page", strconv.Itoa(p))
		v.Set("per_page", strconv.Itoa(perPage))
		return "<" + base + "?" + v.Encode() + ">; rel=\"" + rel + "\""
	}
	if page < lastPage {
		parts = append(parts, mk(page+1, "next"), mk(lastPage, "last"))
	}
	if page > 1 {
		parts = append(parts, mk(1, "first"), mk(page-1, "prev"))
	}
	return page, perPage, strings.Join(parts, ", ")
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// ---------------------------------------------------------------------------
// REST routes
// ---------------------------------------------------------------------------

func (srv *server) handleREST(w http.ResponseWriter, r *http.Request, path string) {
	method := r.Method
	// API root.
	if path == "" || path == "/" {
		if method != http.MethodGet {
			srv.writeErr(w, r, http.StatusNotFound, "Not Found")
			return
		}
		srv.writeJSON(w, r, http.StatusOK, map[string]any{
			"current_user_url": "/api/v3/user",
			"rate_limit_url":   "/api/v3/rate_limit",
		})
		return
	}
	if path == "/meta" && method == http.MethodGet {
		srv.writeJSON(w, r, http.StatusOK, map[string]any{"installed_version": serverVersion})
		return
	}
	if path == "/user" && method == http.MethodGet {
		if !srv.requireAuth(w, r) {
			return
		}
		srv.writeJSON(w, r, http.StatusOK, map[string]any{
			"login": viewerLogin, "id": viewerID, "name": viewerName,
			"email": viewerEmail, "type": "User",
		})
		return
	}
	if path == "/user/repos" {
		switch method {
		case http.MethodGet:
			srv.restListUserRepos(w, r)
		case http.MethodPost:
			srv.restCreateRepo(w, r)
		default:
			srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		}
		return
	}
	if strings.HasPrefix(path, "/repos/") {
		srv.handleRepoRoutes(w, r, strings.TrimPrefix(path, "/repos/"))
		return
	}
	srv.writeErr(w, r, http.StatusNotFound, "Not Found")
}

func (srv *server) restListUserRepos(w http.ResponseWriter, r *http.Request) {
	if !srv.requireAuth(w, r) {
		return
	}
	srv.store.mu.RLock()
	var all []*Repo
	for _, repo := range srv.store.repos {
		if repo.Owner == viewerLogin {
			all = append(all, repo)
		}
	}
	srv.store.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedSeq < all[j].CreatedSeq })
	page, perPage, link := paginate(r, len(all))
	start := (page - 1) * perPage
	if start > len(all) {
		start = len(all)
	}
	end := start + perPage
	if end > len(all) {
		end = len(all)
	}
	items := make([]any, 0, end-start)
	for _, repo := range all[start:end] {
		items = append(items, repoJSON(repo))
	}
	if link != "" {
		w.Header().Set("Link", link)
	}
	srv.writeJSON(w, r, http.StatusOK, items)
}

func (srv *server) restCreateRepo(w http.ResponseWriter, r *http.Request) {
	if !srv.requireAuth(w, r) {
		return
	}
	var body struct {
		Name        string `json:"name"`
		Private     *bool  `json:"private"`
		Description string `json:"description"`
	}
	if !srv.readJSONBody(w, r, &body) {
		return
	}
	private := true
	if body.Private != nil {
		private = *body.Private
	}
	srv.store.mu.Lock()
	repo, err := srv.store.createRepo(viewerLogin, body.Name, body.Description, private)
	srv.store.mu.Unlock()
	if err != nil {
		if err == errRepoExists {
			srv.writeErr(w, r, 422, "Repository already exists")
			return
		}
		srv.writeErr(w, r, 422, err.Error())
		return
	}
	srv.writeJSON(w, r, http.StatusCreated, repoJSON(repo))
}

// handleRepoRoutes handles /repos/{owner}/{name}[/...].
func (srv *server) handleRepoRoutes(w http.ResponseWriter, r *http.Request, rest string) {
	segments := strings.Split(rest, "/")
	if len(segments) < 2 {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	owner, name := segments[0], segments[1]
	tail := segments[2:]
	method := r.Method

	// /repos/{owner}/{name} itself: GET / PATCH / DELETE.
	if len(tail) == 0 {
		switch method {
		case http.MethodGet:
			repo := srv.lookupRepo(w, r, owner, name)
			if repo == nil {
				return
			}
			srv.writeJSON(w, r, http.StatusOK, repoJSON(repo))
		case http.MethodPatch:
			srv.restPatchRepo(w, r, owner, name)
		case http.MethodDelete:
			srv.restDeleteRepo(w, r, owner, name)
		default:
			srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		}
		return
	}

	repo := srv.lookupRepo(w, r, owner, name)
	if repo == nil {
		return
	}
	switch tail[0] {
	case "branches":
		if method == http.MethodGet && len(tail) == 1 {
			srv.restListBranches(w, r, repo)
			return
		}
		if method == http.MethodGet && len(tail) == 2 {
			srv.restGetBranch(w, r, repo, tail[1])
			return
		}
	case "commits":
		if method == http.MethodGet && len(tail) == 1 {
			srv.restListCommits(w, r, repo)
			return
		}
	case "git":
		if len(tail) >= 2 && tail[1] == "refs" {
			if method == http.MethodPost && len(tail) == 2 {
				srv.restCreateRef(w, r, repo)
				return
			}
			if method == http.MethodGet && len(tail) >= 4 && tail[2] == "heads" {
				srv.restGetRef(w, r, repo, "refs/heads/"+strings.Join(tail[3:], "/"))
				return
			}
		}
	case "contents":
		contentsPath := strings.Join(tail[1:], "/")
		switch method {
		case http.MethodGet:
			srv.restGetContents(w, r, repo, contentsPath)
		case http.MethodPut:
			srv.restPutContents(w, r, repo, contentsPath)
		case http.MethodDelete:
			srv.restDeleteContents(w, r, repo, contentsPath)
		default:
			srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		}
		return
	case "labels":
		if method == http.MethodPost && len(tail) == 1 {
			srv.restCreateLabel(w, r, repo)
			return
		}
	case "issues":
		srv.handleIssueRoutes(w, r, repo, tail[1:])
		return
	case "pulls":
		srv.handlePullRoutes(w, r, repo, tail[1:])
		return
	}
	srv.writeErr(w, r, http.StatusNotFound, "Not Found")
}

func (srv *server) restPatchRepo(w http.ResponseWriter, r *http.Request, owner, name string) {
	if !srv.requireAuth(w, r) {
		return
	}
	var body struct {
		Description *string `json:"description"`
		Private     *bool   `json:"private"`
	}
	if !srv.readJSONBody(w, r, &body) {
		return
	}
	srv.store.mu.Lock()
	repo := srv.store.getRepo(owner, name)
	if repo == nil {
		srv.store.mu.Unlock()
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	if body.Description != nil {
		repo.Description = *body.Description
	}
	if body.Private != nil {
		repo.Private = *body.Private
	}
	srv.store.mu.Unlock()
	srv.writeJSON(w, r, http.StatusOK, repoJSON(repo))
}

func (srv *server) restDeleteRepo(w http.ResponseWriter, r *http.Request, owner, name string) {
	if !srv.requireAuth(w, r) {
		return
	}
	srv.store.mu.Lock()
	repo := srv.store.getRepo(owner, name)
	if repo == nil {
		srv.store.mu.Unlock()
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	delete(srv.store.repos, repoKey(owner, name))
	srv.store.mu.Unlock()
	srv.writeNoContent(w, r)
}

func (srv *server) restListBranches(w http.ResponseWriter, r *http.Request, repo *Repo) {
	srv.store.mu.RLock()
	names := sortedRefs(repo)
	srv.store.mu.RUnlock()
	items := make([]any, 0, len(names))
	for _, ref := range names {
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		srv.store.mu.RLock()
		sha := repo.Refs[ref]
		srv.store.mu.RUnlock()
		items = append(items, map[string]any{
			"name":   strings.TrimPrefix(ref, "refs/heads/"),
			"commit": map[string]any{"sha": sha},
		})
	}
	srv.writeJSON(w, r, http.StatusOK, items)
}

func (srv *server) restGetBranch(w http.ResponseWriter, r *http.Request, repo *Repo, branch string) {
	srv.store.mu.RLock()
	sha, ok := repo.Refs["refs/heads/"+branch]
	srv.store.mu.RUnlock()
	if !ok {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	srv.writeJSON(w, r, http.StatusOK, map[string]any{
		"name":   branch,
		"commit": map[string]any{"sha": sha},
	})
}

func (srv *server) restListCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	ref := r.URL.Query().Get("sha")
	srv.store.mu.RLock()
	tip, ok := srv.store.resolveRef(repo, ref)
	srv.store.mu.RUnlock()
	if !ok || tip == "" {
		srv.writeJSON(w, r, http.StatusOK, []any{})
		return
	}
	// Walk ancestors newest-first (creation order is the commit order here:
	// parents were created before their children).
	var commits []string
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		srv.store.mu.RLock()
		obj := srv.store.objects[cur]
		srv.store.mu.RUnlock()
		if obj == nil || obj.Type != "commit" {
			continue
		}
		commits = append(commits, cur)
		for _, p := range parseCommit(obj.Data).Parents {
			queue = append(queue, p)
		}
	}
	items := make([]any, 0, len(commits))
	for _, sha := range commits {
		srv.store.mu.RLock()
		obj := srv.store.objects[sha]
		srv.store.mu.RUnlock()
		msg := ""
		if obj != nil {
			parts := strings.SplitN(string(obj.Data), "\n\n", 2)
			if len(parts) == 2 {
				msg = parts[1]
			}
		}
		items = append(items, map[string]any{
			"sha": sha,
			"commit": map[string]any{
				"message": msg,
				"author":  map[string]any{"name": viewerName, "email": viewerEmail, "date": fixedTimestamp},
			},
			"author": map[string]any{"login": viewerLogin, "id": viewerID},
		})
	}
	srv.writeJSON(w, r, http.StatusOK, items)
}

func (srv *server) restCreateRef(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if !srv.requireAuth(w, r) {
		return
	}
	var body struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if !srv.readJSONBody(w, r, &body) {
		return
	}
	if body.Ref == "" || !isObjectID(body.SHA) {
		srv.writeErr(w, r, 422, "Invalid ref or sha")
		return
	}
	srv.store.mu.Lock()
	defer srv.store.mu.Unlock()
	if _, exists := repo.Refs[body.Ref]; exists {
		srv.writeErr(w, r, 422, "Reference already exists")
		return
	}
	if srv.store.objects[body.SHA] == nil {
		srv.writeErr(w, r, 422, "Object does not exist")
		return
	}
	repo.Refs[body.Ref] = body.SHA
	srv.writeJSON(w, r, http.StatusCreated, map[string]any{
		"ref":    body.Ref,
		"object": map[string]any{"sha": body.SHA, "type": "commit"},
	})
}

func (srv *server) restGetRef(w http.ResponseWriter, r *http.Request, repo *Repo, ref string) {
	srv.store.mu.RLock()
	sha, ok := repo.Refs[ref]
	srv.store.mu.RUnlock()
	if !ok {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	srv.writeJSON(w, r, http.StatusOK, map[string]any{
		"ref":    ref,
		"object": map[string]any{"sha": sha, "type": "commit"},
	})
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

func (srv *server) restPutContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	if !srv.requireAuth(w, r) {
		return
	}
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
	}
	if !srv.readJSONBody(w, r, &body) {
		return
	}
	if body.Message == "" || body.Content == "" {
		srv.writeErr(w, r, 422, "message and content are required")
		return
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
	if err != nil {
		srv.writeErr(w, r, 422, "content is not valid base64")
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	existed := false
	srv.store.mu.Lock()
	defer srv.store.mu.Unlock()
	branchRef := "refs/heads/" + branch
	tip, exists := repo.Refs[branchRef]
	if !exists {
		// New branch from the default branch tip (or an unborn repo).
		if branch != repo.DefaultBranch {
			if defaultTip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
				repo.Refs[branchRef] = defaultTip
				tip = defaultTip
				exists = true
			}
		}
	}
	if exists && tip != "" {
		if blob, ok := srv.store.fileAt(tip, path); ok {
			// Update: the blob sha must match.
			currentSHA := hashObject("blob", blob.Data)
			if body.SHA == "" {
				srv.writeErr(w, r, 422, "sha is required to update a file")
				return
			}
			if !strings.EqualFold(body.SHA, currentSHA) {
				srv.writeErr(w, r, http.StatusConflict, "sha does not match the current file")
				return
			}
			existed = true
		}
	}
	commitSHA := srv.store.commitFile(repo, branch, path, content, body.Message, false)
	blobSHA := hashObject("blob", content)
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	srv.writeJSON(w, r, status, map[string]any{
		"content": map[string]any{
			"name":     baseName(path),
			"path":     path,
			"sha":      blobSHA,
			"size":     len(content),
			"type":     "file",
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString(content),
		},
		"commit": map[string]any{"sha": commitSHA},
	})
}

func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func (srv *server) restGetContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	ref := r.URL.Query().Get("ref")
	srv.store.mu.RLock()
	tip, ok := srv.store.resolveRef(repo, ref)
	srv.store.mu.RUnlock()
	if !ok || tip == "" {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	if path == "" {
		srv.store.mu.RLock()
		entries, ok := srv.store.dirAt(tip, "")
		srv.store.mu.RUnlock()
		if !ok {
			srv.writeErr(w, r, http.StatusNotFound, "Not Found")
			return
		}
		srv.writeJSON(w, r, http.StatusOK, dirEntriesJSON(repo, "", entries))
		return
	}
	srv.store.mu.RLock()
	blob, isFile := srv.store.fileAt(tip, path)
	srv.store.mu.RUnlock()
	if isFile {
		srv.writeJSON(w, r, http.StatusOK, map[string]any{
			"name":     baseName(path),
			"path":     path,
			"sha":      hashObject("blob", blob.Data),
			"size":     len(blob.Data),
			"type":     "file",
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString(blob.Data),
		})
		return
	}
	srv.store.mu.RLock()
	entries, isDir := srv.store.dirAt(tip, path)
	srv.store.mu.RUnlock()
	if isDir {
		srv.writeJSON(w, r, http.StatusOK, dirEntriesJSON(repo, path, entries))
		return
	}
	srv.writeErr(w, r, http.StatusNotFound, "Not Found")
}

func dirEntriesJSON(repo *Repo, dir string, entries []TreeEntry) []any {
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		typ := "file"
		if e.Mode == "40000" {
			typ = "dir"
		}
		p := e.Name
		if dir != "" {
			p = dir + "/" + e.Name
		}
		out = append(out, map[string]any{
			"name": e.Name,
			"path": p,
			"sha":  e.OID,
			"type": typ,
			"size": 0,
		})
	}
	return out
}

func (srv *server) restDeleteContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	if !srv.requireAuth(w, r) {
		return
	}
	var body struct {
		Message string `json:"message"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if !srv.readJSONBody(w, r, &body) {
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	srv.store.mu.Lock()
	defer srv.store.mu.Unlock()
	tip, ok := repo.Refs["refs/heads/"+branch]
	if !ok || tip == "" {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	blob, isFile := srv.store.fileAt(tip, path)
	if !isFile {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	if body.SHA != "" && !strings.EqualFold(body.SHA, hashObject("blob", blob.Data)) {
		srv.writeErr(w, r, http.StatusConflict, "sha does not match the current file")
		return
	}
	msg := body.Message
	if msg == "" {
		msg = "delete " + path
	}
	commitSHA := srv.store.commitFile(repo, branch, path, nil, msg, true)
	srv.writeJSON(w, r, http.StatusOK, map[string]any{
		"commit": map[string]any{"sha": commitSHA},
	})
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

func (srv *server) restCreateLabel(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if !srv.requireAuth(w, r) {
		return
	}
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !srv.readJSONBody(w, r, &body) {
		return
	}
	if body.Name == "" {
		srv.writeErr(w, r, 422, "name is required")
		return
	}
	srv.store.mu.Lock()
	repo.Labels[body.Name] = &Label{Name: body.Name, Color: body.Color}
	srv.store.mu.Unlock()
	srv.writeJSON(w, r, http.StatusCreated, map[string]any{"id": 1, "name": body.Name, "color": body.Color})
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func (srv *server) handleIssueRoutes(w http.ResponseWriter, r *http.Request, repo *Repo, tail []string) {
	method := r.Method
	if len(tail) == 0 {
		switch method {
		case http.MethodGet:
			srv.restListIssues(w, r, repo)
		case http.MethodPost:
			srv.restCreateIssue(w, r, repo)
		default:
			srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		}
		return
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	if len(tail) == 1 {
		switch method {
		case http.MethodGet:
			srv.restGetIssue(w, r, repo, number)
		case http.MethodPatch:
			srv.restPatchIssue(w, r, repo, number)
		default:
			srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		}
		return
	}
	if len(tail) == 2 && tail[1] == "comments" {
		switch method {
		case http.MethodGet:
			srv.restListComments(w, r, repo, number)
		case http.MethodPost:
			srv.restCreateComment(w, r, repo, number)
		default:
			srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		}
		return
	}
	srv.writeErr(w, r, http.StatusNotFound, "Not Found")
}

func (srv *server) restListIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortField := q.Get("sort")
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	srv.store.mu.RLock()
	var list []*Issue
	for _, n := range repo.IssueOrder {
		issue := repo.Issues[n]
		if state != "all" && issue.State != state {
			continue
		}
		list = append(list, issue)
	}
	srv.store.mu.RUnlock()
	if sortField == "created" && direction == "asc" {
		// IssueOrder is already creation order (ascending).
	} else {
		// Default: newest first.
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	page, perPage, link := paginate(r, len(list))
	start := (page - 1) * perPage
	if start > len(list) {
		start = len(list)
	}
	end := start + perPage
	if end > len(list) {
		end = len(list)
	}
	items := make([]any, 0, end-start)
	for _, issue := range list[start:end] {
		items = append(items, issueJSON(repo, issue))
	}
	if link != "" {
		w.Header().Set("Link", link)
	}
	srv.writeJSON(w, r, http.StatusOK, items)
}

func (srv *server) restCreateIssue(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if !srv.requireAuth(w, r) {
		return
	}
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if !srv.readJSONBody(w, r, &body) {
		return
	}
	srv.store.mu.Lock()
	issue, err := srv.store.createIssue(repo, body.Title, body.Body, body.Labels)
	srv.store.mu.Unlock()
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown label") {
			srv.writeErr(w, r, 422, err.Error())
			return
		}
		srv.writeErr(w, r, 422, err.Error())
		return
	}
	srv.writeJSON(w, r, http.StatusCreated, issueJSON(repo, issue))
}

func (srv *server) restGetIssue(w http.ResponseWriter, r *http.Request, repo *Repo, number int) {
	srv.store.mu.RLock()
	issue, ok := repo.Issues[number]
	srv.store.mu.RUnlock()
	if !ok {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	srv.writeJSON(w, r, http.StatusOK, issueJSON(repo, issue))
}

func (srv *server) restPatchIssue(w http.ResponseWriter, r *http.Request, repo *Repo, number int) {
	if !srv.requireAuth(w, r) {
		return
	}
	var body struct {
		State *string `json:"state"`
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if !srv.readJSONBody(w, r, &body) {
		return
	}
	srv.store.mu.Lock()
	issue, ok := repo.Issues[number]
	if !ok {
		srv.store.mu.Unlock()
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	if body.State != nil {
		issue.State = *body.State
	}
	if body.Title != nil {
		issue.Title = *body.Title
	}
	if body.Body != nil {
		issue.Body = *body.Body
	}
	srv.store.mu.Unlock()
	srv.writeJSON(w, r, http.StatusOK, issueJSON(repo, issue))
}

func (srv *server) restListComments(w http.ResponseWriter, r *http.Request, repo *Repo, number int) {
	srv.store.mu.RLock()
	_, ok := repo.Issues[number]
	comments := repo.Comments[number]
	srv.store.mu.RUnlock()
	if !ok {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	items := make([]any, 0, len(comments))
	for _, c := range comments {
		items = append(items, map[string]any{
			"id": c.ID, "body": c.Body,
			"user":       map[string]any{"login": viewerLogin, "id": viewerID},
			"created_at": fixedTimestamp,
		})
	}
	srv.writeJSON(w, r, http.StatusOK, items)
}

func (srv *server) restCreateComment(w http.ResponseWriter, r *http.Request, repo *Repo, number int) {
	if !srv.requireAuth(w, r) {
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if !srv.readJSONBody(w, r, &body) {
		return
	}
	if body.Body == "" {
		srv.writeErr(w, r, 422, "body is required")
		return
	}
	srv.store.mu.Lock()
	_, ok := repo.Issues[number]
	if !ok {
		srv.store.mu.Unlock()
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	comment := &Comment{ID: srv.store.nextCommentID, Body: body.Body}
	srv.store.nextCommentID++
	repo.Comments[number] = append(repo.Comments[number], comment)
	srv.store.mu.Unlock()
	srv.writeJSON(w, r, http.StatusCreated, map[string]any{
		"id": comment.ID, "body": comment.Body,
		"user":       map[string]any{"login": viewerLogin, "id": viewerID},
		"created_at": fixedTimestamp,
	})
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

func (srv *server) handlePullRoutes(w http.ResponseWriter, r *http.Request, repo *Repo, tail []string) {
	method := r.Method
	if len(tail) == 0 {
		switch method {
		case http.MethodGet:
			srv.restListPulls(w, r, repo)
		case http.MethodPost:
			srv.restCreatePull(w, r, repo)
		default:
			srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		}
		return
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	if len(tail) == 1 && method == http.MethodGet {
		srv.restGetPull(w, r, repo, number)
		return
	}
	if len(tail) == 2 && tail[1] == "files" && method == http.MethodGet {
		srv.restListPullFiles(w, r, repo, number)
		return
	}
	if len(tail) == 2 && tail[1] == "merge" && method == http.MethodPut {
		srv.restMergePull(w, r, repo, number)
		return
	}
	srv.writeErr(w, r, http.StatusNotFound, "Not Found")
}

func (srv *server) restListPulls(w http.ResponseWriter, r *http.Request, repo *Repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	srv.store.mu.RLock()
	var list []*Pull
	for _, n := range repo.PROrder {
		pr := repo.PRs[n]
		if state != "all" && pr.State != state {
			continue
		}
		list = append(list, pr)
	}
	srv.store.mu.RUnlock()
	// Newest first.
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	page, perPage, link := paginate(r, len(list))
	start := (page - 1) * perPage
	if start > len(list) {
		start = len(list)
	}
	end := start + perPage
	if end > len(list) {
		end = len(list)
	}
	items := make([]any, 0, end-start)
	for _, pr := range list[start:end] {
		items = append(items, pullJSON(repo, pr))
	}
	if link != "" {
		w.Header().Set("Link", link)
	}
	srv.writeJSON(w, r, http.StatusOK, items)
}

func (srv *server) restCreatePull(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if !srv.requireAuth(w, r) {
		return
	}
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if !srv.readJSONBody(w, r, &body) {
		return
	}
	srv.store.mu.Lock()
	pr, err := srv.store.createPull(repo, body.Title, body.Body, body.Head, body.Base)
	srv.store.mu.Unlock()
	if err != nil {
		srv.writeErr(w, r, 422, err.Error())
		return
	}
	srv.writeJSON(w, r, http.StatusCreated, pullJSON(repo, pr))
}

func (srv *server) restGetPull(w http.ResponseWriter, r *http.Request, repo *Repo, number int) {
	srv.store.mu.RLock()
	pr, ok := repo.PRs[number]
	srv.store.mu.RUnlock()
	if !ok {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	srv.writeJSON(w, r, http.StatusOK, pullJSON(repo, pr))
}

func (srv *server) restListPullFiles(w http.ResponseWriter, r *http.Request, repo *Repo, number int) {
	srv.store.mu.RLock()
	pr, ok := repo.PRs[number]
	srv.store.mu.RUnlock()
	if !ok {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	srv.store.mu.RLock()
	baseTree, headTree := "", ""
	if c := srv.store.objects[pr.BaseSHA]; c != nil && c.Type == "commit" {
		baseTree = parseCommit(c.Data).Tree
	}
	if c := srv.store.objects[pr.HeadSHA]; c != nil && c.Type == "commit" {
		headTree = parseCommit(c.Data).Tree
	}
	changes := srv.store.diffTrees(baseTree, headTree)
	srv.store.mu.RUnlock()
	items := make([]any, 0, len(changes))
	for _, ch := range changes {
		add, del := 0, 0
		srv.store.mu.RLock()
		var oldData, newData []byte
		if ch.OldOID != "" {
			if o := srv.store.objects[ch.OldOID]; o != nil {
				oldData = o.Data
			}
		}
		if ch.NewOID != "" {
			if o := srv.store.objects[ch.NewOID]; o != nil {
				newData = o.Data
			}
		}
		srv.store.mu.RUnlock()
		add, del = lineDiff(oldData, newData)
		status := ch.Status
		if status == "modified" {
			status = "modified"
		}
		items = append(items, map[string]any{
			"filename":  ch.Path,
			"status":    status,
			"additions": add,
			"deletions": del,
			"changes":   add + del,
		})
	}
	// Ensure the JSON shape always carries the fields even for empty diffs.
	srv.writeJSON(w, r, http.StatusOK, items)
}

func (srv *server) restMergePull(w http.ResponseWriter, r *http.Request, repo *Repo, number int) {
	if !srv.requireAuth(w, r) {
		return
	}
	var body struct {
		MergeMethod string `json:"merge_method"`
	}
	if !srv.readJSONBody(w, r, &body) {
		return
	}
	srv.store.mu.Lock()
	pr, ok := repo.PRs[number]
	if !ok {
		srv.store.mu.Unlock()
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	mergeSHA, err := srv.store.mergePull(repo, pr)
	srv.store.mu.Unlock()
	if err != nil {
		if err == errMergeConflict {
			srv.writeErr(w, r, http.StatusMethodNotAllowed, "Merge conflict")
			return
		}
		srv.writeErr(w, r, http.StatusMethodNotAllowed, err.Error())
		return
	}
	srv.writeJSON(w, r, http.StatusOK, map[string]any{
		"merged":  true,
		"sha":     mergeSHA,
		"message": "Pull request successfully merged",
	})
}
