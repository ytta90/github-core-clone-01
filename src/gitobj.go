package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strconv"
)

const zeroOID = "0000000000000000000000000000000000000000"

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func oidBytes(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

// ---- object model ----

type gitObject struct {
	typ  string // commit, tree, blob, tag
	data []byte
}

type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[off : off+sp])
		nul := bytes.IndexByte(data[off+sp+1:], 0)
		if nul < 0 || off+sp+1+nul+21 > len(data) {
			break
		}
		name := string(data[off+sp+1 : off+sp+1+nul])
		oid := hex.EncodeToString(data[off+sp+1+nul+1 : off+sp+1+nul+21])
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
		off = off + sp + 1 + nul + 21
	}
	return out
}

func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		a := sorted[i].name
		if sorted[i].mode == "40000" {
			a += "/"
		}
		b := sorted[j].name
		if sorted[j].mode == "40000" {
			b += "/"
		}
		return a < b
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		buf.Write(oidBytes(e.oid))
	}
	return buf.Bytes()
}

type commitView struct {
	tree    string
	parents []string
	author  string
	commit  string
	message string
}

func parseCommit(data []byte) commitView {
	var cv commitView
	blank := bytes.Index(data, []byte("\n\n"))
	head := data
	if blank >= 0 {
		head = data[:blank]
		cv.message = string(data[blank+2:])
	}
	for len(head) > 0 {
		nl := bytes.IndexByte(head, '\n')
		var line []byte
		if nl < 0 {
			line, head = head, nil
		} else {
			line, head = head[:nl], head[nl+1:]
		}
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		k, v := string(line[:sp]), string(line[sp+1:])
		switch k {
		case "tree":
			cv.tree = v
		case "parent":
			cv.parents = append(cv.parents, v)
		case "author":
			cv.author = v
		case "committer":
			cv.commit = v
		}
	}
	return cv
}

type tagView struct {
	object string
	typ    string
	tag    string
	tagger string
}

func parseTag(data []byte) tagView {
	var tv tagView
	blank := bytes.Index(data, []byte("\n\n"))
	head := data
	if blank >= 0 {
		head = data[:blank]
	}
	for len(head) > 0 {
		nl := bytes.IndexByte(head, '\n')
		var line []byte
		if nl < 0 {
			line, head = head, nil
		} else {
			line, head = head[:nl], head[nl+1:]
		}
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		k, v := string(line[:sp]), string(line[sp+1:])
		switch k {
		case "object":
			tv.object = v
		case "type":
			tv.typ = v
		case "tag":
			tv.tag = v
		case "tagger":
			tv.tagger = v
		}
	}
	return tv
}

// ---- pkt-line ----

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, hex4(n)...)
	out = append(out, payload...)
	return out
}

func hex4(n int) []byte {
	const hexd = "0123456789abcdef"
	return []byte{hexd[(n>>12)&0xf], hexd[(n>>8)&0xf], hexd[(n>>4)&0xf], hexd[n&0xf]}
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktDecode decodes a whole buffer into packets. Each packet is (kind, payload):
// kind 0 = data, 1 = flush, 2 = delim, 3 = response-end. Returns error on malformed input.
func pktDecode(b []byte) ([]pkt, error) {
	var out []pkt
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, errors.New("truncated pkt-line")
		}
		if !isHex4(b[:4]) {
			return nil, errors.New("invalid pkt-line length")
		}
		n, _ := strconv.ParseInt(string(b[:4]), 16, 32)
		if n == 0 {
			out = append(out, pkt{kind: 1})
			b = b[4:]
			continue
		}
		if n == 1 {
			out = append(out, pkt{kind: 2})
			b = b[4:]
			continue
		}
		if n == 2 {
			out = append(out, pkt{kind: 3})
			b = b[4:]
			continue
		}
		if n == 3 || n > 65520 || int(n) > len(b) {
			return nil, errors.New("bad pkt-line length")
		}
		out = append(out, pkt{kind: 0, payload: b[4:n]})
		b = b[n:]
	}
	return out, nil
}

func isHex4(b []byte) bool {
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

type pkt struct {
	kind    int // 0 data, 1 flush, 2 delim, 3 response-end
	payload []byte
}

// ---- packfile ----

var errPack = errors.New("bad pack")

// readPack parses a packfile (version 2/3, plain objects only; deltas resolve
// against objects already in the pack or the store lookup). Returns the objects
// keyed by oid. Thin-pack bases come from lookup.
func readPack(pack []byte, lookup func(oid string) *gitObject) (map[string]*gitObject, error) {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, errPack
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 && ver != 3 {
		return nil, errPack
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	trailer := len(pack) - 20
	// verify trailer
	h := sha1.Sum(pack[:trailer])
	if !bytes.Equal(h[:], pack[trailer:]) {
		return nil, errPack
	}
	type rawEntry struct {
		offset  int
		typ     string
		size    int
		data    []byte
		baseOff int
		baseOID string
		isDelta bool
		deltaOf int // offset of base, or -1 for ref-delta
	}
	raw := make([]rawEntry, 0, count)
	byOffset := map[int]int{}
	off := 12
	for i := 0; i < count; i++ {
		if off >= trailer {
			return nil, errPack
		}
		eoff := off
		// varint type+size
		b := pack[off]
		off++
		typCode := (b >> 4) & 7
		size := int(b & 0x0f)
		shift := 4
		for b&0x80 != 0 {
			if off >= trailer {
				return nil, errPack
			}
			b = pack[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		e := rawEntry{offset: eoff, size: size, deltaOf: -1}
		switch typCode {
		case 1:
			e.typ = "commit"
		case 2:
			e.typ = "tree"
		case 3:
			e.typ = "blob"
		case 4:
			e.typ = "tag"
		case 6: // ofs-delta
			e.isDelta = true
			// ofs base
			b = pack[off]
			off++
			v := int(b & 0x7f)
			for b&0x80 != 0 {
				if off >= trailer {
					return nil, errPack
				}
				b = pack[off]
				off++
				v = ((v + 1) << 7) | int(b&0x7f)
			}
			e.deltaOf = eoff - v
		case 7: // ref-delta
			e.isDelta = true
			if off+20 > trailer {
				return nil, errPack
			}
			e.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		default:
			return nil, errPack
		}
		// inflate (counting reader measures consumed bytes in one pass)
		cr := &countReader{b: pack[off:trailer]}
		zr, err := zlib.NewReader(cr)
		if err != nil {
			return nil, errPack
		}
		data, err := io.ReadAll(zr)
		zr.Close()
		if err != nil || len(data) != size {
			return nil, errPack
		}
		consumed := cr.n
		if consumed <= 0 {
			return nil, errPack
		}
		e.data = data
		off += consumed
		byOffset[eoff] = len(raw)
		raw = append(raw, e)
	}
	if off != trailer {
		return nil, errPack
	}
	// resolve
	out := make(map[string]*gitObject, count)
	resolved := make([]*gitObject, len(raw))
	var resolve func(i int) (*gitObject, error)
	resolve = func(i int) (*gitObject, error) {
		if resolved[i] != nil {
			return resolved[i], nil
		}
		e := raw[i]
		var obj *gitObject
		if !e.isDelta {
			obj = &gitObject{typ: e.typ, data: e.data}
		} else {
			var base *gitObject
			if e.deltaOf >= 0 {
				bi, ok := byOffset[e.deltaOf]
				if !ok {
					return nil, errPack
				}
				var err error
				base, err = resolve(bi)
				if err != nil {
					return nil, err
				}
			} else {
				if o := out[e.baseOID]; o != nil {
					base = o
				} else if lookup != nil {
					base = lookup(e.baseOID)
				}
				if base == nil {
					return nil, errPack
				}
			}
			data, err := applyDelta(base.data, e.data)
			if err != nil {
				return nil, err
			}
			obj = &gitObject{typ: base.typ, data: data}
		}
		resolved[i] = obj
		out[hashObject(obj.typ, obj.data)] = obj
		return obj, nil
	}
	for i := range raw {
		if _, err := resolve(i); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type countReader struct {
	b []byte
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.b)
	c.b = c.b[n:]
	c.n += n
	return n, nil
}

// ReadByte makes flate read byte-by-byte with no read-ahead, so n is the
// exact number of bytes the zlib stream consumed.
func (c *countReader) ReadByte() (byte, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	b := c.b[0]
	c.b = c.b[1:]
	c.n++
	return b, nil
}

// applyDelta applies a git delta to a base object.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readVarint := func() (int, bool) {
		v, shift := 0, uint(0)
		for {
			if off >= len(delta) {
				return 0, false
			}
			c := delta[off]
			off++
			v |= int(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				return v, true
			}
			if shift > 63 {
				return 0, false
			}
		}
	}
	srcSize, ok := readVarint()
	if !ok || srcSize != len(base) {
		return nil, errPack
	}
	tgtSize, ok := readVarint()
	if !ok {
		return nil, errPack
	}
	out := make([]byte, 0, tgtSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			cpOff, cpSize := 0, 0
			for bit := uint(0); bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errPack
					}
					cpOff |= int(delta[off]) << (bit * 8)
					off++
				}
			}
			for bit := uint(4); bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errPack
					}
					cpSize |= int(delta[off]) << ((bit - 4) * 8)
					off++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > len(base) || len(out)+cpSize > tgtSize {
				return nil, errPack
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if op != 0 {
			n := int(op & 0x7f)
			if off+n > len(delta) || len(out)+n > tgtSize {
				return nil, errPack
			}
			out = append(out, delta[off:off+n]...)
			off += n
		} else {
			return nil, errPack
		}
	}
	if len(out) != tgtSize {
		return nil, errPack
	}
	return out, nil
}

var typeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

// writePack builds a version-2 pack of plain (non-delta) objects.
func writePack(objs []*gitObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var h [8]byte
	binary.BigEndian.PutUint32(h[0:4], 2)
	binary.BigEndian.PutUint32(h[4:8], uint32(len(objs)))
	buf.Write(h[:])
	for _, o := range objs {
		// header varint
		size := len(o.data)
		code := typeCode[o.typ]
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
		zw, _ := zlib.NewWriterLevel(&buf, 1)
		zw.Write(o.data)
		zw.Close()
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// verifyPackChecksum checks a pack trailer.
func verifyPackChecksum(pack []byte) bool {
	if len(pack) < 20 {
		return false
	}
	sum := sha1.Sum(pack[:len(pack)-20])
	return bytes.Equal(sum[:], pack[len(pack)-20:])
}
