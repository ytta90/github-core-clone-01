package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// ---------------------------------------------------------------------------
// MCP: Streamable HTTP at POST /mcp, modern era (2026-07-28) via
// server/discover, with a legacy initialize fallback. Plain JSON answers
// (no SSE needed: the client accepts either).
// ---------------------------------------------------------------------------

const mcpRevision = "2026-07-28"

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpTools() []mcpToolDef {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	boolean := func() map[string]any { return map[string]any{"type": "boolean"} }
	schema := func(props map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	return []mcpToolDef{
		{
			Name:        "create_repo",
			Description: "Create a repository owned by the authenticated user.",
			InputSchema: schema(map[string]any{
				"name":        str(),
				"description": str(),
				"private":     boolean(),
			}, []string{"name"}),
		},
		{
			Name:        "get_repo",
			Description: "Get a repository by owner and name.",
			InputSchema: schema(map[string]any{
				"owner": str(),
				"repo":  str(),
			}, []string{"owner", "repo"}),
		},
		{
			Name:        "create_issue",
			Description: "Create an issue in a repository.",
			InputSchema: schema(map[string]any{
				"owner": str(),
				"repo":  str(),
				"title": str(),
				"body":  str(),
			}, []string{"owner", "repo", "title"}),
		},
		{
			Name:        "list_issues",
			Description: "List issues in a repository.",
			InputSchema: schema(map[string]any{
				"owner": str(),
				"repo":  str(),
				"state": str(),
			}, []string{"owner", "repo"}),
		},
		{
			Name:        "create_pull_request",
			Description: "Create a pull request from a head branch into a base branch.",
			InputSchema: schema(map[string]any{
				"owner": str(),
				"repo":  str(),
				"title": str(),
				"body":  str(),
				"head":  str(),
				"base":  str(),
			}, []string{"owner", "repo", "title", "head", "base"}),
		},
		{
			Name:        "merge_pull_request",
			Description: "Merge a pull request by number.",
			InputSchema: schema(map[string]any{
				"owner":       str(),
				"repo":        str(),
				"pull_number": map[string]any{"type": "integer"},
			}, []string{"owner", "repo", "pull_number"}),
		},
	}
}

func (srv *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		srv.writeErr(w, r, http.StatusBadRequest, "could not read request body")
		return
	}
	// A body that is not JSON is a parse error: HTTP 400, JSON-RPC -32700.
	trimmed := bytesTrimSpace(raw)
	var req map[string]any
	if err := json.Unmarshal(trimmed, &req); err != nil {
		w.Header().Set("Content-Type", jsonContentType)
		w.Header().Set("X-GitHub-Request-Id", srv.store.nextReqID())
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`))
		return
	}
	// Notifications (no id) get no JSON-RPC response.
	idRaw, hasID := req["id"]
	if !hasID || idRaw == nil {
		w.Header().Set("X-GitHub-Request-Id", srv.store.nextReqID())
		w.WriteHeader(http.StatusAccepted)
		return
	}
	idJSON, _ := json.Marshal(idRaw)
	method, _ := req["method"].(string)
	params, _ := req["params"].(map[string]any)

	// Authentication is decided per request, before any method runs.
	if !srv.store.isViewerToken(r) {
		srv.writeMCPError(w, r, idJSON, http.StatusUnauthorized, -32000, "unauthorized")
		return
	}
	switch method {
	case "server/discover":
		srv.writeMCPResult(w, r, idJSON, map[string]any{
			"supportedVersions": []string{mcpRevision},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": serverName, "version": serverVersion},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": serverName, "version": serverVersion},
			},
		})
	case "initialize":
		// Legacy handshake: echo a revision the client offered.
		negotiated := "2025-11-25"
		if pv, ok := params["protocolVersion"].(string); ok {
			for _, rev := range []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"} {
				if pv == rev {
					negotiated = pv
				}
			}
		}
		srv.writeMCPResult(w, r, idJSON, map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		})
	case "ping":
		srv.writeMCPResult(w, r, idJSON, map[string]any{})
	case "tools/list":
		srv.writeMCPResult(w, r, idJSON, map[string]any{"tools": mcpTools()})
	case "tools/call":
		srv.mcpCallTool(w, r, idJSON, params)
	default:
		srv.writeMCPError(w, r, idJSON, http.StatusOK, -32601, "Method not found: "+method)
	}
}

func (srv *server) writeMCPResult(w http.ResponseWriter, r *http.Request, idJSON []byte, result any) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(idJSON), "result": result})
	h := w.Header()
	h.Set("Content-Type", jsonContentType)
	h.Set("X-GitHub-Request-Id", srv.store.nextReqID())
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (srv *server) writeMCPError(w http.ResponseWriter, r *http.Request, idJSON []byte, status, code int, message string) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(idJSON),
		"error":   map[string]any{"code": code, "message": message},
	})
	h := w.Header()
	h.Set("Content-Type", jsonContentType)
	h.Set("X-GitHub-Request-Id", srv.store.nextReqID())
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// mcpToolResult writes a tools/call result envelope.
func (srv *server) mcpToolResult(w http.ResponseWriter, r *http.Request, idJSON []byte, structured any, isError bool, errText string) {
	var content []any
	if isError {
		content = []any{map[string]any{"type": "text", "text": errText}}
	} else {
		text, _ := json.Marshal(structured)
		content = []any{map[string]any{"type": "text", "text": string(text)}}
	}
	result := map[string]any{
		"content":           content,
		"structuredContent": structured,
		"isError":           isError,
	}
	srv.writeMCPResult(w, r, idJSON, result)
}

func mcpArgString(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func mcpArgInt(args map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return 0
}

func mcpArgBool(args map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		if v, ok := args[k].(bool); ok {
			return v, true
		}
	}
	return false, false
}

func (srv *server) mcpCallTool(w http.ResponseWriter, r *http.Request, idJSON []byte, params map[string]any) {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	missing := func(fields ...string) bool {
		for _, f := range fields {
			if _, ok := args[f]; !ok {
				return true
			}
		}
		return false
	}
	fail := func(msg string) {
		srv.mcpToolResult(w, r, idJSON, nil, true, msg)
	}
	switch name {
	case "create_repo":
		if missing("name") {
			fail("missing required arguments: name")
			return
		}
		repoName := mcpArgString(args, "name")
		desc := mcpArgString(args, "description")
		private, _ := mcpArgBool(args, "private")
		srv.store.mu.Lock()
		repo, err := srv.store.createRepo(viewerLogin, repoName, desc, private)
		srv.store.mu.Unlock()
		if err != nil {
			fail(err.Error())
			return
		}
		srv.mcpToolResult(w, r, idJSON, repoJSON(repo), false, "")
	case "get_repo":
		if missing("owner", "repo") {
			fail("missing required arguments: owner, repo")
			return
		}
		owner := mcpArgString(args, "owner", "owner_login")
		repoName := mcpArgString(args, "repo", "repository")
		srv.store.mu.RLock()
		repo := srv.store.getRepo(owner, repoName)
		srv.store.mu.RUnlock()
		if repo == nil {
			fail("repository not found")
			return
		}
		srv.mcpToolResult(w, r, idJSON, repoJSON(repo), false, "")
	case "create_issue":
		if missing("owner", "repo", "title") {
			fail("missing required arguments: owner, repo, title")
			return
		}
		owner := mcpArgString(args, "owner", "owner_login")
		repoName := mcpArgString(args, "repo", "repository")
		title := mcpArgString(args, "title")
		body := mcpArgString(args, "body")
		srv.store.mu.Lock()
		repo := srv.store.getRepo(owner, repoName)
		if repo == nil {
			srv.store.mu.Unlock()
			fail("repository not found")
			return
		}
		issue, err := srv.store.createIssue(repo, title, body, nil)
		srv.store.mu.Unlock()
		if err != nil {
			fail(err.Error())
			return
		}
		srv.mcpToolResult(w, r, idJSON, issueJSON(repo, issue), false, "")
	case "list_issues":
		if missing("owner", "repo") {
			fail("missing required arguments: owner, repo")
			return
		}
		owner := mcpArgString(args, "owner", "owner_login")
		repoName := mcpArgString(args, "repo", "repository")
		state := mcpArgString(args, "state")
		if state == "" {
			state = "open"
		}
		srv.store.mu.RLock()
		repo := srv.store.getRepo(owner, repoName)
		if repo == nil {
			srv.store.mu.RUnlock()
			fail("repository not found")
			return
		}
		var list []any
		for _, n := range repo.IssueOrder {
			issue := repo.Issues[n]
			if state != "all" && issue.State != state {
				continue
			}
			list = append(list, issueJSON(repo, issue))
		}
		srv.store.mu.RUnlock()
		if list == nil {
			list = []any{}
		}
		srv.mcpToolResult(w, r, idJSON, map[string]any{"issues": list}, false, "")
	case "create_pull_request":
		if missing("owner", "repo", "title", "head", "base") {
			fail("missing required arguments: owner, repo, title, head, base")
			return
		}
		owner := mcpArgString(args, "owner", "owner_login")
		repoName := mcpArgString(args, "repo", "repository")
		title := mcpArgString(args, "title")
		body := mcpArgString(args, "body")
		head := mcpArgString(args, "head")
		base := mcpArgString(args, "base")
		srv.store.mu.Lock()
		repo := srv.store.getRepo(owner, repoName)
		if repo == nil {
			srv.store.mu.Unlock()
			fail("repository not found")
			return
		}
		pr, err := srv.store.createPull(repo, title, body, head, base)
		srv.store.mu.Unlock()
		if err != nil {
			fail(err.Error())
			return
		}
		srv.mcpToolResult(w, r, idJSON, pullJSON(repo, pr), false, "")
	case "merge_pull_request":
		if missing("owner", "repo", "pull_number") {
			fail("missing required arguments: owner, repo, pull_number")
			return
		}
		owner := mcpArgString(args, "owner", "owner_login")
		repoName := mcpArgString(args, "repo", "repository")
		number := mcpArgInt(args, "pull_number", "number")
		srv.store.mu.Lock()
		repo := srv.store.getRepo(owner, repoName)
		if repo == nil {
			srv.store.mu.Unlock()
			fail("repository not found")
			return
		}
		pr, ok := repo.PRs[number]
		if !ok {
			srv.store.mu.Unlock()
			fail("pull request not found")
			return
		}
		if _, err := srv.store.mergePull(repo, pr); err != nil {
			srv.store.mu.Unlock()
			fail(err.Error())
			return
		}
		srv.store.mu.Unlock()
		srv.mcpToolResult(w, r, idJSON, map[string]any{"number": number, "merged": true, "state": "closed"}, false, "")
	default:
		srv.writeMCPError(w, r, idJSON, http.StatusOK, -32602, "Unknown tool: "+name)
	}
}
