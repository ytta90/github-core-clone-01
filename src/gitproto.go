package main

import (
	"sort"
	"strings"
)

// ---- dispatcher ----

func (sv *server) dispatch(r *request) *response {
	if r.method == "GET" && r.path == "/healthz" {
		return jsonResp(200, []byte(`{"ok":true}`))
	}
	// auth: every other route needs a Bearer token matching the env token
	// (or any non-empty token when no env token is set).
	auth := r.hdr.authorization
	tok := ""
	if strings.HasPrefix(auth, "Bearer ") {
		tok = auth[len("Bearer "):]
	} else if strings.HasPrefix(auth, "token ") {
		tok = auth[len("token "):]
	}
	if tok == "" || (sv.st.token != "" && tok != sv.st.token) {
		return jsonErr(401, "Bad credentials")
	}
	// git smart HTTP: /{owner}/{repo}.git/...
	if len(r.segs) == 3 && strings.HasSuffix(r.segs[1], ".git") {
		owner := r.segs[0]
		repoName := strings.TrimSuffix(r.segs[1], ".git")
		switch r.segs[2] {
		case "info":
			// handled below (info/refs is 4 segs)
		case "git-upload-pack":
			if r.method == "POST" {
				return sv.gitUploadPack(r, owner, repoName)
			}
		case "git-receive-pack":
			if r.method == "POST" {
				return sv.gitReceivePack(r, owner, repoName)
			}
		}
	}
	if len(r.segs) == 4 && strings.HasSuffix(r.segs[1], ".git") && r.segs[2] == "info" && r.segs[3] == "refs" {
		if r.method == "GET" {
			return sv.gitInfoRefs(r, r.segs[0], strings.TrimSuffix(r.segs[1], ".git"))
		}
	}
	// REST / GraphQL / MCP
	if len(r.segs) >= 2 && r.segs[0] == "api" {
		if r.segs[1] == "graphql" && r.method == "POST" {
			return sv.graphql(r)
		}
		if r.segs[1] == "v3" {
			return sv.rest(r)
		}
	}
	if r.method == "POST" && r.path == "/mcp" {
		return sv.mcp(r)
	}
	return jsonErr(404, "Not Found")
}

// ---- git smart HTTP ----

func (sv *server) repoOr404(owner, name string) (*repo, *response) {
	r := sv.st.getRepo(owner, name)
	if r == nil {
		return nil, jsonErr(404, "Not Found")
	}
	return r, nil
}

const uploadCaps = "symref=HEAD:refs/heads/main side-band-64k thin-pack ofs-delta shallow multi_ack agent=forge/1.0"
const receiveCaps = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"

// sortedRefs returns ref names (excluding HEAD) sorted.
func sortedRefs(r *repo) []string {
	names := make([]string, 0, len(r.refs))
	for n := range r.refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// v0 advertisement body for a service.
func (sv *server) v0Advertisement(r *repo, service string) []byte {
	var out []byte
	out = append(out, pktLine([]byte("# service="+service+"\n"))...)
	out = append(out, pktFlush...)
	caps := uploadCaps
	if service == "git-receive-pack" {
		caps = receiveCaps
	}
	refs := sortedRefs(r)
	if len(refs) == 0 {
		line := zeroOID + " capabilities^{}\x00" + caps + "\n"
		out = append(out, pktLine([]byte(line))...)
	} else {
		headOID := sv.st.refTarget(r, "refs/heads/"+r.defaultBranch)
		if headOID == "" {
			headOID = zeroOID
		}
		first := true
		for _, name := range refs {
			oid := r.refs[name]
			line := oid + " " + name
			if first {
				line += "\x00" + caps
				first = false
			}
			line += "\n"
			out = append(out, pktLine([]byte(line))...)
			// peeled annotated tag
			if strings.HasPrefix(name, "refs/tags/") {
				if obj := r.objects[oid]; obj != nil && obj.typ == "tag" {
					target := parseTag(obj.data).object
					peeled := target + " " + name + "^{}\n"
					out = append(out, pktLine([]byte(peeled))...)
				}
			}
		}
	}
	out = append(out, pktFlush...)
	return out
}

func (sv *server) gitInfoRefs(r *request, owner, name string) *response {
	service := r.queryParam("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		return jsonErr(400, "Invalid service")
	}
	sv.st.mu.RLock()
	defer sv.st.mu.RUnlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	ct := "application/x-git-upload-pack-advertisement"
	if service == "git-receive-pack" {
		ct = "application/x-git-receive-pack-advertisement"
	}
	if service == "git-upload-pack" && r.hdr.gitProtocol == "version=2" {
		var out []byte
		out = append(out, pktLine([]byte("version 2\n"))...)
		out = append(out, pktLine([]byte("agent=forge/1.0\n"))...)
		out = append(out, pktLine([]byte("ls-refs\n"))...)
		out = append(out, pktLine([]byte("fetch=shallow filter\n"))...)
		out = append(out, pktLine([]byte("object-format=sha1\n"))...)
		out = append(out, pktFlush...)
		return &response{status: 200, ctype: ct, body: out}
	}
	return &response{status: 200, ctype: ct, body: sv.v0Advertisement(repo, service)}
}

// sideband wraps data into band-1 pkt-lines (chunked at 65515 bytes).
func sideband(data []byte) []byte {
	var out []byte
	for len(data) > 0 {
		n := len(data)
		if n > 65515 {
			n = 65515
		}
		chunk := make([]byte, 0, n+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, data[:n]...)
		out = append(out, pktLine(chunk)...)
		data = data[n:]
	}
	return out
}

func (sv *server) gitUploadPack(r *request, owner, name string) *response {
	sv.st.mu.RLock()
	defer sv.st.mu.RUnlock()
	repo := sv.st.repos[repoKey(owner, name)]
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	pkts, err := pktDecode(r.body)
	if err != nil {
		return jsonErr(400, "malformed request")
	}
	if len(pkts) > 0 && pkts[0].kind == 0 && strings.HasPrefix(string(pkts[0].payload), "command=") {
		cmd := strings.TrimSuffix(string(pkts[0].payload), "\n")
		switch cmd {
		case "command=ls-refs":
			return sv.v2LsRefs(r, repo, pkts[1:])
		case "command=fetch":
			return sv.v2Fetch(r, repo, pkts[1:])
		}
		return jsonErr(400, "unknown command")
	}
	return sv.v0Fetch(r, repo, pkts)
}

func (sv *server) v2LsRefs(r *request, repo *repo, pkts []pkt) *response {
	peel := false
	symrefs := false
	var prefixes []string
	for _, p := range pkts {
		if p.kind != 0 {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
	}
	var out []byte
	headOID := sv.st.refTarget(repo, "refs/heads/"+repo.defaultBranch)
	if headOID == "" {
		headOID = zeroOID
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	if len(prefixes) == 0 {
		line := headOID + " HEAD"
		if symrefs {
			line += " symref-target:refs/heads/" + repo.defaultBranch
		}
		out = append(out, pktLine([]byte(line+"\n"))...)
	}
	for _, name := range sortedRefs(repo) {
		if !matches(name) {
			continue
		}
		oid := repo.refs[name]
		line := oid + " " + name
		if peel {
			if obj := repo.objects[oid]; obj != nil && obj.typ == "tag" {
				line += " peeled:" + parseTag(obj.data).object
			}
		}
		out = append(out, pktLine([]byte(line+"\n"))...)
	}
	out = append(out, pktFlush...)
	return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: out}
}

func (sv *server) v2Fetch(r *request, repo *repo, pkts []pkt) *response {
	var wants, haves, shallows []string
	depth := 0
	inArgs := true
	for _, p := range pkts {
		if p.kind == 2 { // delim
			inArgs = false
			continue
		}
		if p.kind != 0 {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if inArgs {
			continue
		}
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, line[5:])
		case strings.HasPrefix(line, "have "):
			haves = append(haves, line[5:])
		case strings.HasPrefix(line, "shallow "):
			shallows = append(shallows, line[8:])
		case strings.HasPrefix(line, "deepen "):
			depth = atoi(line[7:])
		}
	}
	pack, shallow, unshallow := sv.st.packFor(repo, wants, haves, depth, shallows)
	var out []byte
	out = append(out, pktLine([]byte("acknowledgments\n"))...)
	out = append(out, pktLine([]byte("NAK\n"))...)
	out = append(out, pktDelim...)
	if len(shallow) > 0 || len(unshallow) > 0 {
		out = append(out, pktLine([]byte("shallow-info\n"))...)
		for _, s := range shallow {
			out = append(out, pktLine([]byte("shallow "+s+"\n"))...)
		}
		for _, s := range unshallow {
			out = append(out, pktLine([]byte("unshallow "+s+"\n"))...)
		}
		out = append(out, pktDelim...)
	}
	out = append(out, pktLine([]byte("packfile\n"))...)
	out = append(out, sideband(pack)...)
	out = append(out, pktFlush...)
	return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: out}
}

func (sv *server) v0Fetch(r *request, repo *repo, pkts []pkt) *response {
	var wants, haves, shallows []string
	depth := 0
	seenFlush := false
	for _, p := range pkts {
		if p.kind == 1 {
			seenFlush = true
			continue
		}
		if p.kind != 0 {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			rest := line[5:]
			if sp := strings.IndexByte(rest, ' '); sp >= 0 {
				rest = rest[:sp]
			}
			wants = append(wants, rest)
		case strings.HasPrefix(line, "have "):
			if seenFlush {
				haves = append(haves, line[5:])
			}
		case strings.HasPrefix(line, "shallow "):
			shallows = append(shallows, line[8:])
		case strings.HasPrefix(line, "deepen "):
			depth = atoi(line[7:])
		}
	}
	pack, shallow, unshallow := sv.st.packFor(repo, wants, haves, depth, shallows)
	var out []byte
	out = append(out, pktLine([]byte("NAK\n"))...)
	for _, s := range shallow {
		out = append(out, pktLine([]byte("shallow "+s+"\n"))...)
	}
	for _, s := range unshallow {
		out = append(out, pktLine([]byte("unshallow "+s+"\n"))...)
	}
	out = append(out, sideband(pack)...)
	out = append(out, pktFlush...)
	return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: out}
}

type pushCmd struct {
	old, new, ref string
}

func (sv *server) gitReceivePack(r *request, owner, name string) *response {
	repo, errResp := sv.repoOr404(owner, name)
	if errResp != nil {
		return errResp
	}
	// split body: pkt-lines until flush, then the raw pack
	rest := r.body
	var cmds []pushCmd
	packBytes := []byte{}
	for len(rest) >= 4 {
		if !isHex4(rest[:4]) {
			break
		}
		n := 0
		for _, c := range rest[:4] {
			n = n*16 + hexVal(c)
		}
		if n == 0 {
			// flush: the raw pack follows
			packBytes = rest[4:]
			break
		}
		if n < 4 || n > len(rest) {
			break
		}
		line := string(rest[4:n])
		rest = rest[n:]
		// command line (strip capabilities after NUL on the first line)
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		line = strings.TrimSuffix(line, "\n")
		parts := strings.SplitN(line, " ", 3)
		if len(parts) == 3 {
			cmds = append(cmds, pushCmd{old: parts[0], new: parts[1], ref: parts[2]})
		}
	}

	sv.st.mu.Lock()
	defer sv.st.mu.Unlock()

	unpackOK := true
	if len(packBytes) > 0 {
		if !verifyPackChecksum(packBytes) {
			unpackOK = false
		} else {
			objs, err := readPack(packBytes, func(oid string) *gitObject {
				return repo.objects[oid]
			})
			if err != nil {
				unpackOK = false
			} else {
				for oid, obj := range objs {
					if _, ok := repo.objects[oid]; !ok {
						repo.objects[oid] = obj
					}
				}
			}
		}
	}
	// report (inner pkt-lines inside band-1 sideband packets)
	var inner []byte
	if unpackOK {
		inner = append(inner, pktLine([]byte("unpack ok\n"))...)
	} else {
		inner = append(inner, pktLine([]byte("unpack error\n"))...)
	}
	for _, c := range cmds {
		status := "ok"
		reason := ""
		if !unpackOK {
			status, reason = "ng", "unpack failed"
		} else {
			cur, exists := repo.refs[c.ref]
			switch {
			case c.new == zeroOID: // delete
				if !exists || cur != c.old {
					status, reason = "ng", "non-fast-forward"
				}
			case c.old == zeroOID: // create
				if exists {
					status, reason = "ng", "non-fast-forward"
				} else if _, ok := repo.objects[c.new]; !ok {
					status, reason = "ng", "missing object"
				}
			default: // update
				if !exists || cur != c.old {
					status, reason = "ng", "non-fast-forward"
				} else if _, ok := repo.objects[c.new]; !ok {
					status, reason = "ng", "missing object"
				} else if !sv.st.isAncestor(repo, c.old, c.new) {
					status, reason = "ng", "non-fast-forward"
				}
			}
		}
		if status == "ok" {
			if c.new == zeroOID {
				delete(repo.refs, c.ref)
			} else {
				repo.refs[c.ref] = c.new
			}
			inner = append(inner, pktLine([]byte("ok "+c.ref+"\n"))...)
		} else {
			inner = append(inner, pktLine([]byte("ng "+c.ref+" "+reason+"\n"))...)
		}
	}
	var out []byte
	out = append(out, sideband(inner)...)
	out = append(out, pktFlush...)
	return &response{status: 200, ctype: "application/x-git-receive-pack-result", body: out}
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			break
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}
