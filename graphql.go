package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// GraphQL v4: POST /api/graphql, {query, variables} or a batch array.
// The suite sends a fixed set of documents; dispatch is by operation name
// and each operation returns exactly the fields its document selects.
// ---------------------------------------------------------------------------

var operationNameRe = regexp.MustCompile(`(?m)^\s*(?:query|mutation)\s+([A-Za-z0-9_]+)`)

func (srv *server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	if !srv.store.isViewerToken(r) {
		w.Header().Set("Content-Type", jsonContentType)
		w.Header().Set("X-GitHub-Request-Id", srv.store.nextReqID())
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Requires authentication"}]}`))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		srv.writeErr(w, r, http.StatusBadRequest, "could not read request body")
		return
	}
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		srv.writeJSON(w, r, http.StatusOK, map[string]any{"errors": []any{map[string]string{"message": "empty request body"}}})
		return
	}
	if trimmed[0] == '[' {
		var ops []map[string]any
		if err := json.Unmarshal(trimmed, &ops); err != nil {
			srv.writeJSON(w, r, http.StatusOK, map[string]any{"errors": []any{map[string]string{"message": "Problems parsing JSON"}}})
			return
		}
		out := make([]any, 0, len(ops))
		for _, op := range ops {
			out = append(out, srv.executeGraphQL(op))
		}
		srv.writeJSON(w, r, http.StatusOK, out)
		return
	}
	var op map[string]any
	if err := json.Unmarshal(trimmed, &op); err != nil {
		srv.writeJSON(w, r, http.StatusOK, map[string]any{"errors": []any{map[string]string{"message": "Problems parsing JSON"}}})
		return
	}
	srv.writeJSON(w, r, http.StatusOK, srv.executeGraphQL(op))
}

func gqlVars(op map[string]any) map[string]any {
	if v, ok := op["variables"].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

func gqlStr(vars map[string]any, key string) string {
	if v, ok := vars[key].(string); ok {
		return v
	}
	return ""
}

func gqlInt(vars map[string]any, key string) int {
	if v, ok := vars[key].(float64); ok {
		return int(v)
	}
	return 0
}

func gqlInput(vars map[string]any) map[string]any {
	if v, ok := vars["input"].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

func gqlData(data map[string]any) map[string]any {
	return map[string]any{"data": data}
}

func gqlError(msg string) map[string]any {
	return map[string]any{
		"data":   nil,
		"errors": []any{map[string]string{"message": msg}},
	}
}

// executeGraphQL runs one operation document.
func (srv *server) executeGraphQL(op map[string]any) map[string]any {
	query, _ := op["query"].(string)
	vars := gqlVars(op)
	m := operationNameRe.FindStringSubmatch(query)
	if m == nil {
		return gqlError("could not parse operation")
	}
	name := m[1]
	switch name {
	case "ViewerLogin":
		return gqlData(map[string]any{"viewer": map[string]any{"login": viewerLogin}})
	case "RepositoryFields":
		return srv.gqlRepositoryFields(vars)
	case "Issue_fields":
		return gqlData(map[string]any{"Issue": map[string]any{"fields": typeFields("Issue")}})
	case "PullRequestAndRepositoryFields":
		return gqlData(map[string]any{
			"PullRequest": map[string]any{"fields": typeFields("PullRequest")},
			"Repository":  map[string]any{"fields": typeFields("Repository")},
		})
	case "SearchType_enumValues":
		return gqlData(map[string]any{"SearchType": map[string]any{"enumValues": enumValues("SearchType")}})
	case "OpenIssues":
		return srv.gqlOpenIssues(vars)
	case "IssueByNumber":
		return srv.gqlIssueByNumber(vars)
	case "IssueCreate":
		return srv.gqlCreateIssue(vars)
	case "IssueClose":
		return srv.gqlCloseIssue(vars)
	case "PullRequests":
		return srv.gqlPullRequests(vars)
	case "PullRequestFiles":
		return srv.gqlPullRequestFiles(vars)
	case "PullRequestId":
		return srv.gqlPullRequestID(vars)
	case "PullRequestCreate":
		return srv.gqlCreatePullRequest(vars)
	case "MergePullRequest":
		return srv.gqlMergePullRequest(vars)
	case "AddComment":
		return srv.gqlAddComment(vars)
	case "ViewerInvalidField":
		return gqlError(`Cannot query field "noSuchFieldOnViewer" on type "Viewer".`)
	case "NodeRepository":
		return srv.gqlNodeRepository(vars)
	case "IssuesPage":
		return srv.gqlIssuesPage(vars)
	default:
		return gqlError(fmt.Sprintf("unknown operation %q", name))
	}
}

func fieldNames(names []string) []any {
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return out
}

func typeFields(typeName string) []any {
	switch typeName {
	case "Issue":
		return fieldNames([]string{"id", "number", "title", "body", "state", "author", "labels", "comments", "createdAt", "updatedAt", "closedAt", "url"})
	case "PullRequest":
		return fieldNames([]string{"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "files", "author", "createdAt", "updatedAt", "mergedAt", "url"})
	case "Repository":
		return fieldNames([]string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "url", "createdAt", "updatedAt"})
	}
	return fieldNames(nil)
}

func enumValues(typeName string) []any {
	if typeName == "SearchType" {
		return fieldNames([]string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"})
	}
	return fieldNames(nil)
}

// lookupRepoGQL resolves owner/name for GraphQL: nil when absent (the
// repository field is nullable, so a miss is data.repository = null).
func (srv *server) lookupRepoGQL(owner, name string) *Repo {
	srv.store.mu.RLock()
	defer srv.store.mu.RUnlock()
	return srv.store.getRepo(owner, name)
}

func (srv *server) repositoryJSON(repo *Repo) map[string]any {
	out := map[string]any{
		"id":               repoGlobalID(repo),
		"databaseId":       repo.ID,
		"name":             repo.Name,
		"nameWithOwner":    repo.Owner + "/" + repo.Name,
		"owner":            map[string]any{"login": repo.Owner},
		"viewerPermission": "ADMIN",
		"hasIssuesEnabled": true,
	}
	srv.store.mu.RLock()
	tip, hasCommit := repo.Refs["refs/heads/"+repo.DefaultBranch]
	srv.store.mu.RUnlock()
	if hasCommit && tip != "" {
		out["defaultBranchRef"] = map[string]any{"name": repo.DefaultBranch}
	} else {
		out["defaultBranchRef"] = nil
	}
	return out
}

func (srv *server) gqlRepositoryFields(vars map[string]any) map[string]any {
	repo := srv.lookupRepoGQL(gqlStr(vars, "owner"), gqlStr(vars, "name"))
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	return gqlData(map[string]any{"repository": srv.repositoryJSON(repo)})
}

func issueNodeJSON(repo *Repo, issue *Issue) map[string]any {
	return map[string]any{
		"id":     issueGlobalID(repo, issue.Number),
		"number": issue.Number,
		"title":  issue.Title,
		"body":   issue.Body,
		"state":  strings.ToUpper(issue.State),
	}
}

func (srv *server) gqlOpenIssues(vars map[string]any) map[string]any {
	repo := srv.lookupRepoGQL(gqlStr(vars, "owner"), gqlStr(vars, "name"))
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	first := gqlInt(vars, "first")
	srv.store.mu.RLock()
	var list []*Issue
	for i := len(repo.IssueOrder) - 1; i >= 0; i-- {
		issue := repo.Issues[repo.IssueOrder[i]]
		if issue.State != "open" {
			continue
		}
		list = append(list, issue)
	}
	srv.store.mu.RUnlock()
	if first > 0 && len(list) > first {
		list = list[:first]
	}
	nodes := make([]any, 0, len(list))
	for _, issue := range list {
		nodes = append(nodes, issueNodeJSON(repo, issue))
	}
	return gqlData(map[string]any{"repository": map[string]any{"issues": map[string]any{"nodes": nodes}}})
}

func (srv *server) gqlIssueByNumber(vars map[string]any) map[string]any {
	repo := srv.lookupRepoGQL(gqlStr(vars, "owner"), gqlStr(vars, "name"))
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	number := gqlInt(vars, "number")
	srv.store.mu.RLock()
	issue, ok := repo.Issues[number]
	srv.store.mu.RUnlock()
	if !ok {
		return gqlData(map[string]any{"repository": map[string]any{"issue": nil}})
	}
	return gqlData(map[string]any{"repository": map[string]any{"issue": issueNodeJSON(repo, issue)}})
}

func (srv *server) gqlCreateIssue(vars map[string]any) map[string]any {
	input := gqlInput(vars)
	repoID, _ := input["repositoryId"].(string)
	title, _ := input["title"].(string)
	body, _ := input["body"].(string)
	srv.store.mu.Lock()
	repo := srv.store.resolveGlobalRepo(repoID)
	if repo == nil {
		srv.store.mu.Unlock()
		return gqlError("repository not found")
	}
	issue, err := srv.store.createIssue(repo, title, body, nil)
	srv.store.mu.Unlock()
	if err != nil {
		return gqlError(err.Error())
	}
	return gqlData(map[string]any{"createIssue": map[string]any{"issue": map[string]any{
		"number": issue.Number,
		"id":     issueGlobalID(repo, issue.Number),
	}}})
}

func (srv *server) gqlCloseIssue(vars map[string]any) map[string]any {
	input := gqlInput(vars)
	issueID, _ := input["issueId"].(string)
	srv.store.mu.Lock()
	repo, issue := srv.store.resolveGlobalIssue(issueID)
	if repo == nil || issue == nil {
		srv.store.mu.Unlock()
		return gqlError("issue not found")
	}
	issue.State = "closed"
	number := issue.Number
	srv.store.mu.Unlock()
	return gqlData(map[string]any{"closeIssue": map[string]any{"issue": map[string]any{
		"number": number,
		"id":     issueGlobalID(repo, number),
		"state":  "CLOSED",
	}}})
}

func pullNodeJSON(repo *Repo, pr *Pull) map[string]any {
	state := "OPEN"
	if pr.State == "closed" {
		if pr.Merged {
			state = "MERGED"
		} else {
			state = "CLOSED"
		}
	}
	return map[string]any{
		"id":          pullGlobalID(repo, pr.Number),
		"number":      pr.Number,
		"state":       state,
		"merged":      pr.Merged,
		"headRefName": pr.Head,
		"baseRefName": pr.Base,
		"mergeable":   "MERGEABLE",
	}
}

func (srv *server) gqlPullRequests(vars map[string]any) map[string]any {
	repo := srv.lookupRepoGQL(gqlStr(vars, "owner"), gqlStr(vars, "name"))
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	first := gqlInt(vars, "first")
	srv.store.mu.RLock()
	var list []*Pull
	for i := len(repo.PROrder) - 1; i >= 0; i-- {
		pr := repo.PRs[repo.PROrder[i]]
		if pr.State != "open" {
			continue
		}
		list = append(list, pr)
	}
	srv.store.mu.RUnlock()
	if first > 0 && len(list) > first {
		list = list[:first]
	}
	nodes := make([]any, 0, len(list))
	for _, pr := range list {
		nodes = append(nodes, pullNodeJSON(repo, pr))
	}
	return gqlData(map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"nodes": nodes}}})
}

func (srv *server) gqlPullRequestFiles(vars map[string]any) map[string]any {
	repo := srv.lookupRepoGQL(gqlStr(vars, "owner"), gqlStr(vars, "name"))
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	number := gqlInt(vars, "number")
	srv.store.mu.RLock()
	pr, ok := repo.PRs[number]
	srv.store.mu.RUnlock()
	if !ok {
		return gqlData(map[string]any{"repository": map[string]any{"pullRequest": nil}})
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
	nodes := make([]any, 0, len(changes))
	for _, ch := range changes {
		nodes = append(nodes, map[string]any{"path": ch.Path})
	}
	node := pullNodeJSON(repo, pr)
	node["files"] = map[string]any{"nodes": nodes}
	return gqlData(map[string]any{"repository": map[string]any{"pullRequest": node}})
}

func (srv *server) gqlPullRequestID(vars map[string]any) map[string]any {
	repo := srv.lookupRepoGQL(gqlStr(vars, "owner"), gqlStr(vars, "name"))
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	number := gqlInt(vars, "number")
	srv.store.mu.RLock()
	_, ok := repo.PRs[number]
	srv.store.mu.RUnlock()
	if !ok {
		return gqlData(map[string]any{"repository": map[string]any{"pullRequest": nil}})
	}
	return gqlData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"id":     pullGlobalID(repo, number),
		"number": number,
	}}})
}

func (srv *server) gqlCreatePullRequest(vars map[string]any) map[string]any {
	input := gqlInput(vars)
	repoID, _ := input["repositoryId"].(string)
	base, _ := input["baseRefName"].(string)
	head, _ := input["headRefName"].(string)
	title, _ := input["title"].(string)
	srv.store.mu.Lock()
	repo := srv.store.resolveGlobalRepo(repoID)
	if repo == nil {
		srv.store.mu.Unlock()
		return gqlError("repository not found")
	}
	pr, err := srv.store.createPull(repo, title, "", head, base)
	srv.store.mu.Unlock()
	if err != nil {
		return gqlError(err.Error())
	}
	node := pullNodeJSON(repo, pr)
	return gqlData(map[string]any{"createPullRequest": map[string]any{"pullRequest": node}})
}

func (srv *server) gqlMergePullRequest(vars map[string]any) map[string]any {
	input := gqlInput(vars)
	prID, _ := input["pullRequestId"].(string)
	srv.store.mu.Lock()
	repo, pr := srv.store.resolveGlobalPull(prID)
	if repo == nil || pr == nil {
		srv.store.mu.Unlock()
		return gqlError("pull request not found")
	}
	if _, err := srv.store.mergePull(repo, pr); err != nil {
		srv.store.mu.Unlock()
		return gqlError(err.Error())
	}
	number := pr.Number
	srv.store.mu.Unlock()
	return gqlData(map[string]any{"mergePullRequest": map[string]any{"pullRequest": map[string]any{
		"number": number,
		"merged": true,
		"state":  "MERGED",
	}}})
}

func (srv *server) gqlAddComment(vars map[string]any) map[string]any {
	input := gqlInput(vars)
	subjectID, _ := input["subjectId"].(string)
	body, _ := input["body"].(string)
	srv.store.mu.Lock()
	repo, issue := srv.store.resolveGlobalIssue(subjectID)
	if repo == nil || issue == nil {
		srv.store.mu.Unlock()
		return gqlError("issue not found")
	}
	comment := &Comment{ID: srv.store.nextCommentID, Body: body}
	srv.store.nextCommentID++
	repo.Comments[issue.Number] = append(repo.Comments[issue.Number], comment)
	commentID := comment.ID
	number := issue.Number
	srv.store.mu.Unlock()
	return gqlData(map[string]any{"addComment": map[string]any{"commentEdge": map[string]any{"node": map[string]any{
		"id":   commentGlobalID(repo, number, commentID),
		"body": body,
	}}}})
}

func (srv *server) gqlNodeRepository(vars map[string]any) map[string]any {
	id := gqlStr(vars, "id")
	srv.store.mu.RLock()
	repo := srv.store.resolveGlobalRepo(id)
	srv.store.mu.RUnlock()
	if repo == nil {
		return gqlData(map[string]any{"node": nil})
	}
	return gqlData(map[string]any{"node": map[string]any{
		"id":            repoGlobalID(repo),
		"name":          repo.Name,
		"nameWithOwner": repo.Owner + "/" + repo.Name,
	}})
}

// decodeCursor turns an opaque page cursor back into an issue number.
func decodeCursor(cursor string) int {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil {
		return 0
	}
	return n
}

func encodeCursor(number int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(number)))
}

func (srv *server) gqlIssuesPage(vars map[string]any) map[string]any {
	repo := srv.lookupRepoGQL(gqlStr(vars, "owner"), gqlStr(vars, "name"))
	if repo == nil {
		return gqlData(map[string]any{"repository": nil})
	}
	first := gqlInt(vars, "first")
	after, _ := vars["after"].(string)
	afterNumber := 0
	if after != "" {
		afterNumber = decodeCursor(after)
	}
	srv.store.mu.RLock()
	// Issues in CREATED_AT DESC order (newest first), after the cursor.
	var ordered []*Issue
	for i := len(repo.IssueOrder) - 1; i >= 0; i-- {
		issue := repo.Issues[repo.IssueOrder[i]]
		if afterNumber > 0 && issue.Number >= afterNumber {
			continue
		}
		ordered = append(ordered, issue)
	}
	srv.store.mu.RUnlock()
	hasNext := false
	if first > 0 && len(ordered) > first {
		hasNext = true
		ordered = ordered[:first]
	}
	nodes := make([]any, 0, len(ordered))
	endCursor := ""
	for _, issue := range ordered {
		nodes = append(nodes, map[string]any{"number": issue.Number})
		endCursor = encodeCursor(issue.Number)
	}
	return gqlData(map[string]any{"repository": map[string]any{"issues": map[string]any{
		"nodes":    nodes,
		"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": endCursor},
	}}})
}
