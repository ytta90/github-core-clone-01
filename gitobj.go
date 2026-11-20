package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Object ids and serialization
// ---------------------------------------------------------------------------

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func isObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

const zeroOID = "0000000000000000000000000000000000000000"

// ---------------------------------------------------------------------------
// Trees and commits
// ---------------------------------------------------------------------------

type TreeEntry struct {
	Mode string // "100644" | "40000"
	Name string
	OID  string
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		space := bytes.IndexByte(data[i:], ' ')
		if space < 0 {
			break
		}
		mode := string(data[i : i+space])
		i += space + 1
		nul := bytes.IndexByte(data[i:], 0)
		if nul < 0 || i+nul+20 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		i += nul + 1
		oid := hex.EncodeToString(data[i : i+20])
		i += 20
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
	}
	return out
}

// buildTree serializes entries in git's order (names, directories compared
// as if they had a trailing slash).
func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			a += "/"
		}
		if sorted[j].Mode == "40000" {
			b += "/"
		}
		return a < b
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		oid, err := hex.DecodeString(e.OID)
		if err != nil || len(oid) != 20 {
			continue
		}
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		buf.Write(oid)
	}
	return buf.Bytes()
}

type parsedCommit struct {
	Tree    string
	Parents []string
}

func parseCommit(data []byte) parsedCommit {
	var out parsedCommit
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "tree ") {
			out.Tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			out.Parents = append(out.Parents, strings.TrimSpace(line[7:]))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// pkt-line framing
// ---------------------------------------------------------------------------

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktResponseEnd
)

type packet struct {
	kind    pktKind
	payload []byte
}

func encodePktLine(payload []byte) []byte {
	total := len(payload) + 4
	out := make([]byte, 4, total)
	copy(out, fmt.Sprintf("%04x", total))
	return append(out, payload...)
}

func encodePktLines(lines [][]byte) []byte {
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(encodePktLine(l))
	}
	return buf.Bytes()
}

// decodePktLines parses a whole buffer into packets. The trailing bytes
// after a flush that are not pkt-framed are returned as remainder.
func decodePktLines(data []byte) (packets []packet, remainder []byte) {
	i := 0
	for i+4 <= len(data) {
		n, err := strconv.ParseInt(string(data[i:i+4]), 16, 32)
		if err != nil {
			return packets, data[i:]
		}
		if n == 0 {
			packets = append(packets, packet{kind: pktFlush})
			i += 4
			continue
		}
		if n == 1 {
			packets = append(packets, packet{kind: pktDelim})
			i += 4
			continue
		}
		if n == 2 {
			packets = append(packets, packet{kind: pktResponseEnd})
			i += 4
			continue
		}
		if i+int(n) > len(data) {
			return packets, data[i:]
		}
		packets = append(packets, packet{kind: pktData, payload: data[i+4 : i+int(n)]})
		i += int(n)
	}
	return packets, data[i:]
}

var (
	pktFlushBytes = []byte("0000")
	pktDelimBytes = []byte("0001")
)

// ---------------------------------------------------------------------------
// Packfiles
// ---------------------------------------------------------------------------

var errPack = errors.New("invalid packfile")

// readPack parses a packfile (v2/v3), verifies each object's SHA-1 and size,
// and resolves OFS_DELTA / REF_DELTA entries. resolveBase supplies objects
// a thin pack leaves out.
func readPack(pack []byte, resolveBase func(oid string) *Object) (map[string]*Object, error) {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("%w: bad header", errPack)
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("%w: unsupported version %d", errPack, version)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	if trailerStart < 12 {
		return nil, fmt.Errorf("%w: too short", errPack)
	}
	// Verify the trailer checksum.
	sum := sha1.Sum(pack[:trailerStart])
	if !bytes.Equal(sum[:], pack[trailerStart:]) {
		return nil, fmt.Errorf("%w: trailer checksum mismatch", errPack)
	}

	type rawEntry struct {
		offset   int
		typeCode int
		size     int
		data     []byte
		baseOff  int
		baseOID  string
	}
	raw := make([]*rawEntry, 0, count)
	byOffset := make(map[int]*rawEntry)
	offset := 12
	for idx := 0; idx < count; idx++ {
		entryOffset := offset
		// varint type/size
		first := pack[offset]
		offset++
		typeCode := int((first >> 4) & 0x07)
		size := int(first & 0x0f)
		shift := 4
		for first&0x80 != 0 {
			if offset >= trailerStart {
				return nil, fmt.Errorf("%w: truncated size varint", errPack)
			}
			b := pack[offset]
			offset++
			size |= int(b&0x7f) << shift
			shift += 7
			first = b
		}
		entry := &rawEntry{offset: entryOffset, typeCode: typeCode, size: size}
		if typeCode == 6 {
			// OFS_DELTA base offset
			b := pack[offset]
			offset++
			baseOff := int(b & 0x7f)
			for b&0x80 != 0 {
				if offset >= trailerStart {
					return nil, fmt.Errorf("%w: truncated ofs-delta", errPack)
				}
				b = pack[offset]
				offset++
				baseOff = ((baseOff + 1) << 7) | int(b&0x7f)
			}
			entry.baseOff = entryOffset - baseOff
		} else if typeCode == 7 {
			if offset+20 > trailerStart {
				return nil, fmt.Errorf("%w: truncated ref-delta", errPack)
			}
			entry.baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if typeCode < 1 || typeCode > 4 {
			return nil, fmt.Errorf("%w: unknown type code %d", errPack, typeCode)
		}
		inflated, next, err := inflateAt(pack[:trailerStart], offset)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errPack, err)
		}
		if len(inflated) != size {
			return nil, fmt.Errorf("%w: entry at %d declares %d bytes, inflated to %d", errPack, entryOffset, size, len(inflated))
		}
		entry.data = inflated
		offset = next
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("%w: %d stray bytes before trailer", errPack, trailerStart-offset)
	}

	typeCodeToType := map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}
	objects := make(map[string]*Object)
	resolvedByOID := make(map[string]*rawEntry)
	resolved := make(map[int]*Object)
	resolving := make(map[int]bool)
	var resolve func(e *rawEntry) (*Object, error)
	resolve = func(e *rawEntry) (*Object, error) {
		if obj, ok := resolved[e.offset]; ok {
			return obj, nil
		}
		if resolving[e.offset] {
			return nil, fmt.Errorf("%w: delta cycle", errPack)
		}
		resolving[e.offset] = true
		defer delete(resolving, e.offset)
		var typ string
		var data []byte
		switch e.typeCode {
		case 6:
			base, ok := byOffset[e.baseOff]
			if !ok {
				return nil, fmt.Errorf("%w: ofs-delta base missing", errPack)
			}
			baseObj, err := resolve(base)
			if err != nil {
				return nil, err
			}
			typ = baseObj.Type
			data, err = applyDelta(baseObj.Data, e.data)
			if err != nil {
				return nil, err
			}
		case 7:
			if baseEntry, ok := resolvedByOID[e.baseOID]; ok {
				baseObj, err := resolve(baseEntry)
				if err != nil {
					return nil, err
				}
				typ = baseObj.Type
				data, err = applyDelta(baseObj.Data, e.data)
				if err != nil {
					return nil, err
				}
			} else if resolveBase != nil {
				ext := resolveBase(e.baseOID)
				if ext == nil {
					return nil, fmt.Errorf("%w: ref-delta base %s not found", errPack, e.baseOID)
				}
				var err error
				typ = ext.Type
				data, err = applyDelta(ext.Data, e.data)
				if err != nil {
					return nil, err
				}
			} else {
				return nil, fmt.Errorf("%w: ref-delta base %s not found", errPack, e.baseOID)
			}
		default:
			typ = typeCodeToType[e.typeCode]
			data = e.data
		}
		oid := hashObject(typ, data)
		obj := &Object{Type: typ, Data: data}
		resolved[e.offset] = obj
		resolvedByOID[oid] = e
		objects[oid] = obj
		return obj, nil
	}
	for _, e := range raw {
		if _, err := resolve(e); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

func inflateAt(data []byte, offset int) ([]byte, int, error) {
	// bytes.Reader is an io.ByteReader, so the zlib reader consumes it one
	// byte at a time with no read-ahead: Len() afterwards gives the exact
	// compressed length.
	br := bytes.NewReader(data[offset:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	out, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed := len(data[offset:]) - br.Len()
	return out, offset + consumed, nil
}

// applyDelta applies a git delta to a base object.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		v := 0
		shift := 0
		for {
			if pos >= len(delta) {
				return 0, fmt.Errorf("%w: truncated delta varint", errPack)
			}
			b := delta[pos]
			pos++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("%w: delta base size %d, got %d", errPack, srcSize, len(base))
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out []byte
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// copy from base
			off, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("%w: truncated delta copy", errPack)
					}
					off |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("%w: truncated delta copy", errPack)
					}
					size |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, fmt.Errorf("%w: delta copy out of range", errPack)
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, fmt.Errorf("%w: truncated delta insert", errPack)
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, fmt.Errorf("%w: delta opcode 0", errPack)
		}
	}
	if len(out) != dstSize {
		return nil, fmt.Errorf("%w: delta result %d bytes, expected %d", errPack, len(out), dstSize)
	}
	return out, nil
}

// writePack builds a packfile from plain objects (no deltas), in the
// caller's order. Trailer is SHA-1 over everything before it.
func writePack(objects []*Object) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objects)))
	buf.Write(header)
	typeCodes := map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
	for _, obj := range objects {
		code := typeCodes[obj.Type]
		size := len(obj.Data)
		// type/size varint
		first := byte((code << 4) | (size & 0x0f))
		size >>= 4
		if size > 0 {
			buf.WriteByte(first | 0x80)
			for {
				b := byte(size & 0x7f)
				size >>= 7
				if size > 0 {
					buf.WriteByte(b | 0x80)
				} else {
					buf.WriteByte(b)
					break
				}
			}
		} else {
			buf.WriteByte(first)
		}
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		_, _ = zw.Write(obj.Data)
		_ = zw.Close()
		buf.Write(zb.Bytes())
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

// packChecksum verifies a pack's trailer.
func packChecksumOK(pack []byte) bool {
	if len(pack) < 32 {
		return false
	}
	sum := sha1.Sum(pack[:len(pack)-20])
	return bytes.Equal(sum[:], pack[len(pack)-20:])
}

// ---------------------------------------------------------------------------
// Reachability
// ---------------------------------------------------------------------------

// collectReachable returns every object reachable from tips (commits walk
// parents + trees; trees recurse; tags dereference). Missing objects are
// skipped when allowMissing is set.
func (s *Store) collectReachable(tips []string, allowMissing bool) map[string]bool {
	seen := make(map[string]bool)
	var walkCommit func(oid string)
	var walkTree func(oid string)
	walkTree = func(oid string) {
		if seen[oid] {
			return
		}
		obj := s.objects[oid]
		if obj == nil {
			if allowMissing {
				return
			}
			return
		}
		if obj.Type != "tree" {
			return
		}
		seen[oid] = true
		for _, e := range parseTree(obj.Data) {
			if e.Mode == "40000" {
				walkTree(e.OID)
			} else {
				seen[e.OID] = true
			}
		}
	}
	walkCommit = func(oid string) {
		if seen[oid] {
			return
		}
		obj := s.objects[oid]
		if obj == nil {
			return
		}
		if obj.Type == "tag" {
			// Tag objects point at another object on their first line.
			seen[oid] = true
			for _, line := range strings.Split(string(obj.Data), "\n") {
				if strings.HasPrefix(line, "object ") {
					target := strings.TrimSpace(line[7:])
					if t := s.objects[target]; t != nil {
						if t.Type == "commit" {
							walkCommit(target)
						} else if t.Type == "tree" {
							walkTree(target)
						} else {
							seen[target] = true
						}
					}
				}
				break
			}
			return
		}
		if obj.Type != "commit" {
			return
		}
		seen[oid] = true
		c := parseCommit(obj.Data)
		walkTree(c.Tree)
		for _, p := range c.Parents {
			walkCommit(p)
		}
	}
	for _, tip := range tips {
		walkCommit(tip)
	}
	return seen
}

// objectsToSend returns the objects reachable from newTips that are not
// reachable from serverTips, in deterministic (sorted) order.
func (s *Store) objectsToSend(serverTips, newTips []string) []*Object {
	onServer := s.collectReachable(serverTips, true)
	wanted := s.collectReachable(newTips, false)
	var oids []string
	for oid := range wanted {
		if !onServer[oid] {
			oids = append(oids, oid)
		}
	}
	sort.Strings(oids)
	out := make([]*Object, 0, len(oids))
	for _, oid := range oids {
		if obj := s.objects[oid]; obj != nil {
			out = append(out, obj)
		}
	}
	return out
}

// commitLevels walks from tips level by level. It returns (included, boundary)
// where included holds commits up to maxDepth levels (maxDepth<=0 means all)
// and boundary holds the commits at the deepest included level.
func (s *Store) commitLevels(tips []string, maxDepth int) (included map[string]bool, boundary []string) {
	included = make(map[string]bool)
	if maxDepth <= 0 {
		// unlimited: include everything reachable
		for oid := range s.collectReachable(tips, false) {
			if obj := s.objects[oid]; obj != nil && obj.Type == "commit" {
				included[oid] = true
			}
		}
		return included, nil
	}
	level := make(map[string]int)
	queue := []string{}
	for _, t := range tips {
		if !included[t] {
			if obj := s.objects[t]; obj != nil && obj.Type == "commit" {
				included[t] = true
				level[t] = 0
				queue = append(queue, t)
			}
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		lvl := level[cur]
		if lvl >= maxDepth-1 {
			continue
		}
		obj := s.objects[cur]
		if obj == nil {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if included[p] {
				continue
			}
			if s.objects[p] == nil {
				continue
			}
			included[p] = true
			level[p] = lvl + 1
			queue = append(queue, p)
		}
	}
	for oid := range included {
		if level[oid] == maxDepth-1 {
			boundary = append(boundary, oid)
		}
	}
	sort.Strings(boundary)
	return included, boundary
}

// commitDepth returns the distance from any tip to oid (BFS), or -1.
func (s *Store) commitDepth(tips []string, oid string) int {
	seen := map[string]int{}
	queue := []string{}
	for _, t := range tips {
		if _, ok := seen[t]; !ok {
			seen[t] = 0
			queue = append(queue, t)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == oid {
			return seen[cur]
		}
		obj := s.objects[cur]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if _, ok := seen[p]; !ok {
				seen[p] = seen[cur] + 1
				queue = append(queue, p)
			}
		}
	}
	return -1
}

// isAncestor reports whether anc is oid itself or an ancestor of oid.
func (s *Store) isAncestor(anc, oid string) bool {
	if anc == oid {
		return true
	}
	seen := map[string]bool{}
	queue := []string{oid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := s.objects[cur]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if p == anc {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Tree diff
// ---------------------------------------------------------------------------

type FileChange struct {
	Path   string
	Status string // "added" | "modified" | "removed"
	OldOID string
	NewOID string
}

func (s *Store) diffTrees(baseTree, headTree string) []FileChange {
	base := s.flattenTree(baseTree)
	head := s.flattenTree(headTree)
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var out []FileChange
	for p := range paths {
		b, inBase := base[p]
		h, inHead := head[p]
		switch {
		case inBase && !inHead:
			out = append(out, FileChange{Path: p, Status: "removed", OldOID: b})
		case !inBase && inHead:
			out = append(out, FileChange{Path: p, Status: "added", NewOID: h})
		case b != h:
			out = append(out, FileChange{Path: p, Status: "modified", OldOID: b, NewOID: h})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// lineDiff counts added/removed lines between two blobs (LCS on lines).
func lineDiff(old, new []byte) (add, del int) {
	oldLines := splitLines(old)
	newLines := splitLines(new)
	n, m := len(oldLines), len(newLines)
	if n == 0 {
		return m, 0
	}
	if m == 0 {
		return 0, n
	}
	// LCS with two rows.
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if oldLines[i-1] == newLines[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	lcs := prev[m]
	return m - lcs, n - lcs
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
