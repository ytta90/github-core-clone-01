package main

import (
	"bytes"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Smart HTTP: info/refs, upload-pack, receive-pack
// ---------------------------------------------------------------------------

// gitRoutes handles /{owner}/{repo}.git/* paths.
func (srv *server) handleGit(w http.ResponseWriter, r *http.Request, owner, repo, rest string) {
	srv.store.mu.Lock()
	defer srv.store.mu.Unlock()
	rep := srv.store.getRepo(owner, repo)
	switch rest {
	case "/info/refs":
		srv.gitInfoRefs(w, r, rep, owner, repo)
	case "/git-upload-pack":
		srv.gitUploadPack(w, r, rep, owner, repo)
	case "/git-receive-pack":
		srv.gitReceivePack(w, r, rep, owner, repo)
	default:
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
	}
}

func isGitService(r *http.Request, service string) bool {
	return r.URL.Query().Get("service") == service
}

// sortedRefs returns the repo's refs sorted by name.
func sortedRefs(repo *Repo) []string {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// gitInfoRefs answers GET info/refs for upload-pack (v2 when requested) and
// receive-pack (v0).
func (srv *server) gitInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repo, owner, name string) {
	service := r.URL.Query().Get("service")
	if repo == nil {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	wantV2 := service == "git-upload-pack" && r.Header.Get("Git-Protocol") == "version=2"
	if wantV2 {
		var buf bytes.Buffer
		buf.Write(encodePktLine([]byte("# service=git-upload-pack\n")))
		buf.Write(pktFlushBytes)
		buf.Write(encodePktLine([]byte("version 2\n")))
		for _, cap := range []string{"ls-refs=unborn", "fetch=shallow filter", "object-format=sha1"} {
			buf.Write(encodePktLine([]byte(cap + "\n")))
		}
		buf.Write(pktFlushBytes)
		srv.writeGitResult(w, r, "application/x-git-upload-pack-advertisement", buf.Bytes())
		return
	}
	// v0 advertisement
	var buf bytes.Buffer
	buf.Write(encodePktLine([]byte("# service=" + service + "\n")))
	buf.Write(pktFlushBytes)
	caps := "report-status-v2 report-status side-band-64k ofs-delta agent=forge/1.0.0"
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		// Empty repo: the capabilities^{} pseudo-ref.
		buf.Write(encodePktLine([]byte(zeroOID + " capabilities^{}\x00" + caps + "\n")))
	} else {
		head := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if head == "" {
			head = repo.Refs[refs[0]]
		}
		buf.Write(encodePktLine([]byte(head + " HEAD\x00" + caps + " symref=HEAD:refs/heads/" + repo.DefaultBranch + "\n")))
		for _, ref := range refs {
			buf.Write(encodePktLine([]byte(repo.Refs[ref] + " " + ref + "\n")))
		}
	}
	buf.Write(pktFlushBytes)
	srv.writeGitResult(w, r, "application/x-"+service+"-advertisement", buf.Bytes())
}

func (srv *server) writeGitResult(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-GitHub-Request-Id", srv.store.nextReqID())
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

func (srv *server) gitUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo, owner, name string) {
	if repo == nil {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		srv.writeErr(w, r, http.StatusBadRequest, "could not read request body")
		return
	}
	if r.Header.Get("Git-Protocol") == "version=2" {
		srv.uploadPackV2(w, r, repo, body)
		return
	}
	srv.uploadPackV0(w, r, repo, body)
}

// parseV2Command splits v2 request pkt-lines into the command and its args.
func parseV2Command(body []byte) (command string, args []string, err error) {
	packets, _ := decodePktLines(body)
	inArgs := false
	for _, p := range packets {
		switch p.kind {
		case pktFlush:
			return command, args, nil
		case pktDelim:
			inArgs = true
		case pktData:
			line := strings.TrimSuffix(string(p.payload), "\n")
			if !inArgs {
				if strings.HasPrefix(line, "command=") {
					command = strings.TrimPrefix(line, "command=")
				}
			} else {
				args = append(args, line)
			}
		}
	}
	return command, args, nil
}

func (srv *server) uploadPackV2(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	command, args, _ := parseV2Command(body)
	switch command {
	case "ls-refs":
		srv.v2LsRefs(w, r, repo, args)
	case "fetch":
		srv.v2Fetch(w, r, repo, args)
	default:
		srv.writeErr(w, r, http.StatusBadRequest, "unknown command")
	}
}

func (srv *server) v2LsRefs(w http.ResponseWriter, r *http.Request, repo *Repo, args []string) {
	var prefixes []string
	peel := true
	_ = peel
	for _, a := range args {
		if strings.HasPrefix(a, "ref-prefix ") {
			prefixes = append(prefixes, strings.TrimPrefix(a, "ref-prefix "))
		}
	}
	keep := func(name string) bool {
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
	var buf bytes.Buffer
	headOID := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if keep("HEAD") {
		if headOID != "" {
			buf.Write(encodePktLine([]byte(headOID + " HEAD symref-target:refs/heads/" + repo.DefaultBranch + "\n")))
		} else {
			buf.Write(encodePktLine([]byte("unborn HEAD symref-target:refs/heads/" + repo.DefaultBranch + "\n")))
		}
	}
	for _, ref := range sortedRefs(repo) {
		if !keep(ref) {
			continue
		}
		buf.Write(encodePktLine([]byte(repo.Refs[ref] + " " + ref + "\n")))
	}
	buf.Write(pktFlushBytes)
	srv.writeGitResult(w, r, "application/x-git-upload-pack-result", buf.Bytes())
}

func (srv *server) v2Fetch(w http.ResponseWriter, r *http.Request, repo *Repo, args []string) {
	var wants, haves, shallow []string
	depth := 0
	var deepenSince int64
	var deepenNot []string
	var filter string
	done := false
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			wants = append(wants, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "have "):
			haves = append(haves, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "shallow "):
			shallow = append(shallow, strings.TrimSpace(a[8:]))
		case strings.HasPrefix(a, "deepen "):
			depth, _ = strconv.Atoi(strings.TrimSpace(a[7:]))
		case strings.HasPrefix(a, "deepen-since "):
			deepenSince, _ = strconv.ParseInt(strings.TrimSpace(a[13:]), 10, 64)
		case strings.HasPrefix(a, "deepen-not "):
			deepenNot = append(deepenNot, strings.TrimSpace(a[11:]))
		case strings.HasPrefix(a, "filter "):
			filter = strings.TrimSpace(a[7:])
		case a == "done":
			done = true
		}
	}
	_ = done
	_ = deepenNot
	pack, shallowList, unshallowList, acks, nak := srv.buildFetchPack(repo, wants, haves, shallow, depth, deepenSince, filter)

	var buf bytes.Buffer
	// acknowledgments
	buf.Write(encodePktLine([]byte("acknowledgments\n")))
	if nak {
		buf.Write(encodePktLine([]byte("NAK\n")))
	} else {
		for _, ack := range acks {
			buf.Write(encodePktLine([]byte("ACK " + ack + "\n")))
		}
		buf.Write(encodePktLine([]byte("ready\n")))
	}
	buf.Write(pktDelimBytes)
	// shallow-info
	if len(shallowList) > 0 || len(unshallowList) > 0 {
		buf.Write(encodePktLine([]byte("shallow-info\n")))
		for _, s := range shallowList {
			buf.Write(encodePktLine([]byte("shallow " + s + "\n")))
		}
		for _, u := range unshallowList {
			buf.Write(encodePktLine([]byte("unshallow " + u + "\n")))
		}
		buf.Write(pktDelimBytes)
	}
	// wanted-refs
	var wantedRefs []string
	for _, ref := range sortedRefs(repo) {
		oid := repo.Refs[ref]
		for _, w := range wants {
			if w == oid {
				wantedRefs = append(wantedRefs, oid+" "+ref+"\n")
				break
			}
		}
	}
	if len(wantedRefs) > 0 {
		buf.Write(encodePktLine([]byte("wanted-refs\n")))
		for _, line := range wantedRefs {
			buf.Write(encodePktLine([]byte(line)))
		}
		buf.Write(pktDelimBytes)
	}
	// packfile (sideband band 1)
	buf.Write(encodePktLine([]byte("packfile\n")))
	for _, chunk := range splitSideband(pack) {
		buf.Write(encodePktLine(chunk))
	}
	buf.Write(pktFlushBytes)
	srv.writeGitResult(w, r, "application/x-git-upload-pack-result", buf.Bytes())
}

// splitSideband splits pack bytes into sideband band-1 pkt-line payloads.
// A pkt-line is at most 65520 bytes total, so each chunk carries at most
// 65515 pack bytes plus the 1 band byte.
func splitSideband(pack []byte) [][]byte {
	const maxPack = 65515
	var out [][]byte
	for len(pack) > 0 {
		n := len(pack)
		if n > maxPack {
			n = maxPack
		}
		chunk := make([]byte, 0, n+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, pack[:n]...)
		out = append(out, chunk)
		pack = pack[n:]
	}
	if len(out) == 0 {
		out = append(out, []byte{1})
	}
	return out
}

// buildFetchPack computes the fetch response pack and negotiation metadata.
// It returns (pack, shallow, unshallow, acks, nak).
func (srv *server) buildFetchPack(repo *Repo, wants, haves, shallow []string, depth int, deepenSince int64, filter string) ([]byte, []string, []string, []string, bool) {
	store := srv.store
	// Deduplicate wants; drop unknown ones.
	seenWant := map[string]bool{}
	var cleanWants []string
	for _, w := range wants {
		if !isObjectID(w) || seenWant[w] {
			continue
		}
		seenWant[w] = true
		if obj := store.objects[w]; obj != nil && obj.Type == "commit" {
			cleanWants = append(cleanWants, w)
		}
	}
	// ACK negotiation.
	var acks []string
	nak := false
	if len(haves) == 0 {
		nak = true
	} else {
		for _, h := range haves {
			if obj := store.objects[h]; obj != nil && obj.Type == "commit" {
				common := false
				for _, w := range cleanWants {
					if store.isAncestor(h, w) {
						common = true
						break
					}
				}
				if common {
					acks = append(acks, h)
				}
			}
		}
		if len(acks) == 0 {
			nak = true
		}
	}
	// Depth / shallow handling.
	var shallowList, unshallowList []string
	var included map[string]bool
	if depth > 0 || deepenSince > 0 {
		if len(shallow) > 0 && depth > 0 {
			// Deepening: new depth = old depth + deepen.
			oldDepth := 0
			for _, s := range shallow {
				if d := store.commitDepth(cleanWants, s); d > oldDepth {
					oldDepth = d
				}
			}
			newDepth := oldDepth + depth
			included, shallowList = store.commitLevels(cleanWants, newDepth)
			for _, s := range shallow {
				if included[s] {
					isBoundary := false
					for _, b := range shallowList {
						if b == s {
							isBoundary = true
						}
					}
					if !isBoundary {
						unshallowList = append(unshallowList, s)
					}
				}
			}
		} else if depth > 0 {
			included, shallowList = store.commitLevels(cleanWants, depth)
		} else {
			// deepen-since: commits newer than the timestamp.
			included = map[string]bool{}
			all, _ := store.commitLevels(cleanWants, 0)
			for oid := range all {
				if deepenSince <= commitEpoch {
					included[oid] = true
				}
			}
			// boundary = roots of the included set
			for oid := range included {
				obj := store.objects[oid]
				hasParent := false
				if obj != nil {
					for _, p := range parseCommit(obj.Data).Parents {
						if included[p] {
							hasParent = true
						}
					}
				}
				if !hasParent {
					shallowList = append(shallowList, oid)
				}
			}
			sort.Strings(shallowList)
		}
	} else if len(shallow) > 0 {
		// Client restates its shallow boundary without deepening: keep it.
		included, _ = store.commitLevels(cleanWants, 0)
		shallowList = append([]string(nil), shallow...)
	} else {
		included, _ = store.commitLevels(cleanWants, 0)
	}
	// Objects to send: reachable from included commits, minus haves' closure.
	haveSet := store.collectReachable(haves, true)
	var objs []*Object
	seenObj := map[string]bool{}
	var addTree func(oid string)
	var addCommit func(oid string)
	addTree = func(oid string) {
		if seenObj[oid] {
			return
		}
		obj := store.objects[oid]
		if obj == nil || obj.Type != "tree" {
			return
		}
		seenObj[oid] = true
		if !haveSet[oid] {
			objs = append(objs, obj)
		}
		for _, e := range parseTree(obj.Data) {
			if e.Mode == "40000" {
				addTree(e.OID)
			} else if !seenObj[e.OID] && !haveSet[e.OID] {
				seenObj[e.OID] = true
				if b := store.objects[e.OID]; b != nil {
					objs = append(objs, b)
				}
			}
		}
	}
	addCommit = func(oid string) {
		if seenObj[oid] {
			return
		}
		obj := store.objects[oid]
		if obj == nil || obj.Type != "commit" {
			return
		}
		seenObj[oid] = true
		if !haveSet[oid] {
			objs = append(objs, obj)
		}
		c := parseCommit(obj.Data)
		addTree(c.Tree)
		// Only walk parents that are themselves included.
		for _, p := range c.Parents {
			if included[p] {
				addCommit(p)
			}
		}
	}
	for oid := range included {
		addCommit(oid)
	}
	// filter blob:none drops blobs.
	if filter == "blob:none" {
		filtered := objs[:0]
		for _, o := range objs {
			if o.Type != "blob" {
				filtered = append(filtered, o)
			}
		}
		objs = filtered
	}
	sort.Slice(objs, func(i, j int) bool {
		oi, oj := hashObject(objs[i].Type, objs[i].Data), hashObject(objs[j].Type, objs[j].Data)
		return oi < oj
	})
	pack := writePack(objs)
	return pack, shallowList, unshallowList, acks, nak
}

// uploadPackV0 answers a v0 fetch.
func (srv *server) uploadPackV0(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	packets, _ := decodePktLines(body)
	var wants, haves, shallow []string
	depth := 0
	var deepenSince int64
	section := 0
	for _, p := range packets {
		if p.kind == pktFlush {
			section++
			continue
		}
		if p.kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		// The first want line carries capabilities after a space: strip them.
		if strings.HasPrefix(line, "want ") {
			rest := strings.TrimSpace(line[5:])
			if i := strings.Index(rest, " "); i >= 0 {
				rest = rest[:i]
			}
			wants = append(wants, rest)
		} else if strings.HasPrefix(line, "have ") {
			haves = append(haves, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, "shallow ") {
			shallow = append(shallow, strings.TrimSpace(line[8:]))
		} else if strings.HasPrefix(line, "deepen ") {
			depth, _ = strconv.Atoi(strings.TrimSpace(line[7:]))
		} else if strings.HasPrefix(line, "deepen-since ") {
			deepenSince, _ = strconv.ParseInt(strings.TrimSpace(line[13:]), 10, 64)
		}
	}
	_ = section
	pack, shallowList, unshallowList, acks, nak := srv.buildFetchPack(repo, wants, haves, shallow, depth, deepenSince, "")
	var buf bytes.Buffer
	if nak {
		buf.Write(encodePktLine([]byte("NAK\n")))
	} else {
		for _, ack := range acks {
			buf.Write(encodePktLine([]byte("ACK " + ack + "\n")))
		}
	}
	for _, s := range shallowList {
		buf.Write(encodePktLine([]byte("shallow " + s + "\n")))
	}
	for _, u := range unshallowList {
		buf.Write(encodePktLine([]byte("unshallow " + u + "\n")))
	}
	for _, chunk := range splitSideband(pack) {
		buf.Write(encodePktLine(chunk))
	}
	buf.Write(pktFlushBytes)
	srv.writeGitResult(w, r, "application/x-git-upload-pack-result", buf.Bytes())
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func (srv *server) gitReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo, owner, name string) {
	if repo == nil {
		srv.writeErr(w, r, http.StatusNotFound, "Not Found")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		srv.writeErr(w, r, http.StatusBadRequest, "could not read request body")
		return
	}
	// Parse command pkt-lines until the flush; the remainder is the pack.
	packets, remainder := decodePktLines(body)
	var commands []pushCommand
	sawFlush := false
	for _, p := range packets {
		if p.kind == pktFlush {
			sawFlush = true
			break
		}
		if p.kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if i := strings.Index(line, "\x00"); i >= 0 {
			line = line[:i]
		}
		parts := strings.Fields(line)
		if len(parts) != 3 {
			continue
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}
	if !sawFlush || len(commands) == 0 {
		srv.writeErr(w, r, http.StatusBadRequest, "bad receive-pack request")
		return
	}
	// Unpack.
	unpackOK := true
	unpackDetail := "ok"
	if len(remainder) > 0 {
		objects, err := readPack(remainder, nil)
		if err != nil {
			unpackOK = false
			unpackDetail = "unpacker error"
		} else {
			for oid, obj := range objects {
				srv.store.objects[oid] = obj
			}
		}
	}
	// Apply commands.
	type refResult struct {
		ref    string
		ok     bool
		reason string
	}
	results := make([]refResult, 0, len(commands))
	for _, cmd := range commands {
		if !unpackOK {
			results = append(results, refResult{ref: cmd.ref, ok: false, reason: "unpacker error"})
			continue
		}
		current, exists := repo.Refs[cmd.ref]
		switch {
		case cmd.newOID == zeroOID:
			// delete
			if !exists {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "no such ref"})
			} else if current != cmd.oldOID {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			} else {
				delete(repo.Refs, cmd.ref)
				results = append(results, refResult{ref: cmd.ref, ok: true})
			}
		case cmd.oldOID == zeroOID:
			// create
			if exists {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "already exists"})
			} else if srv.store.objects[cmd.newOID] == nil {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "invalid object"})
			} else {
				repo.Refs[cmd.ref] = cmd.newOID
				results = append(results, refResult{ref: cmd.ref, ok: true})
			}
		default:
			// update
			if !exists {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "no such ref"})
			} else if current != cmd.oldOID {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			} else if srv.store.objects[cmd.newOID] == nil {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "invalid object"})
			} else if cmd.newOID == cmd.oldOID || srv.store.isAncestor(cmd.oldOID, cmd.newOID) {
				repo.Refs[cmd.ref] = cmd.newOID
				results = append(results, refResult{ref: cmd.ref, ok: true})
			} else {
				results = append(results, refResult{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
			}
		}
	}
	// Report (report-status inside sideband band 1).
	var report bytes.Buffer
	if unpackOK {
		report.WriteString("unpack ok\n")
	} else {
		report.WriteString("unpack " + unpackDetail + "\n")
	}
	for _, res := range results {
		if res.ok {
			report.WriteString("ok " + res.ref + "\n")
		} else {
			report.WriteString("ng " + res.ref + " " + res.reason + "\n")
		}
	}
	inner := encodePktLines(splitReportLines(report.Bytes()))
	inner = append(inner, pktFlushBytes...)
	var buf bytes.Buffer
	for _, chunk := range splitSideband(inner) {
		buf.Write(encodePktLine(chunk))
	}
	buf.Write(pktFlushBytes)
	srv.writeGitResult(w, r, "application/x-git-receive-pack-result", buf.Bytes())
}

// splitReportLines splits report bytes into lines (each kept whole) for
// pkt-line framing.
func splitReportLines(report []byte) [][]byte {
	var out [][]byte
	for len(report) > 0 {
		i := bytes.IndexByte(report, '\n')
		if i < 0 {
			out = append(out, report)
			break
		}
		out = append(out, report[:i+1])
		report = report[i+1:]
	}
	return out
}
