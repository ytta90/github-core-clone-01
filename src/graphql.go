package main

import (
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
)

// ---- GraphQL parser ----
// Supports the documents the suite sends: operations (named/anonymous),
// variables, aliases, arguments (string/int/bool/null/enum/list/object),
// inline fragments, named fragment spreads (inlined), directives (skipped).

type gqlField struct {
	alias     string
	name      string
	args      map[string]any
	selection []*gqlField
	// inline fragment
	fragment bool
	typeCond string
}

type gqlOperation struct {
	kind      string // query | mutation
	name      string
	varDefs   map[string]any // variable name -> default value
	selection []*gqlField
}

type gqlParser struct {
	s string
	i int
}

func (p *gqlParser) ws() {
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.i++
			continue
		}
		if c == '#' {
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				p.i++
			}
			continue
		}
		return
	}
}

func (p *gqlParser) peek() byte {
	p.ws()
	if p.i >= len(p.s) {
		return 0
	}
	return p.s[p.i]
}

func (p *gqlParser) expect(c byte) bool {
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *gqlParser) ident() string {
	p.ws()
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			p.i++
		} else {
			break
		}
	}
	return p.s[start:p.i]
}

func (p *gqlParser) str() (string, bool) {
	p.ws()
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		return "", false
	}
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '"' {
			p.i++
			return b.String(), true
		}
		if c == '\\' {
			p.i++
			if p.i >= len(p.s) {
				return "", false
			}
			e := p.s[p.i]
			p.i++
			switch e {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				if p.i+4 > len(p.s) {
					return "", false
				}
				n, err := strconv.ParseUint(p.s[p.i:p.i+4], 16, 32)
				if err != nil {
					return "", false
				}
				p.i += 4
				b.WriteRune(rune(n))
			default:
				return "", false
			}
			continue
		}
		b.WriteByte(c)
		p.i++
	}
	return "", false
}

// parseValue parses a GraphQL value literal (variables resolved by the caller).
func (p *gqlParser) parseValue() (any, bool) {
	p.ws()
	if p.i >= len(p.s) {
		return nil, false
	}
	switch c := p.s[p.i]; {
	case c == '"':
		return p.str()
	case c == '$':
		p.i++
		name := p.ident()
		return gqlVar{name: name}, true
	case c == '[':
		p.i++
		var out []any
		for {
			p.ws()
			if p.i < len(p.s) && p.s[p.i] == ']' {
				p.i++
				return out, true
			}
			v, ok := p.parseValue()
			if !ok {
				return nil, false
			}
			out = append(out, v)
		}
	case c == '{':
		p.i++
		m := map[string]any{}
		for {
			p.ws()
			if p.i < len(p.s) && p.s[p.i] == '}' {
				p.i++
				return m, true
			}
			k := p.ident()
			if k == "" || !p.expect(':') {
				return nil, false
			}
			v, ok := p.parseValue()
			if !ok {
				return nil, false
			}
			m[k] = v
		}
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.i
		for p.i < len(p.s) {
			c := p.s[p.i]
			if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '-' || c == '+' {
				p.i++
			} else {
				break
			}
		}
		num := p.s[start:p.i]
		if f, err := strconv.ParseFloat(num, 64); err == nil {
			return f, true
		}
		return nil, false
	default:
		name := p.ident()
		if name == "" {
			return nil, false
		}
		switch name {
		case "true":
			return true, true
		case "false":
			return false, true
		case "null":
			return nil, true
		}
		return gqlEnum(name), true
	}
}

type gqlVar struct{ name string }
type gqlEnum string

// skipDirectives consumes @name(...) directives.
func (p *gqlParser) skipDirectives() {
	for {
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '@' {
			p.i++
			p.ident()
			p.ws()
			if p.i < len(p.s) && p.s[p.i] == '(' {
				p.parseArgs()
			}
			continue
		}
		return
	}
}

func (p *gqlParser) parseArgs() map[string]any {
	args := map[string]any{}
	if !p.expect('(') {
		return args
	}
	for {
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == ')' {
			p.i++
			return args
		}
		k := p.ident()
		if k == "" || !p.expect(':') {
			return args
		}
		v, ok := p.parseValue()
		if !ok {
			return args
		}
		args[k] = v
	}
}

func (p *gqlParser) parseSelection(frags map[string][]*gqlField) ([]*gqlField, bool) {
	if !p.expect('{') {
		return nil, false
	}
	var out []*gqlField
	for {
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return out, true
		}
		// fragment?
		if p.i+2 < len(p.s) && p.s[p.i] == '.' && p.s[p.i+1] == '.' && p.s[p.i+2] == '.' {
			p.i += 3
			p.ws()
			if p.i < len(p.s) && p.s[p.i] == 'o' && strings.HasPrefix(p.s[p.i:], "on ") {
				p.i += 3
				cond := p.ident()
				p.skipDirectives()
				sel, ok := p.parseSelection(frags)
				if !ok {
					return nil, false
				}
				out = append(out, &gqlField{fragment: true, typeCond: cond, selection: sel})
				continue
			}
			// named spread: inline it
			name := p.ident()
			p.skipDirectives()
			if sel, ok := frags[name]; ok {
				out = append(out, sel...)
			}
			continue
		}
		name := p.ident()
		if name == "" {
			return nil, false
		}
		f := &gqlField{name: name}
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == ':' {
			p.i++
			alias := name
			name = p.ident()
			f.alias, f.name = alias, name
		}
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '(' {
			f.args = p.parseArgs()
		}
		p.skipDirectives()
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '{' {
			sel, ok := p.parseSelection(frags)
			if !ok {
				return nil, false
			}
			f.selection = sel
		}
		out = append(out, f)
	}
}

func (p *gqlParser) parseType() {
	// consumes a type reference like [String!]! or ID
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '[' {
		p.i++
		p.parseType()
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
		}
	} else {
		p.ident()
	}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '!' {
		p.i++
	}
}

func parseGraphQL(doc string) ([]*gqlOperation, map[string][]*gqlField, error) {
	p := &gqlParser{s: doc}
	frags := map[string][]*gqlField{}
	var ops []*gqlOperation
	for {
		p.ws()
		if p.i >= len(p.s) {
			break
		}
		kw := p.ident()
		switch kw {
		case "query", "mutation", "subscription":
			op := &gqlOperation{kind: kw, varDefs: map[string]any{}}
			// optional name
			p.ws()
			if p.i < len(p.s) && p.s[p.i] != '(' && p.s[p.i] != '{' {
				op.name = p.ident()
			}
			// variable definitions
			p.ws()
			if p.i < len(p.s) && p.s[p.i] == '(' {
				p.i++
				for {
					p.ws()
					if p.i < len(p.s) && p.s[p.i] == ')' {
						p.i++
						break
					}
					if p.i < len(p.s) && p.s[p.i] == '$' {
						p.i++
						name := p.ident()
						p.ws()
						if p.i < len(p.s) && p.s[p.i] == ':' {
							p.i++
						}
						p.parseType()
						p.ws()
						if p.i < len(p.s) && p.s[p.i] == '=' {
							p.i++
							if v, ok := p.parseValue(); ok {
								op.varDefs[name] = v
							}
						} else {
							op.varDefs[name] = nil
						}
						continue
					}
					break
				}
			}
			p.skipDirectives()
			sel, ok := p.parseSelection(frags)
			if !ok {
				return nil, nil, errParse
			}
			op.selection = sel
			ops = append(ops, op)
		case "fragment":
			name := p.ident()
			p.ws()
			if strings.HasPrefix(p.s[p.i:], "on ") {
				p.i += 3
				p.ident()
			}
			p.skipDirectives()
			sel, ok := p.parseSelection(frags)
			if !ok {
				return nil, nil, errParse
			}
			frags[name] = sel
		case "":
			// shorthand { ... }
			if p.peek() == '{' {
				sel, ok := p.parseSelection(frags)
				if !ok {
					return nil, nil, errParse
				}
				ops = append(ops, &gqlOperation{kind: "query", varDefs: map[string]any{}, selection: sel})
			} else {
				return nil, nil, errParse
			}
		default:
			return nil, nil, errParse
		}
	}
	return ops, frags, nil
}

var errParse = errorString("graphql parse error")

type errorString string

func (e errorString) Error() string { return string(e) }

// ---- execution ----

type gqlError struct{ message string }

type gnode interface {
	typeName() string
	field(name string, args map[string]any) (any, error)
}

type execCtx struct {
	sv   *server
	vars map[string]any
	errs []gqlError
}

// resolveValue turns a raw argument value into its runtime value (variables,
// enums -> strings, nested objects/lists recursively).
func (x *execCtx) resolveValue(v any) any {
	switch t := v.(type) {
	case gqlVar:
		if val, ok := x.vars[t.name]; ok {
			return val
		}
		return nil
	case gqlEnum:
		return string(t)
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = x.resolveValue(val)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = x.resolveValue(val)
		}
		return out
	}
	return v
}

func (x *execCtx) resolveArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = x.resolveValue(v)
	}
	return out
}

// resolve walks a selection set against a node, building the response object.
func (x *execCtx) resolve(node gnode, sel []*gqlField) map[string]any {
	out := map[string]any{}
	for _, f := range sel {
		if f.fragment {
			if f.typeCond != "" && f.typeCond != node.typeName() {
				continue
			}
			sub := x.resolve(node, f.selection)
			for k, v := range sub {
				out[k] = v
			}
			continue
		}
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		v, err := node.field(f.name, x.resolveArgs(f.args))
		if err != nil {
			x.errs = append(x.errs, gqlError{message: err.Error()})
			out[key] = nil
			continue
		}
		out[key] = x.materialize(v, f.selection)
	}
	return out
}

// materialize converts a field value into its JSON form, resolving nested nodes.
func (x *execCtx) materialize(v any, sel []*gqlField) any {
	switch t := v.(type) {
	case gnode:
		if len(sel) == 0 {
			return map[string]any{}
		}
		return x.resolve(t, sel)
	case []gnode:
		out := make([]any, len(t))
		for i, n := range t {
			out[i] = x.materialize(n, sel)
		}
		return out
	case nil:
		return nil
	}
	return v
}

func unknownField(field, typ string) error {
	return errorString("Cannot query field \"" + field + "\" on type \"" + typ + "\".")
}

// ---- global ids ----

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func decodeB64url(s string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func repoGlobalID(r *repo) string         { return b64url("repo:" + itoa(r.id)) }
func issueGlobalID(r *repo, n int) string { return b64url("issue:" + itoa(r.id) + ":" + itoa(n)) }
func prGlobalID(r *repo, n int) string    { return b64url("pr:" + itoa(r.id) + ":" + itoa(n)) }
func commentGlobalID(r *repo, n, cid int) string {
	return b64url("comment:" + itoa(r.id) + ":" + itoa(n) + ":" + itoa(cid))
}

// ---- nodes ----

type userNode struct{ login string }

func (n *userNode) typeName() string { return "User" }
func (n *userNode) field(name string, args map[string]any) (any, error) {
	switch name {
	case "login":
		return n.login, nil
	case "id":
		return 1000, nil
	case "name":
		return n.login, nil
	case "email":
		return n.login + "@example.invalid", nil
	}
	return nil, unknownField(name, "User")
}

type refNode struct{ name string }

func (n *refNode) typeName() string { return "Ref" }
func (n *refNode) field(name string, args map[string]any) (any, error) {
	if name == "name" {
		return n.name, nil
	}
	return nil, unknownField(name, "Ref")
}

type issueNode struct {
	repo *repo
	is   *issue
}

func (n *issueNode) typeName() string { return "Issue" }
func (n *issueNode) field(name string, args map[string]any) (any, error) {
	switch name {
	case "id":
		return issueGlobalID(n.repo, n.is.number), nil
	case "number":
		return n.is.number, nil
	case "title":
		return n.is.title, nil
	case "body":
		return n.is.body, nil
	case "state":
		if n.is.state == "closed" {
			return "CLOSED", nil
		}
		return "OPEN", nil
	}
	return nil, unknownField(name, "Issue")
}

type issueConnNode struct {
	repo      *repo
	issues    []*issue
	hasNext   bool
	endCursor string
}

func (n *issueConnNode) typeName() string { return "IssueConnection" }
func (n *issueConnNode) field(name string, args map[string]any) (any, error) {
	switch name {
	case "nodes":
		var out []gnode
		for _, is := range n.issues {
			out = append(out, &issueNode{repo: n.repo, is: is})
		}
		return out, nil
	case "pageInfo":
		return &pageInfoNode{hasNext: n.hasNext, endCursor: n.endCursor}, nil
	case "totalCount":
		return len(n.issues), nil
	}
	return nil, unknownField(name, "IssueConnection")
}

type pageInfoNode struct {
	hasNext   bool
	endCursor string
}

func (n *pageInfoNode) typeName() string { return "PageInfo" }
func (n *pageInfoNode) field(name string, args map[string]any) (any, error) {
	switch name {
	case "hasNextPage":
		return n.hasNext, nil
	case "endCursor":
		if n.endCursor == "" {
			return nil, nil
		}
		return n.endCursor, nil
	case "hasPreviousPage":
		return false, nil
	case "startCursor":
		return nil, nil
	}
	return nil, unknownField(name, "PageInfo")
}

type prNode struct {
	repo         *repo
	p            *pr
	filePaths    []string
	mergeableVal string
}

func (n *prNode) typeName() string { return "PullRequest" }
func (n *prNode) field(name string, args map[string]any) (any, error) {
	switch name {
	case "id":
		return prGlobalID(n.repo, n.p.number), nil
	case "number":
		return n.p.number, nil
	case "title":
		return n.p.title, nil
	case "body":
		return n.p.body, nil
	case "state":
		if n.p.merged {
			return "MERGED", nil
		}
		if n.p.state == "closed" {
			return "CLOSED", nil
		}
		return "OPEN", nil
	case "merged":
		return n.p.merged, nil
	case "headRefName":
		return n.p.headRef, nil
	case "baseRefName":
		return n.p.baseRef, nil
	case "mergeable":
		if n.mergeableVal == "" {
			return "MERGEABLE", nil
		}
		return n.mergeableVal, nil
	case "files":
		first := 100
		if v, ok := args["first"]; ok {
			first = jnum(v)
		}
		return n.files(first), nil
	}
	return nil, unknownField(name, "PullRequest")
}

func (n *prNode) files(first int) gnode {
	if n.filePaths == nil {
		return &fileConnNode{}
	}
	paths := n.filePaths
	if first < len(paths) {
		paths = paths[:first]
	}
	return &fileConnNode{paths: paths}
}

type fileConnNode struct{ paths []string }

func (n *fileConnNode) typeName() string { return "PullRequestFileConnection" }
func (n *fileConnNode) field(name string, args map[string]any) (any, error) {
	if name == "nodes" {
		var out []gnode
		for _, p := range n.paths {
			out = append(out, &fileNode{path: p})
		}
		return out, nil
	}
	return nil, unknownField(name, "PullRequestFileConnection")
}

type fileNode struct{ path string }

func (n *fileNode) typeName() string { return "PullRequestFile" }
func (n *fileNode) field(name string, args map[string]any) (any, error) {
	if name == "path" {
		return n.path, nil
	}
	return nil, unknownField(name, "PullRequestFile")
}

type repoNode struct {
	sv   *server
	repo *repo
}

func (n *repoNode) typeName() string { return "Repository" }
func (n *repoNode) field(name string, args map[string]any) (any, error) {
	r := n.repo
	switch name {
	case "id":
		return repoGlobalID(r), nil
	case "databaseId":
		return r.id, nil
	case "name":
		return r.name, nil
	case "nameWithOwner":
		return r.owner + "/" + r.name, nil
	case "owner":
		return &userNode{login: r.owner}, nil
	case "defaultBranchRef":
		tip := n.sv.st.refTarget(r, "refs/heads/"+r.defaultBranch)
		if tip == "" {
			return nil, nil
		}
		return &refNode{name: r.defaultBranch}, nil
	case "viewerPermission":
		return "ADMIN", nil
	case "hasIssuesEnabled":
		return true, nil
	case "issue":
		num := jnum(args["number"])
		is, ok := r.issues[num]
		if !ok {
			return nil, nil
		}
		return &issueNode{repo: r, is: is}, nil
	case "issues":
		return n.issues(args), nil
	case "pullRequests":
		first := 100
		if v, ok := args["first"]; ok {
			first = jnum(v)
		}
		var list []*pr
		for _, p := range r.prs {
			list = append(list, p)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].seq > list[j].seq })
		if first < len(list) {
			list = list[:first]
		}
		return &prConnNode{sv: n.sv, repo: r, prs: list}, nil
	case "pullRequest":
		num := jnum(args["number"])
		p, ok := r.prs[num]
		if !ok {
			return nil, nil
		}
		return n.sv.newPRNode(r, p), nil
	}
	return nil, unknownField(name, "Repository")
}

func (n *repoNode) issues(args map[string]any) gnode {
	first := 100
	if v, ok := args["first"]; ok {
		first = jnum(v)
	}
	var states []string
	if v, ok := args["states"]; ok {
		for _, s := range jarr(v) {
			states = append(states, jstr(s))
		}
	}
	orderBy := map[string]any{}
	if v, ok := args["orderBy"]; ok {
		orderBy = jobj(v)
	}
	dir := jstr(orderBy["direction"])
	if dir == "" {
		dir = "DESC"
	}
	after := jstr(args["after"])
	afterNum := -1
	if after != "" {
		if dec, ok := decodeB64url(after); ok {
			parts := strings.Split(dec, ":")
			if len(parts) == 2 && parts[0] == "issue" {
				afterNum, _ = strconv.Atoi(parts[1])
			}
		}
	}
	var list []*issue
	for _, is := range n.repo.issues {
		if len(states) > 0 {
			want := "OPEN"
			if is.state == "closed" {
				want = "CLOSED"
			}
			found := false
			for _, s := range states {
				if s == want {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		list = append(list, is)
	}
	sort.Slice(list, func(i, j int) bool {
		if dir == "ASC" {
			if list[i].seq == list[j].seq {
				return list[i].number < list[j].number
			}
			return list[i].seq < list[j].seq
		}
		if list[i].seq == list[j].seq {
			return list[i].number > list[j].number
		}
		return list[i].seq > list[j].seq
	})
	if afterNum >= 0 {
		var filtered []*issue
		for _, is := range list {
			if dir == "ASC" {
				if is.number > afterNum {
					filtered = append(filtered, is)
				}
			} else {
				if is.number < afterNum {
					filtered = append(filtered, is)
				}
			}
		}
		list = filtered
	}
	hasNext := len(list) > first
	if hasNext {
		list = list[:first]
	}
	endCursor := ""
	if len(list) > 0 {
		endCursor = b64url("issue:" + itoa(list[len(list)-1].number))
	}
	return &issueConnNode{repo: n.repo, issues: list, hasNext: hasNext, endCursor: endCursor}
}

type prConnNode struct {
	sv   *server
	repo *repo
	prs  []*pr
}

func (n *prConnNode) typeName() string { return "PullRequestConnection" }
func (n *prConnNode) field(name string, args map[string]any) (any, error) {
	switch name {
	case "nodes":
		var out []gnode
		for _, p := range n.prs {
			out = append(out, n.sv.newPRNode(n.repo, p))
		}
		return out, nil
	case "totalCount":
		return len(n.prs), nil
	}
	return nil, unknownField(name, "PullRequestConnection")
}

// typeNode serves __type introspection.
type typeNode struct{ name string }

var gqlSchema = map[string]struct {
	fields []string
	enums  []string
}{
	"Issue":       {fields: []string{"id", "number", "title", "state", "body"}},
	"PullRequest": {fields: []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"}},
	"Repository":  {fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}},
	"SearchType":  {enums: []string{"ISSUE"}},
}

func (n *typeNode) typeName() string { return "__Type" }
func (n *typeNode) field(name string, args map[string]any) (any, error) {
	switch name {
	case "name":
		return n.name, nil
	case "fields":
		t, ok := gqlSchema[n.name]
		if !ok {
			return nil, nil
		}
		var out []gnode
		for _, f := range t.fields {
			out = append(out, &fieldNode{name: f})
		}
		return out, nil
	case "enumValues":
		t, ok := gqlSchema[n.name]
		if !ok {
			return nil, nil
		}
		var out []gnode
		for _, e := range t.enums {
			out = append(out, &enumNode{name: e})
		}
		return out, nil
	}
	return nil, unknownField(name, "__Type")
}

type fieldNode struct{ name string }

func (n *fieldNode) typeName() string { return "__Field" }
func (n *fieldNode) field(name string, args map[string]any) (any, error) {
	if name == "name" {
		return n.name, nil
	}
	return nil, unknownField(name, "__Field")
}

type enumNode struct{ name string }

func (n *enumNode) typeName() string { return "__EnumValue" }
func (n *enumNode) field(name string, args map[string]any) (any, error) {
	if name == "name" {
		return n.name, nil
	}
	return nil, unknownField(name, "__EnumValue")
}

// ---- root ----

type queryRoot struct{ sv *server }

func (n *queryRoot) typeName() string { return "Query" }
func (n *queryRoot) field(name string, args map[string]any) (any, error) {
	switch name {
	case "viewer":
		return &userNode{login: n.sv.st.login}, nil
	case "repository":
		owner := jstr(args["owner"])
		repoName := jstr(args["name"])
		r := n.sv.st.repos[repoKey(owner, repoName)]
		if r == nil {
			return nil, nil
		}
		return &repoNode{sv: n.sv, repo: r}, nil
	case "node":
		id := jstr(args["id"])
		dec, ok := decodeB64url(id)
		if !ok {
			return nil, nil
		}
		parts := strings.Split(dec, ":")
		if len(parts) >= 2 && parts[0] == "repo" {
			dbid, _ := strconv.Atoi(parts[1])
			var found *repo
			for _, r := range n.sv.st.repos {
				if r.id == dbid {
					found = r
					break
				}
			}
			if found == nil {
				return nil, nil
			}
			return &repoNode{sv: n.sv, repo: found}, nil
		}
		return nil, nil
	case "__type":
		tname := jstr(args["name"])
		if _, ok := gqlSchema[tname]; !ok {
			return nil, nil
		}
		return &typeNode{name: tname}, nil
	case "__schema":
		return &schemaNode{}, nil
	}
	return nil, unknownField(name, "Query")
}

type schemaNode struct{}

func (n *schemaNode) typeName() string { return "__Schema" }
func (n *schemaNode) field(name string, args map[string]any) (any, error) {
	if name == "queryType" {
		return &typeNode{name: "Query"}, nil
	}
	return nil, unknownField(name, "__Schema")
}

type mutationRoot struct{ sv *server }

func (n *mutationRoot) typeName() string { return "Mutation" }
func (n *mutationRoot) field(name string, args map[string]any) (any, error) {
	input := jobj(args["input"])
	switch name {
	case "createIssue":
		repoID := jstr(input["repositoryId"])
		title := jstr(input["title"])
		body := jstr(input["body"])
		dec, ok := decodeB64url(repoID)
		if !ok {
			return nil, errorString("invalid repositoryId")
		}
		parts := strings.Split(dec, ":")
		if len(parts) != 2 || parts[0] != "repo" {
			return nil, errorString("invalid repositoryId")
		}
		dbid, _ := strconv.Atoi(parts[1])
		n.sv.st.mu.Lock()
		var r *repo
		for _, rr := range n.sv.st.repos {
			if rr.id == dbid {
				r = rr
				break
			}
		}
		if r == nil {
			n.sv.st.mu.Unlock()
			return nil, errorString("repository not found")
		}
		is, err := n.sv.st.createIssueLocked(r, title, body, nil)
		n.sv.st.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &payloadNode{fields: map[string]any{"issue": &issueNode{repo: r, is: is}}}, nil
	case "closeIssue":
		issueID := jstr(input["issueId"])
		dec, ok := decodeB64url(issueID)
		if !ok {
			return nil, errorString("invalid issueId")
		}
		parts := strings.Split(dec, ":")
		if len(parts) != 3 || parts[0] != "issue" {
			return nil, errorString("invalid issueId")
		}
		dbid, _ := strconv.Atoi(parts[1])
		num, _ := strconv.Atoi(parts[2])
		n.sv.st.mu.Lock()
		var r *repo
		for _, rr := range n.sv.st.repos {
			if rr.id == dbid {
				r = rr
				break
			}
		}
		if r == nil {
			n.sv.st.mu.Unlock()
			return nil, errorString("repository not found")
		}
		is, ok := r.issues[num]
		if !ok {
			n.sv.st.mu.Unlock()
			return nil, errorString("issue not found")
		}
		is.state = "closed"
		is.jsonCache = nil
		n.sv.st.mu.Unlock()
		return &payloadNode{fields: map[string]any{"issue": &issueNode{repo: r, is: is}}}, nil
	case "createPullRequest":
		repoID := jstr(input["repositoryId"])
		base := jstr(input["baseRefName"])
		head := jstr(input["headRefName"])
		title := jstr(input["title"])
		body := jstr(input["body"])
		dec, ok := decodeB64url(repoID)
		if !ok {
			return nil, errorString("invalid repositoryId")
		}
		parts := strings.Split(dec, ":")
		if len(parts) != 2 || parts[0] != "repo" {
			return nil, errorString("invalid repositoryId")
		}
		dbid, _ := strconv.Atoi(parts[1])
		n.sv.st.mu.Lock()
		var r *repo
		for _, rr := range n.sv.st.repos {
			if rr.id == dbid {
				r = rr
				break
			}
		}
		if r == nil {
			n.sv.st.mu.Unlock()
			return nil, errorString("repository not found")
		}
		p, err := n.sv.st.createPRLocked(r, head, base, title, body)
		n.sv.st.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &payloadNode{fields: map[string]any{"pullRequest": n.sv.newPRNode(r, p)}}, nil
	case "mergePullRequest":
		prID := jstr(input["pullRequestId"])
		dec, ok := decodeB64url(prID)
		if !ok {
			return nil, errorString("invalid pullRequestId")
		}
		parts := strings.Split(dec, ":")
		if len(parts) != 3 || parts[0] != "pr" {
			return nil, errorString("invalid pullRequestId")
		}
		dbid, _ := strconv.Atoi(parts[1])
		num, _ := strconv.Atoi(parts[2])
		n.sv.st.mu.Lock()
		var r *repo
		for _, rr := range n.sv.st.repos {
			if rr.id == dbid {
				r = rr
				break
			}
		}
		if r == nil {
			n.sv.st.mu.Unlock()
			return nil, errorString("repository not found")
		}
		p, ok := r.prs[num]
		if !ok {
			n.sv.st.mu.Unlock()
			return nil, errorString("pull request not found")
		}
		err := n.sv.st.mergePRLocked(r, p)
		n.sv.st.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &payloadNode{fields: map[string]any{"pullRequest": n.sv.newPRNode(r, p)}}, nil
	case "addComment":
		subjectID := jstr(input["subjectId"])
		body := jstr(input["body"])
		dec, ok := decodeB64url(subjectID)
		if !ok {
			return nil, errorString("invalid subjectId")
		}
		parts := strings.Split(dec, ":")
		if len(parts) != 3 || parts[0] != "issue" {
			return nil, errorString("invalid subjectId")
		}
		dbid, _ := strconv.Atoi(parts[1])
		num, _ := strconv.Atoi(parts[2])
		n.sv.st.mu.Lock()
		var r *repo
		for _, rr := range n.sv.st.repos {
			if rr.id == dbid {
				r = rr
				break
			}
		}
		if r == nil {
			n.sv.st.mu.Unlock()
			return nil, errorString("subject not found")
		}
		is, ok := r.issues[num]
		if !ok {
			n.sv.st.mu.Unlock()
			return nil, errorString("issue not found")
		}
		c := &comment{id: r.nextCommentID, body: body}
		r.nextCommentID++
		is.comments = append(is.comments, c)
		n.sv.st.mu.Unlock()
		return &payloadNode{fields: map[string]any{"commentEdge": &commentEdgeNode{repo: r, issueNum: num, c: c}}}, nil
	}
	return nil, unknownField(name, "Mutation")
}

type payloadNode struct{ fields map[string]any }

func (n *payloadNode) typeName() string { return "Payload" }
func (n *payloadNode) field(name string, args map[string]any) (any, error) {
	if v, ok := n.fields[name]; ok {
		return v, nil
	}
	return nil, unknownField(name, "Payload")
}

type commentEdgeNode struct {
	repo     *repo
	issueNum int
	c        *comment
}

func (n *commentEdgeNode) typeName() string { return "IssueCommentEdge" }
func (n *commentEdgeNode) field(name string, args map[string]any) (any, error) {
	if name == "node" {
		return &commentNode{repo: n.repo, issueNum: n.issueNum, c: n.c}, nil
	}
	return nil, unknownField(name, "IssueCommentEdge")
}

type commentNode struct {
	repo     *repo
	issueNum int
	c        *comment
}

func (n *commentNode) typeName() string { return "IssueComment" }
func (n *commentNode) field(name string, args map[string]any) (any, error) {
	switch name {
	case "id":
		return commentGlobalID(n.repo, n.issueNum, n.c.id), nil
	case "body":
		return n.c.body, nil
	}
	return nil, unknownField(name, "IssueComment")
}

// newPRNode builds a pr node wired to the store for mergeable/files.
// Caller must hold the store lock (read or write).
func (sv *server) newPRNode(r *repo, p *pr) *prNode {
	n := &prNode{repo: r, p: p}
	// file paths for the files connection
	files := sv.st.prFiles(r, p)
	var paths []string
	for _, f := range files {
		paths = append(paths, f["filename"].(string))
	}
	n.filePaths = paths
	// mergeable computed live
	if sv.st.prConflictLocked(r, p) {
		n.mergeableVal = "CONFLICTING"
	} else {
		n.mergeableVal = "MERGEABLE"
	}
	return n
}

// ---- handler ----

func (sv *server) graphql(r *request) *response {
	body := parseJSON(r.body)
	if body == nil {
		return jsonErr(400, "Invalid JSON")
	}
	if arr, ok := body.([]any); ok {
		var j jb
		j.raw(`[`)
		for i, item := range arr {
			if i > 0 {
				j.raw(`,`)
			}
			j.bytes(sv.execGraphQL(jobj(item)))
		}
		j.raw(`]`)
		return jsonResp(200, j.b)
	}
	return jsonResp(200, sv.execGraphQL(jobj(body)))
}

func (sv *server) execGraphQL(body map[string]any) []byte {
	query := jstr(body["query"])
	vars := jobj(body["variables"])
	opName := jstr(body["operationName"])
	if vars == nil {
		vars = map[string]any{}
	}
	ops, _, err := parseGraphQL(query)
	if err != nil {
		return gqlErrorResponse("Syntax Error: " + err.Error())
	}
	var op *gqlOperation
	for _, o := range ops {
		if opName != "" {
			if o.name == opName {
				op = o
				break
			}
		} else if len(ops) == 1 || o.kind == "query" {
			op = o
			break
		}
	}
	if op == nil {
		return gqlErrorResponse("Unknown operation")
	}
	// defaults
	for k, v := range op.varDefs {
		if _, ok := vars[k]; !ok && v != nil {
			vars[k] = v
		}
	}
	x := &execCtx{sv: sv, vars: vars}
	var root gnode
	if op.kind == "mutation" {
		root = &mutationRoot{sv: sv}
	} else {
		root = &queryRoot{sv: sv}
	}
	var data map[string]any
	if op.kind == "mutation" {
		// mutations take the write lock per-field inside their resolvers
		data = x.resolve(root, op.selection)
	} else {
		sv.st.mu.RLock()
		data = x.resolve(root, op.selection)
		sv.st.mu.RUnlock()
	}
	var j jb
	j.raw(`{"data":`)
	j.bytes(marshalJSON(data))
	if len(x.errs) > 0 {
		j.raw(`,"errors":[`)
		for i, e := range x.errs {
			if i > 0 {
				j.raw(`,`)
			}
			j.raw(`{"message":`)
			j.str(e.message)
			j.raw(`}`)
		}
		j.raw(`]`)
	}
	j.raw(`}`)
	return j.b
}

func gqlErrorResponse(msg string) []byte {
	var j jb
	j.raw(`{"data":null,"errors":[{"message":`)
	j.str(msg)
	j.raw(`}]}`)
	return j.b
}

// marshalJSON serializes a generic value (map[string]any / []any / scalar / nil).
func marshalJSON(v any) []byte {
	var j jb
	writeJSONValue(&j, v)
	return j.b
}

func writeJSONValue(j *jb, v any) {
	switch t := v.(type) {
	case nil:
		j.raw("null")
	case bool:
		j.boolean(t)
	case int:
		j.num(t)
	case float64:
		if t == float64(int64(t)) {
			j.num(int(t))
		} else {
			j.raw(strconv.FormatFloat(t, 'f', -1, 64))
		}
	case string:
		j.str(t)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		j.raw("{")
		for i, k := range keys {
			if i > 0 {
				j.raw(",")
			}
			j.str(k)
			j.raw(":")
			writeJSONValue(j, t[k])
		}
		j.raw("}")
	case []any:
		j.raw("[")
		for i, e := range t {
			if i > 0 {
				j.raw(",")
			}
			writeJSONValue(j, e)
		}
		j.raw("]")
	default:
		j.raw("null")
	}
}
