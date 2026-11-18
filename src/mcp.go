package main

// MCP over Streamable HTTP (POST /mcp), JSON-RPC 2.0, era 2026-07-28.

type mcpTool struct {
	name        string
	description string
	schema      string // inputSchema JSON
	required    []string
}

var mcpTools = []mcpTool{
	{
		name:        "create_repo",
		description: "Create a new repository owned by the authenticated user.",
		schema:      `{"type":"object","properties":{"name":{"type":"string"},"description":{"type":"string"},"private":{"type":"boolean"}},"required":["name"]}`,
		required:    []string{"name"},
	},
	{
		name:        "get_repo",
		description: "Get a repository by owner and name.",
		schema:      `{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"}},"required":["owner","repo"]}`,
		required:    []string{"owner", "repo"},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		schema:      `{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},"labels":{"type":"array","items":{"type":"string"}}},"required":["owner","repo","title"]}`,
		required:    []string{"owner", "repo", "title"},
	},
	{
		name:        "list_issues",
		description: "List issues in a repository.",
		schema:      `{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"state":{"type":"string"}},"required":["owner","repo"]}`,
		required:    []string{"owner", "repo"},
	},
	{
		name:        "create_pull_request",
		description: "Create a pull request in a repository.",
		schema:      `{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},"head":{"type":"string"},"base":{"type":"string"}},"required":["owner","repo","title","head","base"]}`,
		required:    []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name:        "merge_pull_request",
		description: "Merge a pull request in a repository.",
		schema:      `{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"pull_number":{"type":"number"}},"required":["owner","repo","pull_number"]}`,
		required:    []string{"owner", "repo", "pull_number"},
	},
}

// precomputed tools/list result (byte-identical across calls).
var mcpToolsListJSON []byte

func init() {
	var j jb
	j.raw(`{"tools":[`)
	for i, t := range mcpTools {
		if i > 0 {
			j.raw(`,`)
		}
		j.raw(`{"name":`)
		j.str(t.name)
		j.raw(`,"description":`)
		j.str(t.description)
		j.raw(`,"inputSchema":`)
		j.raw(t.schema)
		j.raw(`}`)
	}
	j.raw(`]}`)
	mcpToolsListJSON = j.b
}

func (sv *server) mcp(r *request) *response {
	body := parseJSON(r.body)
	if body == nil {
		return &response{status: 400, ctype: "application/json", body: []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)}
	}
	req := jobj(body)
	if req == nil {
		return &response{status: 400, ctype: "application/json", body: []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)}
	}
	id := req["id"]
	method := jstr(req["method"])
	params := jobj(req["params"])
	// notifications (no id) get 202 empty
	if id == nil {
		if method == "notifications/initialized" {
			return &response{status: 202, ctype: "application/json", body: []byte{}}
		}
		return &response{status: 202, ctype: "application/json", body: []byte{}}
	}
	switch method {
	case "server/discover":
		return sv.mcpResult(id, `{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"serverInfo":{"name":"forge","version":"1.0.0"},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"forge","version":"1.0.0"}}}`)
	case "initialize":
		pv := jstr(params["protocolVersion"])
		if pv == "" {
			pv = "2026-07-28"
		}
		var j jb
		j.raw(`{"protocolVersion":`)
		j.str(pv)
		j.raw(`,"capabilities":{"tools":{}},"serverInfo":{"name":"forge","version":"1.0.0"}}`)
		return sv.mcpResult(id, string(j.b))
	case "tools/list":
		return sv.mcpResult(id, string(mcpToolsListJSON))
	case "tools/call":
		return sv.mcpCall(id, params)
	case "ping":
		return sv.mcpResult(id, `{}`)
	}
	return sv.mcpError(id, -32601, "Method not found")
}

func (sv *server) mcpResult(id any, resultJSON string) *response {
	var j jb
	j.raw(`{"jsonrpc":"2.0","id":`)
	j.bytes(marshalJSON(id))
	j.raw(`,"result":`)
	j.raw(resultJSON)
	j.raw(`}`)
	return jsonResp(200, j.b)
}

func (sv *server) mcpError(id any, code int, msg string) *response {
	var j jb
	j.raw(`{"jsonrpc":"2.0","id":`)
	j.bytes(marshalJSON(id))
	j.raw(`,"error":{"code":`)
	j.num(code)
	j.raw(`,"message":`)
	j.str(msg)
	j.raw(`}}`)
	return jsonResp(200, j.b)
}

func (sv *server) mcpCall(id any, params map[string]any) *response {
	toolName := jstr(params["name"])
	args := jobj(params["arguments"])
	if args == nil {
		args = map[string]any{}
	}
	var tool *mcpTool
	for i := range mcpTools {
		if mcpTools[i].name == toolName {
			tool = &mcpTools[i]
			break
		}
	}
	if tool == nil {
		return sv.mcpError(id, -32602, "Unknown tool: "+toolName)
	}
	for _, req := range tool.required {
		if _, ok := args[req]; !ok {
			return sv.mcpError(id, -32602, "Missing required argument: "+req)
		}
	}
	switch toolName {
	case "create_repo":
		name := jstr(args["name"])
		desc := jstr(args["description"])
		priv := jbool(args["private"], false)
		repo, err := sv.st.createRepo(sv.st.login, name, desc, priv)
		if err != nil {
			return sv.mcpError(id, -32602, "Repository already exists")
		}
		return sv.mcpToolResult(id, sv.st.repoJSON(repo))
	case "get_repo":
		owner := jstr(args["owner"])
		name := jstr(args["repo"])
		repo := sv.st.getRepo(owner, name)
		if repo == nil {
			return sv.mcpError(id, -32602, "Repository not found")
		}
		return sv.mcpToolResult(id, sv.st.repoJSON(repo))
	case "create_issue":
		owner := jstr(args["owner"])
		name := jstr(args["repo"])
		title := jstr(args["title"])
		bodyText := jstr(args["body"])
		var labelNames []string
		for _, v := range jarr(args["labels"]) {
			if s, ok := v.(string); ok {
				labelNames = append(labelNames, s)
			}
		}
		sv.st.mu.Lock()
		repo := sv.st.repos[repoKey(owner, name)]
		if repo == nil {
			sv.st.mu.Unlock()
			return sv.mcpError(id, -32602, "Repository not found")
		}
		is, err := sv.st.createIssueLocked(repo, title, bodyText, labelNames)
		if err != nil {
			sv.st.mu.Unlock()
			return sv.mcpError(id, -32602, "Unknown label")
		}
		json := sv.st.issueJSON(is)
		sv.st.mu.Unlock()
		return sv.mcpToolResult(id, json)
	case "list_issues":
		owner := jstr(args["owner"])
		name := jstr(args["repo"])
		state := jstr(args["state"])
		if state == "" {
			state = "open"
		}
		sv.st.mu.RLock()
		repo := sv.st.repos[repoKey(owner, name)]
		if repo == nil {
			sv.st.mu.RUnlock()
			return sv.mcpError(id, -32602, "Repository not found")
		}
		var list []*issue
		for _, is := range repo.issues {
			if state == "all" || is.state == state {
				list = append(list, is)
			}
		}
		sv.st.mu.RUnlock()
		// sort by number ascending (creation order)
		for i := 0; i < len(list); i++ {
			for k := i + 1; k < len(list); k++ {
				if list[k].number < list[i].number {
					list[i], list[k] = list[k], list[i]
				}
			}
		}
		var j jb
		j.raw(`[`)
		for i, is := range list {
			if i > 0 {
				j.raw(`,`)
			}
			j.bytes(sv.st.issueJSON(is))
		}
		j.raw(`]`)
		return sv.mcpToolResult(id, j.b)
	case "create_pull_request":
		owner := jstr(args["owner"])
		name := jstr(args["repo"])
		title := jstr(args["title"])
		bodyText := jstr(args["body"])
		head := jstr(args["head"])
		base := jstr(args["base"])
		sv.st.mu.Lock()
		repo := sv.st.repos[repoKey(owner, name)]
		if repo == nil {
			sv.st.mu.Unlock()
			return sv.mcpError(id, -32602, "Repository not found")
		}
		p, err := sv.st.createPRLocked(repo, head, base, title, bodyText)
		if err != nil {
			sv.st.mu.Unlock()
			return sv.mcpError(id, -32602, "A pull request already exists for these branches")
		}
		json := sv.st.prJSON(p)
		sv.st.mu.Unlock()
		return sv.mcpToolResult(id, json)
	case "merge_pull_request":
		owner := jstr(args["owner"])
		name := jstr(args["repo"])
		num := jnum(args["pull_number"])
		sv.st.mu.Lock()
		repo := sv.st.repos[repoKey(owner, name)]
		if repo == nil {
			sv.st.mu.Unlock()
			return sv.mcpError(id, -32602, "Repository not found")
		}
		p, ok := repo.prs[num]
		if !ok {
			sv.st.mu.Unlock()
			return sv.mcpError(id, -32602, "Pull request not found")
		}
		err := sv.st.mergePRLocked(repo, p)
		if err == errConflict {
			sv.st.mu.Unlock()
			return sv.mcpError(id, -32602, "Merge conflict")
		}
		if err != nil {
			sv.st.mu.Unlock()
			return sv.mcpError(id, -32602, err.Error())
		}
		sha := p.mergeCommitSHA
		sv.st.mu.Unlock()
		var j jb
		j.raw(`{"merged":true,"sha":`)
		j.str(sha)
		j.raw(`,"message":"Pull request #`)
		j.num(num)
		j.raw(` successfully merged"}`)
		return sv.mcpToolResult(id, j.b)
	}
	return sv.mcpError(id, -32602, "Unknown tool: "+toolName)
}

// mcpToolResult wraps a JSON payload as a text content item + structuredContent.
func (sv *server) mcpToolResult(id any, payload []byte) *response {
	var j jb
	j.raw(`{"jsonrpc":"2.0","id":`)
	j.bytes(marshalJSON(id))
	j.raw(`,"result":{"content":[{"type":"text","text":`)
	j.str(string(payload))
	j.raw(`}],"structuredContent":`)
	j.bytes(payload)
	j.raw(`}}`)
	return jsonResp(200, j.b)
}
