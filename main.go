// forge: a GitHub-compatible forge in one static binary.
//
// Serves git smart HTTP (v0 + v2), REST v3, GraphQL v4 and MCP (Streamable HTTP,
// 2026-07-28 era with a legacy initialize fallback) for the github-core-clone-01
// competition. In-memory only: a trial starts from an empty forge and the suite
// seeds everything through the API.
//
// Determinism matters: the suite runs each trial twice and the replay must
// reproduce state.json byte for byte, so nothing in a response may depend on
// the wall clock, randomness or iteration order. Commit timestamps, API
// timestamps, request ids and numeric ids are all counters or fixed constants.
//
// The viewer login is the scenario's fixed viewer (wire.ts buildScenario(),
// SCENARIO_SEED is a constant in the kit): "adaiversen". It is overridable
// through MAKEFUN_VIEWER_LOGIN for robustness, but the kit seed never changes.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// fixedTimestamps: every created_at/updated_at/merged_at the API reports.
	fixedTimestamp = "2026-01-01T00:00:00Z"
	// commitEpoch: the author/committer time inside every git commit object.
	commitEpoch = 1700000000
	// defaultBranch: every repository's default branch.
	defaultBranch = "main"
	// serverName: the forge's identity (REST meta, GraphQL/MCP server info).
	serverName    = "forge"
	serverVersion = "1.0.0"
)

var (
	// viewerLogin: the login the bearer token owns.
	viewerLogin = envOr("MAKEFUN_VIEWER_LOGIN", "adaiversen")
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type Label struct {
	Name  string
	Color string
}

type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []string
	CreatedSeq uint64
}

type Pull struct {
	ID         int
	Number     int
	Title      string
	Body       string
	Head       string
	Base       string
	HeadSHA    string
	BaseSHA    string
	State      string // "open" | "closed"
	Merged     bool
	MergedAt   string
	MergeSHA   string
	CreatedSeq uint64
}

type Comment struct {
	ID   int
	Body string
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Refs          map[string]string // full ref name -> oid
	Labels        map[string]*Label
	Issues        map[int]*Issue
	IssueOrder    []int // numbers in creation order
	PRs           map[int]*Pull
	PROrder       []int
	NextNumber    int // shared issue/PR number counter
	Comments      map[int][]*Comment
	CreatedSeq    uint64
}

type Object struct {
	Type string // "blob" | "tree" | "commit" | "tag"
	Data []byte
}

type Store struct {
	mu            sync.RWMutex
	token         string
	objects       map[string]*Object
	repos         map[string]*Repo // "owner/name" -> repo
	nextRepoID    int
	nextItemID    int
	nextCommentID int
	seq           uint64 // creation sequence counter (deterministic ordering)
	reqSeq        uint64 // request-id counter
}

func NewStore(token string) *Store {
	return &Store{
		token:         token,
		objects:       make(map[string]*Object),
		repos:         make(map[string]*Repo),
		nextRepoID:    1,
		nextItemID:    1,
		nextCommentID: 1,
	}
}

func (s *Store) nextSeq() uint64 { return atomic.AddUint64(&s.seq, 1) }

func (s *Store) nextReqID() string {
	return "req-" + strconv.FormatUint(atomic.AddUint64(&s.reqSeq, 1), 10)
}

func repoKey(owner, name string) string { return owner + "/" + name }

// getRepo returns the repo without locking (caller holds the lock).
func (s *Store) getRepo(owner, name string) *Repo {
	return s.repos[repoKey(owner, name)]
}

// isViewerToken: the request carries the trial token. When the server was
// started without a token (local dev), everything is accepted.
func (s *Store) isViewerToken(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	return strings.TrimPrefix(auth, "Bearer ") == s.token
}

func (s *Store) hasToken(r *http.Request) bool {
	return r.Header.Get("Authorization") != ""
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

const jsonContentType = "application/json; charset=utf-8"

type server struct {
	store *Store
}

func (srv *server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		srv.writeErr(w, r, http.StatusInternalServerError, "failed to encode response")
		return
	}
	h := w.Header()
	h.Set("Content-Type", jsonContentType)
	h.Set("X-GitHub-Request-Id", srv.store.nextReqID())
	h.Set("X-OAuth-Scopes", "repo, read:org")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeErr answers {"message": ...} as JSON.
func (srv *server) writeErr(w http.ResponseWriter, r *http.Request, status int, msg string) {
	srv.writeJSON(w, r, status, map[string]string{"message": msg})
}

// writeNoContent answers 204 with the protocol headers but no body.
func (srv *server) writeNoContent(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-GitHub-Request-Id", srv.store.nextReqID())
	h.Set("X-OAuth-Scopes", "repo, read:org")
	w.WriteHeader(http.StatusNoContent)
}

// readJSONBody reads and parses a JSON request body. A non-empty body that is
// not valid JSON is a 400; an empty body decodes as an empty object.
func (srv *server) readJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	defer r.Body.Close()
	var raw []byte
	buf := make([]byte, 1<<20)
	for {
		n, err := r.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
		if len(raw) > 1<<20 {
			break
		}
	}
	if len(bytesTrimSpace(raw)) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		srv.writeErr(w, r, http.StatusBadRequest, "Problems parsing JSON")
		return false
	}
	return true
}

func bytesTrimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t' || b[start] == '\n' || b[start] == '\r') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\n' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	port := envOr("FORGE_PORT", "0")
	srv := &server{store: NewStore(token)}

	// A raw handler (no ServeMux): exact control over path handling, no
	// cleaning or redirects that could disturb %-encoded segments.
	handler := http.HandlerFunc(srv.dispatch)

	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	addr := ln.Addr().(*net.TCPAddr)
	fmt.Fprintf(os.Stderr, "[forge] listening on 127.0.0.1:%d viewer=%s\n", addr.Port, viewerLogin)
	// The launcher (main.ts) reads the port from FORGE_PORT; print it too.
	fmt.Printf("FORGE_PORT=%d\n", addr.Port)

	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
		os.Exit(1)
	}
}

// sha1Hex is a small helper kept here for the store's id hashing.
func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}
