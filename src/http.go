package main

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// Raw-syscall HTTP/1.1 server. One goroutine per connection, keep-alive,
// Content-Length bodies (plus a chunked fallback). No net/http, no bufio.

type conn struct {
	fd  int
	buf []byte // read buffer
	pos int    // consumed offset
	end int    // filled offset
}

var connBufPool = sync.Pool{New: func() any { return make([]byte, 65536) }}

func newConn(fd int) *conn {
	return &conn{fd: fd, buf: connBufPool.Get().([]byte)}
}

func (c *conn) close() {
	syscall.Close(c.fd)
	connBufPool.Put(c.buf)
}

// fill reads more data into the buffer. Returns false on EOF/error.
func (c *conn) fill() bool {
	if c.pos > 0 {
		copy(c.buf, c.buf[c.pos:c.end])
		c.end -= c.pos
		c.pos = 0
	}
	if c.end == len(c.buf) {
		// grow
		nb := make([]byte, len(c.buf)*2)
		copy(nb, c.buf[:c.end])
		connBufPool.Put(c.buf)
		c.buf = nb
	}
	n, err := syscall.Read(c.fd, c.buf[c.end:])
	if n <= 0 || err != nil {
		return false
	}
	c.end += n
	return true
}

// readUntil returns the slice up to and including delim, or nil on EOF.
func (c *conn) readUntil(delim []byte) []byte {
	for {
		if idx := indexBytes(c.buf[c.pos:c.end], delim); idx >= 0 {
			out := c.buf[c.pos : c.pos+idx+len(delim)]
			c.pos += idx + len(delim)
			return out
		}
		if !c.fill() {
			return nil
		}
	}
}

// readExact returns exactly n bytes (may span fills), or nil on EOF.
func (c *conn) readExact(n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		if c.pos < c.end {
			take := n - len(out)
			if avail := c.end - c.pos; avail < take {
				take = avail
			}
			out = append(out, c.buf[c.pos:c.pos+take]...)
			c.pos += take
			continue
		}
		if !c.fill() {
			return nil
		}
	}
	return out
}

func indexBytes(b, sub []byte) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		ok := true
		for j := range sub {
			if b[i+j] != sub[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

func writeAll(fd int, b []byte) bool {
	for len(b) > 0 {
		n, err := syscall.Write(fd, b)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return false
		}
		if n <= 0 {
			return false
		}
		b = b[n:]
	}
	return true
}

type reqHeaders struct {
	authorization    string
	contentLength    string
	transferEncoding string
	connection       string
	host             string
	gitProtocol      string
	contentType      string
}

type request struct {
	method  string
	path    string // decoded path (no query)
	rawPath string
	query   string
	version string // "1.1" etc
	hdr     reqHeaders
	body    []byte
	// parsed segments of the path
	segs []string
	// request id for X-GitHub-Request-Id
	reqID uint64
}

var reqCounter uint64

// parseRequest reads one request from the connection. Returns nil on EOF/error.
func (c *conn) parseRequest() *request {
	head := c.readUntil([]byte("\r\n\r\n"))
	if head == nil {
		return nil
	}
	r := &request{reqID: atomic.AddUint64(&reqCounter, 1)}
	// request line
	eol := indexBytes(head, []byte("\r\n"))
	line := string(head[:eol])
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil
	}
	r.method = parts[0]
	r.rawPath = parts[1]
	r.version = strings.TrimPrefix(parts[2], "HTTP/")
	// split query
	if q := strings.IndexByte(r.rawPath, '?'); q >= 0 {
		r.query = r.rawPath[q+1:]
		r.rawPath = r.rawPath[:q]
	}
	r.path = urlDecode(r.rawPath)
	// headers (only the fields the forge reads)
	rest := head[eol+2 : len(head)-4]
	for len(rest) > 0 {
		nl := indexBytes(rest, []byte("\r\n"))
		var hline []byte
		if nl < 0 {
			hline, rest = rest, nil
		} else {
			hline, rest = rest[:nl], rest[nl+2:]
		}
		colon := indexBytes(hline, []byte(":"))
		if colon < 0 {
			continue
		}
		key := hline[:colon]
		v := strings.TrimSpace(string(hline[colon+1:]))
		// match lowercased header name without allocating
		switch {
		case headerEq(key, "authorization"):
			r.hdr.authorization = v
		case headerEq(key, "content-length"):
			r.hdr.contentLength = v
		case headerEq(key, "transfer-encoding"):
			r.hdr.transferEncoding = v
		case headerEq(key, "connection"):
			r.hdr.connection = v
		case headerEq(key, "host"):
			r.hdr.host = v
		case headerEq(key, "git-protocol"):
			r.hdr.gitProtocol = v
		case headerEq(key, "content-type"):
			r.hdr.contentType = v
		}
	}
	// body
	if te := r.hdr.transferEncoding; strings.Contains(te, "chunked") {
		r.body = c.readChunked()
		if r.body == nil {
			r.body = []byte{}
		}
	} else if cl := r.hdr.contentLength; cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 {
			return nil
		}
		if n > 64<<20 {
			return nil
		}
		r.body = c.readExact(n)
		if r.body == nil {
			return nil
		}
	}
	// segments
	if r.path != "" && r.path != "/" {
		p := strings.TrimPrefix(r.path, "/")
		r.segs = strings.Split(p, "/")
	}
	return r
}

func (c *conn) readChunked() []byte {
	var out []byte
	for {
		line := c.readUntil([]byte("\r\n"))
		if line == nil {
			return nil
		}
		s := strings.TrimSpace(string(line))
		if semi := strings.IndexByte(s, ';'); semi >= 0 {
			s = s[:semi]
		}
		n, err := strconv.ParseInt(s, 16, 64)
		if err != nil || n < 0 {
			return nil
		}
		if n == 0 {
			// trailing headers until blank line
			for {
				l := c.readUntil([]byte("\r\n"))
				if l == nil || len(l) == 2 {
					break
				}
			}
			return out
		}
		chunk := c.readExact(int(n))
		if chunk == nil {
			return nil
		}
		out = append(out, chunk...)
		// trailing CRLF
		if c.readExact(2) == nil {
			return nil
		}
	}
}

// queryParam returns the decoded value of a query key, or "" if absent.
func (r *request) queryParam(key string) string {
	q := r.query
	for len(q) > 0 {
		amp := strings.IndexByte(q, '&')
		var kv string
		if amp < 0 {
			kv, q = q, ""
		} else {
			kv, q = q[:amp], q[amp+1:]
		}
		eq := strings.IndexByte(kv, '=')
		var k, v string
		if eq < 0 {
			k, v = kv, ""
		} else {
			k, v = kv[:eq], kv[eq+1:]
		}
		if k == key {
			return urlDecode(v)
		}
	}
	return ""
}

// headerEq reports whether a raw header name equals a lowercase name,
// case-insensitively, without allocating.
func headerEq(raw []byte, want string) bool {
	if len(raw) != len(want) {
		return false
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != want[i] {
			return false
		}
	}
	return true
}

func urlDecode(s string) string {
	if !strings.Contains(s, "%") && !strings.Contains(s, "+") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) {
			hi := hexVal(s[i+1])
			lo := hexVal(s[i+2])
			if hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		if c == '+' {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// ---- response ----

type response struct {
	status  int
	ctype   string
	extra   [][2]string
	body    []byte
	noCache bool
}

func (r *response) header(k, v string) {
	r.extra = append(r.extra, [2]string{k, v})
}

var statusText = map[int]string{
	200: "OK", 201: "Created", 204: "No Content", 400: "Bad Request",
	401: "Unauthorized", 404: "Not Found", 405: "Method Not Allowed",
	409: "Conflict", 422: "Unprocessable Entity", 500: "Internal Server Error",
}

func (c *conn) writeResponse(r *request, resp *response) bool {
	text, ok := statusText[resp.status]
	if !ok {
		text = "OK"
	}
	keepAlive := r.version == "1.1" && r.hdr.connection != "close" ||
		r.hdr.connection == "keep-alive"
	// Small bodies go out in a single write (header + body together).
	if len(resp.body) < 8192 {
		var hb []byte
		hb = append(hb, "HTTP/1.1 "...)
		hb = append(hb, strconv.Itoa(resp.status)...)
		hb = append(hb, ' ')
		hb = append(hb, text...)
		hb = append(hb, "\r\nContent-Type: "...)
		hb = append(hb, resp.ctype...)
		hb = append(hb, "\r\nContent-Length: "...)
		hb = append(hb, strconv.Itoa(len(resp.body))...)
		hb = append(hb, "\r\nX-GitHub-Request-Id: "...)
		hb = append(hb, strconv.FormatUint(r.reqID, 16)...)
		hb = append(hb, "\r\n"...)
		for _, h := range resp.extra {
			hb = append(hb, h[0]...)
			hb = append(hb, ": "...)
			hb = append(hb, h[1]...)
			hb = append(hb, "\r\n"...)
		}
		if keepAlive {
			hb = append(hb, "Connection: keep-alive\r\n"...)
		} else {
			hb = append(hb, "Connection: close\r\n"...)
		}
		hb = append(hb, "\r\n"...)
		hb = append(hb, resp.body...)
		return writeAll(c.fd, hb) && keepAlive
	}
	var hb strings.Builder
	hb.WriteString("HTTP/1.1 ")
	hb.WriteString(strconv.Itoa(resp.status))
	hb.WriteByte(' ')
	hb.WriteString(text)
	hb.WriteString("\r\nContent-Type: ")
	hb.WriteString(resp.ctype)
	hb.WriteString("\r\nContent-Length: ")
	hb.WriteString(strconv.Itoa(len(resp.body)))
	hb.WriteString("\r\nX-GitHub-Request-Id: ")
	hb.WriteString(strconv.FormatUint(r.reqID, 16))
	hb.WriteString("\r\n")
	for _, h := range resp.extra {
		hb.WriteString(h[0])
		hb.WriteString(": ")
		hb.WriteString(h[1])
		hb.WriteString("\r\n")
	}
	if keepAlive {
		hb.WriteString("Connection: keep-alive\r\n")
	} else {
		hb.WriteString("Connection: close\r\n")
	}
	hb.WriteString("\r\n")
	if !writeAll(c.fd, []byte(hb.String())) {
		return false
	}
	if len(resp.body) > 0 && !writeAll(c.fd, resp.body) {
		return false
	}
	return keepAlive
}

// jsonResp builds a JSON response.
func jsonResp(status int, body []byte) *response {
	return &response{status: status, ctype: "application/json", body: body}
}

func jsonErr(status int, msg string) *response {
	var j jb
	j.raw(`{"message":`)
	j.str(msg)
	j.raw(`}`)
	return jsonResp(status, j.b)
}

// ---- server ----

type server struct {
	st *store
}

func listenTCP() (int, int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return 0, 0, err
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fd)
		return 0, 0, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: 0, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		syscall.Close(fd)
		return 0, 0, err
	}
	if err := syscall.Listen(fd, 256); err != nil {
		syscall.Close(fd)
		return 0, 0, err
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		syscall.Close(fd)
		return 0, 0, err
	}
	port := sa.(*syscall.SockaddrInet4).Port
	return fd, port, nil
}

func (sv *server) serve(listener int) {
	for {
		fd, _, err := syscall.Accept(listener)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return
		}
		syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		go sv.handleConn(fd)
	}
}

func (sv *server) handleConn(fd int) {
	c := newConn(fd)
	defer c.close()
	for {
		r := c.parseRequest()
		if r == nil {
			return
		}
		resp := sv.dispatch(r)
		if resp == nil {
			resp = jsonErr(404, "Not Found")
		}
		if !c.writeResponse(r, resp) {
			return
		}
	}
}
