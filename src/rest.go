package main

import (
	"sort"
	"strings"
)

// REST v3 under /api/v3/...

func (sv *server) rest(r *request) *response {
	segs := r.segs
	if len(segs) == 2 || (len(segs) == 3 && segs[2] == "") {
		// GET /api/v3/
		if r.method == "GET" {
			resp := jsonResp(200, []byte(`{}`))
			resp.header("X-OAuth-Scopes", "repo, user")
			return resp
		}
		return jsonErr(404, "Not Found")
	}
	if len(segs) == 3 && segs[2] == "meta" {
		if r.method == "GET" {
			return jsonResp(200, []byte(`{"installed_version":"1.0.0"}`))
		}
		return jsonErr(404, "Not Found")
	}
	if len(segs) == 3 && segs[2] == "user" {
		if r.method == "GET" {
			return jsonResp(200, sv.viewerJSON())
		}
		return jsonErr(404, "Not Found")
	}
	if len(segs) == 4 && segs[2] == "user" && segs[3] == "repos" {
		if r.method == "POST" {
			return sv.restCreateRepo(r)
		}
		if r.method == "GET" {
			return sv.restListUserRepos(r)
		}
		return jsonErr(404, "Not Found")
	}
	if len(segs) >= 5 && segs[2] == "repos" {
		owner, name := segs[3], segs[4]
		rest := segs[5:]
		if len(rest) == 0 {
			switch r.method {
			case "GET":
				return sv.restGetRepo(r, owner, name)
			case "PATCH":
				return sv.restPatchRepo(r, owner, name)
			case "DELETE":
				return sv.restDeleteRepo(r, owner, name)
			}
			return jsonErr(404, "Not Found")
		}
		switch rest[0] {
		case "git":
			if len(rest) == 2 && rest[1] == "refs" {
				if r.method == "POST" {
					return sv.restCreateRef(r, owner, name)
				}
			}
			if len(rest) >= 4 && rest[1] == "refs" && rest[2] == "heads" {
				if r.method == "GET" {
					branch := strings.Join(rest[3:], "/")
					return sv.restGetRef(r, owner, name, "refs/heads/"+branch)
				}
			}
		case "branches":
			if r.method == "GET" && len(rest) == 1 {
				return sv.restListBranches(r, owner, name)
			}
			if r.method == "GET" && len(rest) >= 2 {
				branch := strings.Join(rest[1:], "/")
				return sv.restGetBranch(r, owner, name, branch)
			}
		case "commits":
			if r.method == "GET" && len(rest) == 1 {
				return sv.restListCommits(r, owner, name)
			}
		case "contents":
			if len(rest) >= 2 {
				path := strings.Join(rest[1:], "/")
				switch r.method {
				case "GET":
					return sv.restGetContents(r, owner, name, path)
				case "PUT":
					return sv.restPutContents(r, owner, name, path)
				case "DELETE":
					return sv.restDeleteContents(r, owner, name, path)
				}
			}
		case "labels":
			if r.method == "POST" && len(rest) == 1 {
				return sv.restCreateLabel(r, owner, name)
			}
		case "issues":
			if len(rest) == 1 {
				if r.method == "GET" {
					return sv.restListIssues(r, owner, name)
				}
				if r.method == "POST" {
					return sv.restCreateIssue(r, owner, name)
				}
			}
			if len(rest) >= 2 {
				num := atoi(rest[1])
				if len(rest) == 2 {
					if r.method == "GET" {
						return sv.restGetIssue(r, owner, name, num)
					}
					if r.method == "PATCH" {
						return sv.restPatchIssue(r, owner, name, num)
					}
				}
				if len(rest) == 3 && rest[2] == "comments" {
					if r.method == "GET" {
						return sv.restListComments(r, owner, name, num)
					}
					if r.method == "POST" {
						return sv.restCreateComment(r, owner, name, num)
					}
				}
			}
		case "pulls":
			if len(rest) == 1 {
				if r.method == "GET" {
					return sv.restListPulls(r, owner, name)
				}
				if r.method == "POST" {
					return sv.restCreatePull(r, owner, name)
				}
			}
			if len(rest) >= 2 {
				num := atoi(rest[1])
				if len(rest) == 2 {
					if r.method == "GET" {
						return sv.restGetPull(r, owner, name, num)
					}
				}
				if len(rest) == 3 && rest[2] == "files" && r.method == "GET" {
					return sv.restPullFiles(r, owner, name, num)
				}
				if len(rest) == 3 && rest[2] == "merge" && r.method == "PUT" {
					return sv.restMergePull(r, owner, name, num)
				}
			}
		}
	}
	return jsonErr(404, "Not Found")
}

func (sv *server) viewerJSON() []byte {
	var j jb
	j.raw(`{"login":`)
	j.str(sv.st.login)
	j.raw(`,"id":1000,"name":`)
	j.str(sv.st.name)
	j.raw(`,"email":`)
	j.str(sv.st.email)
	j.raw(`,"type":"User"}`)
	return j.b
}

// parseBody decodes a JSON object body; returns nil + writes the error response on failure.
func parseBody(r *request) (map[string]any, *response) {
	if len(r.body) == 0 {
		return map[string]any{}, nil
	}
	v := parseJSON(r.body)
	m := jobj(v)
	if m == nil {
		return nil, jsonErr(400, "Problems parsing JSON")
	}
	return m, nil
}

func (sv *server) restCreateRepo(r *request) *response {
	body, errResp := parseBody(r)
	if errResp != nil {
		return errResp
	}
	name := jstr(body["name"])
	if name == "" {
		return jsonErr(422, "name is required")
	}
	desc := jstr(body["description"])
	priv := jbool(body["private"], false)
	repo, err := sv.st.createRepo(sv.st.login, name, desc, priv)
	if err != nil {
		return jsonErr(422, "Repository already exists")
	}
	return jsonResp(201, sv.st.repoJSON(repo))
}

func (sv *server) restListUserRepos(r *request) *response {
	perPage, page := pageParams(r)
	repos := sv.st.userRepos(sv.st.login)
	total := len(repos)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var j jb
	j.raw(`[`)
	for i := start; i < end; i++ {
		if i > start {
			j.raw(`,`)
		}
		j.bytes(sv.st.repoJSON(repos[i]))
	}
	j.raw(`]`)
	resp := jsonResp(200, j.b)
	sv.addLink(r, resp, "/api/v3/user/repos", perPage, page, total)
	return resp
}

func pageParams(r *request) (perPage, page int) {
	perPage = atoi(r.queryParam("per_page"))
	if perPage <= 0 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}
	page = atoi(r.queryParam("page"))
	if page <= 0 {
		page = 1
	}
	return
}

// addLink adds a Link header for pagination (next/prev) when applicable.
func (sv *server) addLink(r *request, resp *response, base string, perPage, page, total int) {
	host := r.hdr.host
	if host == "" {
		host = "127.0.0.1"
	}
	var parts []string
	if page*perPage < total {
		parts = append(parts, "<http://"+host+base+"?per_page="+itoa(perPage)+"&page="+itoa(page+1)+`>; rel="next"`)
	}
	if page > 1 {
		parts = append(parts, "<http://"+host+base+"?per_page="+itoa(perPage)+"&page="+itoa(page-1)+`>; rel="prev"`)
	}
	if len(parts) > 0 {
		resp.header("Link", strings.Join(parts, ", "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (sv *server) restGetRepo(r *request, owner, name string) *response {
	repo, errResp := sv.repoOr404(owner, name)
	if errResp != nil {
		return errResp
	}
	return jsonResp(200, sv.st.repoJSON(repo))
}

func (sv *server) restPatchRepo(r *request, owner, name string) *response {
	body, errResp := parseBody(r)
	if errResp != nil {
		return errResp
	}
	sv.st.mu.Lock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	if d, ok := body["description"]; ok {
		repo.description = jstr(d)
	}
	repo.jsonCache = nil
	json := sv.st.repoJSON(repo)
	sv.st.mu.Unlock()
	return jsonResp(200, json)
}

func (sv *server) restDeleteRepo(r *request, owner, name string) *response {
	if !sv.st.deleteRepo(owner, name) {
		return jsonErr(404, "Not Found")
	}
	return &response{status: 204, ctype: "application/json", body: []byte{}}
}

func (sv *server) restCreateRef(r *request, owner, name string) *response {
	body, errResp := parseBody(r)
	if errResp != nil {
		return errResp
	}
	ref := jstr(body["ref"])
	sha := jstr(body["sha"])
	if ref == "" || sha == "" {
		return jsonErr(422, "ref and sha are required")
	}
	sv.st.mu.Lock()
	defer sv.st.mu.Unlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	if _, ok := repo.refs[ref]; ok {
		return jsonErr(422, "Reference already exists")
	}
	if _, ok := repo.objects[sha]; !ok {
		return jsonErr(422, "Invalid SHA")
	}
	repo.refs[ref] = sha
	var j jb
	j.raw(`{"ref":`)
	j.str(ref)
	j.raw(`,"object":{"sha":`)
	j.str(sha)
	j.raw(`,"type":"commit"}}`)
	return jsonResp(201, j.b)
}

func (sv *server) restGetRef(r *request, owner, name, ref string) *response {
	sv.st.mu.RLock()
	defer sv.st.mu.RUnlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	oid, ok := repo.refs[ref]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	var j jb
	j.raw(`{"ref":`)
	j.str(ref)
	j.raw(`,"object":{"sha":`)
	j.str(oid)
	j.raw(`,"type":"commit"}}`)
	return jsonResp(200, j.b)
}

func (sv *server) restListBranches(r *request, owner, name string) *response {
	sv.st.mu.RLock()
	defer sv.st.mu.RUnlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	var names []string
	for ref := range repo.refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, ref[len("refs/heads/"):])
		}
	}
	sort.Strings(names)
	var j jb
	j.raw(`[`)
	for i, b := range names {
		if i > 0 {
			j.raw(`,`)
		}
		j.raw(`{"name":`)
		j.str(b)
		j.raw(`,"commit":{"sha":`)
		j.str(repo.refs["refs/heads/"+b])
		j.raw(`}}`)
	}
	j.raw(`]`)
	return jsonResp(200, j.b)
}

func (sv *server) restGetBranch(r *request, owner, name, branch string) *response {
	sv.st.mu.RLock()
	defer sv.st.mu.RUnlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	oid, ok := repo.refs["refs/heads/"+branch]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	var j jb
	j.raw(`{"name":`)
	j.str(branch)
	j.raw(`,"commit":{"sha":`)
	j.str(oid)
	j.raw(`}}`)
	return jsonResp(200, j.b)
}

func (sv *server) restListCommits(r *request, owner, name string) *response {
	sv.st.mu.RLock()
	defer sv.st.mu.RUnlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	branch := r.queryParam("sha")
	if branch == "" {
		branch = repo.defaultBranch
	}
	tip := sv.st.refTarget(repo, "refs/heads/"+branch)
	if tip == "" {
		return jsonResp(200, []byte(`[]`))
	}
	history := sv.st.commitHistory(repo, tip)
	var j jb
	j.raw(`[`)
	for i, oid := range history {
		if i > 0 {
			j.raw(`,`)
		}
		obj := repo.objects[oid]
		cv := parseCommit(obj.data)
		j.raw(`{"sha":`)
		j.str(oid)
		j.raw(`,"commit":{"message":`)
		j.str(cv.message)
		j.raw(`,"author":{"name":`)
		j.str(personName(cv.author))
		j.raw(`,"email":`)
		j.str(personEmail(cv.author))
		j.raw(`},"committer":{"name":`)
		j.str(personName(cv.commit))
		j.raw(`,"email":`)
		j.str(personEmail(cv.commit))
		j.raw(`}},"author":{"login":`)
		j.str(sv.st.login)
		j.raw(`},"committer":{"login":`)
		j.str(sv.st.login)
		j.raw(`}}`)
	}
	j.raw(`]`)
	return jsonResp(200, j.b)
}

// personName extracts the name from "Name <email> ts +0000".
func personName(s string) string {
	if i := strings.IndexByte(s, '<'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func personEmail(s string) string {
	a := strings.IndexByte(s, '<')
	b := strings.IndexByte(s, '>')
	if a >= 0 && b > a {
		return s[a+1 : b]
	}
	return ""
}

func (sv *server) restGetContents(r *request, owner, name, path string) *response {
	sv.st.mu.RLock()
	defer sv.st.mu.RUnlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	ref := r.queryParam("ref")
	if ref == "" {
		ref = repo.defaultBranch
	}
	tip := sv.st.refTarget(repo, "refs/heads/"+ref)
	if tip == "" {
		return jsonErr(404, "Not Found")
	}
	treeOID := parseCommit(repo.objects[tip].data).tree
	flat := sv.st.flattenTree(repo, treeOID)
	// file?
	if fe, ok := flat[path]; ok {
		blob := repo.objects[fe.oid]
		var j jb
		j.raw(`{"type":"file","encoding":"base64","content":`)
		j.str(encodeB64(blob.data))
		j.raw(`,"sha":`)
		j.str(fe.oid)
		j.raw(`,"size":`)
		j.num(len(blob.data))
		j.raw(`,"name":`)
		j.str(baseName(path))
		j.raw(`,"path":`)
		j.str(path)
		j.raw(`}`)
		return jsonResp(200, j.b)
	}
	// directory?
	prefix := path + "/"
	var entries []struct {
		name string
		path string
		typ  string
		sha  string
	}
	seenDirs := map[string]string{}
	for p, fe := range flat {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rel := p[len(prefix):]
		if idx := strings.IndexByte(rel, '/'); idx >= 0 {
			dir := rel[:idx]
			if _, ok := seenDirs[dir]; !ok {
				// find the tree oid for this subdirectory
				seenDirs[dir] = sv.dirTreeOID(repo, treeOID, path+"/"+dir)
				entries = append(entries, struct {
					name string
					path string
					typ  string
					sha  string
				}{dir, path + "/" + dir, "dir", seenDirs[dir]})
			}
		} else {
			entries = append(entries, struct {
				name string
				path string
				typ  string
				sha  string
			}{rel, p, "file", fe.oid})
		}
	}
	if len(entries) == 0 {
		// root listing when path == "" handled by caller path; empty dir or missing
		if path != "" {
			return jsonErr(404, "Not Found")
		}
	}
	// root listing: path == "" (only reachable via /contents/ with trailing slash)
	if path == "" {
		for p, fe := range flat {
			if idx := strings.IndexByte(p, '/'); idx < 0 {
				entries = append(entries, struct {
					name string
					path string
					typ  string
					sha  string
				}{p, p, "file", fe.oid})
			}
		}
		for dir := range dirNames(flat) {
			entries = append(entries, struct {
				name string
				path string
				typ  string
				sha  string
			}{dir, dir, "dir", sv.dirTreeOID(repo, treeOID, dir)})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	var j jb
	j.raw(`[`)
	for i, e := range entries {
		if i > 0 {
			j.raw(`,`)
		}
		j.raw(`{"type":`)
		j.str(e.typ)
		j.raw(`,"name":`)
		j.str(e.name)
		j.raw(`,"path":`)
		j.str(e.path)
		j.raw(`,"sha":`)
		j.str(e.sha)
		j.raw(`}`)
	}
	j.raw(`]`)
	return jsonResp(200, j.b)
}

func dirNames(flat map[string]fileEntry) map[string]bool {
	out := map[string]bool{}
	for p := range flat {
		if idx := strings.IndexByte(p, '/'); idx >= 0 {
			out[p[:idx]] = true
		}
	}
	return out
}

// dirTreeOID finds the tree oid for a directory path under a root tree.
func (sv *server) dirTreeOID(repo *repo, rootTree, dirPath string) string {
	parts := strings.Split(dirPath, "/")
	cur := rootTree
	for _, part := range parts {
		obj := repo.objects[cur]
		if obj == nil || obj.typ != "tree" {
			return ""
		}
		found := false
		for _, e := range parseTree(obj.data) {
			if e.name == part && e.mode == "40000" {
				cur = e.oid
				found = true
				break
			}
		}
		if !found {
			return ""
		}
	}
	return cur
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (sv *server) restPutContents(r *request, owner, name, path string) *response {
	body, errResp := parseBody(r)
	if errResp != nil {
		return errResp
	}
	message := jstr(body["message"])
	contentB64 := jstr(body["content"])
	branch := jstr(body["branch"])
	sha := jstr(body["sha"])
	if message == "" || contentB64 == "" {
		return jsonErr(422, "message and content are required")
	}
	content, err := decodeB64(contentB64)
	if err != nil {
		return jsonErr(400, "Invalid base64 content")
	}
	sv.st.mu.Lock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	// create vs update?
	branchName := branch
	if branchName == "" {
		branchName = repo.defaultBranch
	}
	tip := sv.st.refTarget(repo, "refs/heads/"+branchName)
	existed := false
	if tip != "" {
		flat := sv.st.flattenTree(repo, parseCommit(repo.objects[tip].data).tree)
		_, existed = flat[path]
	}
	commitOID, err := sv.st.putContentsLocked(repo, path, message, content, branch, sha)
	if err == errStaleSHA {
		sv.st.mu.Unlock()
		return jsonErr(409, "sha does not match")
	}
	if err != nil {
		sv.st.mu.Unlock()
		return jsonErr(422, err.Error())
	}
	blobOID := hashObject("blob", content)
	status := 201
	if existed {
		status = 200
	}
	sv.st.mu.Unlock()
	var j jb
	j.raw(`{"content":{"name":`)
	j.str(baseName(path))
	j.raw(`,"path":`)
	j.str(path)
	j.raw(`,"sha":`)
	j.str(blobOID)
	j.raw(`,"size":`)
	j.num(len(content))
	j.raw(`,"encoding":"base64"},"commit":{"sha":`)
	j.str(commitOID)
	j.raw(`}}`)
	return jsonResp(status, j.b)
}

func (sv *server) restDeleteContents(r *request, owner, name, path string) *response {
	body, errResp := parseBody(r)
	if errResp != nil {
		return errResp
	}
	message := jstr(body["message"])
	sha := jstr(body["sha"])
	branch := jstr(body["branch"])
	sv.st.mu.Lock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	commitOID, err := sv.st.deleteContentsLocked(repo, path, message, sha, branch)
	if err == errNotFound {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	if err == errStaleSHA {
		sv.st.mu.Unlock()
		return jsonErr(409, "sha does not match")
	}
	sv.st.mu.Unlock()
	var j jb
	j.raw(`{"commit":{"sha":`)
	j.str(commitOID)
	j.raw(`}}`)
	return jsonResp(200, j.b)
}

func (sv *server) restCreateLabel(r *request, owner, name string) *response {
	body, errResp := parseBody(r)
	if errResp != nil {
		return errResp
	}
	labelName := jstr(body["name"])
	color := jstr(body["color"])
	if labelName == "" {
		return jsonErr(422, "name is required")
	}
	sv.st.mu.Lock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	if l, ok := repo.labels[labelName]; ok {
		color = l.color
	} else {
		repo.labels[labelName] = &label{name: labelName, color: color}
	}
	sv.st.mu.Unlock()
	var j jb
	j.raw(`{"name":`)
	j.str(labelName)
	j.raw(`,"color":`)
	j.str(color)
	j.raw(`}`)
	return jsonResp(201, j.b)
}

func (sv *server) restCreateIssue(r *request, owner, name string) *response {
	body, errResp := parseBody(r)
	if errResp != nil {
		return errResp
	}
	title := jstr(body["title"])
	bodyText := jstr(body["body"])
	var labelNames []string
	for _, v := range jarr(body["labels"]) {
		if s, ok := v.(string); ok {
			labelNames = append(labelNames, s)
		}
	}
	sv.st.mu.Lock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	is, err := sv.st.createIssueLocked(repo, title, bodyText, labelNames)
	if err == errBadLabel {
		sv.st.mu.Unlock()
		return jsonErr(422, "Unknown label")
	}
	json := sv.st.issueJSON(is)
	sv.st.mu.Unlock()
	return jsonResp(201, json)
}

func (sv *server) restListIssues(r *request, owner, name string) *response {
	state := r.queryParam("state")
	if state == "" {
		state = "open"
	}
	sortDir := r.queryParam("sort")
	if sortDir == "" {
		sortDir = "created"
	}
	direction := r.queryParam("direction")
	if direction == "" {
		direction = "desc"
	}
	perPage, page := pageParams(r)
	sv.st.mu.RLock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		sv.st.mu.RUnlock()
		return jsonErr(404, "Not Found")
	}
	var list []*issue
	for _, is := range repo.issues {
		if state == "all" || is.state == state {
			list = append(list, is)
		}
	}
	sv.st.mu.RUnlock()
	_ = sortDir
	sort.Slice(list, func(i, j int) bool {
		if direction == "asc" {
			return list[i].seq < list[j].seq
		}
		return list[i].seq > list[j].seq
	})
	total := len(list)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var j jb
	j.raw(`[`)
	for i := start; i < end; i++ {
		if i > start {
			j.raw(`,`)
		}
		j.bytes(sv.st.issueJSON(list[i]))
	}
	j.raw(`]`)
	resp := jsonResp(200, j.b)
	sv.addLink(r, resp, "/api/v3/repos/"+owner+"/"+name+"/issues", perPage, page, total)
	return resp
}

func (sv *server) restGetIssue(r *request, owner, name string, num int) *response {
	sv.st.mu.RLock()
	defer sv.st.mu.RUnlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	is, ok := repo.issues[num]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	return jsonResp(200, sv.st.issueJSON(is))
}

func (sv *server) restPatchIssue(r *request, owner, name string, num int) *response {
	body, errResp := parseBody(r)
	if errResp != nil {
		return errResp
	}
	sv.st.mu.Lock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	is, ok := repo.issues[num]
	if !ok {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	if s, ok := body["state"]; ok {
		st := jstr(s)
		if st == "open" || st == "closed" {
			is.state = st
		}
	}
	if t, ok := body["title"]; ok {
		is.title = jstr(t)
	}
	if b, ok := body["body"]; ok {
		is.body = jstr(b)
	}
	is.jsonCache = nil
	json := sv.st.issueJSON(is)
	sv.st.mu.Unlock()
	return jsonResp(200, json)
}

func (sv *server) restCreateComment(r *request, owner, name string, num int) *response {
	body, errResp := parseBody(r)
	if errResp != nil {
		return errResp
	}
	bodyText := jstr(body["body"])
	sv.st.mu.Lock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	is, ok := repo.issues[num]
	if !ok {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	c := &comment{id: repo.nextCommentID, body: bodyText}
	repo.nextCommentID++
	is.comments = append(is.comments, c)
	sv.st.mu.Unlock()
	var j jb
	j.raw(`{"id":`)
	j.num(c.id)
	j.raw(`,"body":`)
	j.str(bodyText)
	j.raw(`,"user":{"login":`)
	j.str(sv.st.login)
	j.raw(`}}`)
	return jsonResp(201, j.b)
}

func (sv *server) restListComments(r *request, owner, name string, num int) *response {
	sv.st.mu.RLock()
	defer sv.st.mu.RUnlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	is, ok := repo.issues[num]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	var j jb
	j.raw(`[`)
	for i, c := range is.comments {
		if i > 0 {
			j.raw(`,`)
		}
		j.raw(`{"id":`)
		j.num(c.id)
		j.raw(`,"body":`)
		j.str(c.body)
		j.raw(`,"user":{"login":`)
		j.str(sv.st.login)
		j.raw(`}}`)
	}
	j.raw(`]`)
	return jsonResp(200, j.b)
}

func (sv *server) restCreatePull(r *request, owner, name string) *response {
	body, errResp := parseBody(r)
	if errResp != nil {
		return errResp
	}
	head := jstr(body["head"])
	base := jstr(body["base"])
	title := jstr(body["title"])
	bodyText := jstr(body["body"])
	sv.st.mu.Lock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	p, err := sv.st.createPRLocked(repo, head, base, title, bodyText)
	if err == errExists {
		sv.st.mu.Unlock()
		return jsonErr(422, "A pull request already exists for these branches")
	}
	json := sv.st.prJSON(p)
	sv.st.mu.Unlock()
	return jsonResp(201, json)
}

func (sv *server) restListPulls(r *request, owner, name string) *response {
	state := r.queryParam("state")
	if state == "" {
		state = "open"
	}
	perPage, page := pageParams(r)
	sv.st.mu.RLock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		sv.st.mu.RUnlock()
		return jsonErr(404, "Not Found")
	}
	var list []*pr
	for _, p := range repo.prs {
		if state == "all" || p.state == state {
			list = append(list, p)
		}
	}
	sv.st.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].seq > list[j].seq })
	total := len(list)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var j jb
	j.raw(`[`)
	for i := start; i < end; i++ {
		if i > start {
			j.raw(`,`)
		}
		j.bytes(sv.st.prJSON(list[i]))
	}
	j.raw(`]`)
	resp := jsonResp(200, j.b)
	sv.addLink(r, resp, "/api/v3/repos/"+owner+"/"+name+"/pulls", perPage, page, total)
	return resp
}

func (sv *server) restGetPull(r *request, owner, name string, num int) *response {
	sv.st.mu.RLock()
	defer sv.st.mu.RUnlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	p, ok := repo.prs[num]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	return jsonResp(200, sv.st.prJSON(p))
}

func (sv *server) restPullFiles(r *request, owner, name string, num int) *response {
	sv.st.mu.RLock()
	defer sv.st.mu.RUnlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	p, ok := repo.prs[num]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	files := sv.st.prFiles(repo, p)
	var j jb
	j.raw(`[`)
	for i, f := range files {
		if i > 0 {
			j.raw(`,`)
		}
		j.raw(`{"filename":`)
		j.str(f["filename"].(string))
		j.raw(`,"status":`)
		j.str(f["status"].(string))
		j.raw(`,"additions":`)
		j.num(f["additions"].(int))
		j.raw(`,"deletions":`)
		j.num(f["deletions"].(int))
		j.raw(`,"changes":`)
		j.num(f["changes"].(int))
		j.raw(`}`)
	}
	j.raw(`]`)
	return jsonResp(200, j.b)
}

func (sv *server) restMergePull(r *request, owner, name string, num int) *response {
	sv.st.mu.Lock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	p, ok := repo.prs[num]
	if !ok {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	if p.merged {
		sv.st.mu.Unlock()
		var j jb
		j.raw(`{"merged":true,"sha":`)
		j.str(p.mergeCommitSHA)
		j.raw(`,"message":"`)
		j.raw(`Pull request #`)
		j.num(p.number)
		j.raw(` is already merged"}`)
		return jsonResp(200, j.b)
	}
	err := sv.st.mergePRLocked(repo, p)
	if err == errConflict {
		sv.st.mu.Unlock()
		return jsonErr(405, "Merge conflict")
	}
	if err != nil {
		sv.st.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	sha := p.mergeCommitSHA
	sv.st.mu.Unlock()
	var j jb
	j.raw(`{"merged":true,"sha":`)
	j.str(sha)
	j.raw(`,"message":"Pull request #`)
	j.num(p.number)
	j.raw(` successfully merged"}`)
	return jsonResp(200, j.b)
}
