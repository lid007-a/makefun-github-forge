// store.go — in-memory data model for the forge. Port of store.ts.
//
// One Store holds every repository. All state lives here; nothing is read
// from disk at request time. Determinism: nothing here touches the wall
// clock or unseeded randomness. Creation timestamps come from the
// caller-supplied "now" string (MAKEFUN_TRIAL_NOW or a fixed fallback), so
// a live run and its replay leave byte-identical state.
//
// Concurrency: a single RWMutex on the Store guards every repo and every
// repo field. Handlers take it (read for reads, write for writes); the
// only concurrent workload is clone (read-only), so a shared read lock
// keeps the hot path parallel. Pack builds for one repo are further
// serialized by the repo's buildMu so 50 concurrent clones of the same tip
// build the pack once instead of 50 times.

package main

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ViewerLogin   = "adaiversen"
	ViewerID      = 1000
	ViewerName    = "ada iversen"
	ViewerEmail   = "adaiversen@example.invalid"
	DefaultBranch = "main"
	FixedNow      = "2025-01-01T00:00:00Z"
	// FixedEpoch is used for merge commits when no trial time is supplied.
	FixedEpoch = 1700000000
)

// trialNow is MAKEFUN_TRIAL_NOW, read once: the env never changes during a
// run, and os.Getenv takes a lock on every call.
var trialNow = os.Getenv("MAKEFUN_TRIAL_NOW")

func nowIso() string {
	if trialNow != "" {
		return trialNow
	}
	return FixedNow
}

func nowEpoch() int64 {
	if trialNow != "" {
		if tm, err := time.Parse(time.RFC3339, trialNow); err == nil {
			return tm.Unix()
		}
	}
	return FixedEpoch
}

// ---------------------------------------------------------------------------
// Git objects
// ---------------------------------------------------------------------------

type ObjType string

const (
	ObjBlob   ObjType = "blob"
	ObjTree   ObjType = "tree"
	ObjCommit ObjType = "commit"
	ObjTag    ObjType = "tag"
)

type GitObject struct {
	Type ObjType
	// Data is the raw payload, without the "<type> <len>\0" header.
	Data []byte
}

func sha1Of(typ ObjType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(string(typ) + " " + itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func storeObject(objects map[string]*GitObject, typ ObjType, data []byte) string {
	sha := sha1Of(typ, data)
	if _, ok := objects[sha]; !ok {
		objects[sha] = &GitObject{Type: typ, Data: data}
	}
	return sha
}

// storeObjectSha stores an object whose SHA-1 is already known (e.g.
// computed by readPack), skipping the redundant re-hash. Identical result
// to storeObject.
func storeObjectSha(objects map[string]*GitObject, sha string, typ ObjType, data []byte) {
	if _, ok := objects[sha]; !ok {
		objects[sha] = &GitObject{Type: typ, Data: data}
	}
}

type TreeEntry struct {
	Mode string // "100644" | "100755" | "120000" | "40000"
	Name string
	Sha  string
}

func encodeTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		an := sorted[i].Name
		if sorted[i].Mode == "40000" {
			an += "/"
		}
		bn := sorted[j].Name
		if sorted[j].Mode == "40000" {
			bn += "/"
		}
		return an < bn
	})
	var out []byte
	for _, e := range sorted {
		out = append(out, []byte(e.Mode+" "+e.Name+"\x00")...)
		raw, _ := hex.DecodeString(e.Sha)
		out = append(out, raw...)
	}
	return out
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := indexByte(data, ' ', i)
		nul := indexByte(data, 0, sp)
		mode := string(data[i:sp])
		name := string(data[sp+1 : nul])
		sha := hex.EncodeToString(data[nul+1 : nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: name, Sha: sha})
		i = nul + 21
	}
	return out
}

func indexByte(b []byte, c byte, from int) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func encodeCommit(c CommitInfo) []byte {
	var sb strings.Builder
	sb.WriteString("tree " + c.Tree + "\n")
	for _, p := range c.Parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + c.Author + "\n")
	sb.WriteString("committer " + c.Committer + "\n")
	sb.WriteString("\n")
	sb.WriteString(c.Message)
	return []byte(sb.String())
}

func parseCommit(data []byte) CommitInfo {
	text := string(data)
	headEnd := strings.Index(text, "\n\n")
	head := text
	message := ""
	if headEnd >= 0 {
		head = text[:headEnd]
		message = text[headEnd+2:]
	}
	var out CommitInfo
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			out.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			out.Parents = append(out.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			out.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			out.Committer = line[10:]
		}
	}
	out.Message = message
	return out
}

type TagInfo struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) TagInfo {
	text := string(data)
	headEnd := strings.Index(text, "\n\n")
	head := text
	message := ""
	if headEnd >= 0 {
		head = text[:headEnd]
		message = text[headEnd+2:]
	}
	var out TagInfo
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			out.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			out.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			out.Tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			out.Tagger = line[7:]
		}
	}
	out.Message = message
	return out
}

// identLine is the identity line used for commits we author:
// "name <email> epoch +0000".
func identLine() string {
	return ViewerName + " <" + ViewerEmail + "> " + itoa64(nowEpoch()) + " +0000"
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------------------------------------------------------------------------
// Repositories, issues, pull requests
// ---------------------------------------------------------------------------

type Label struct {
	Name        string
	Color       string
	Description string
}

type Comment struct {
	ID        int
	Body      string
	CreatedAt string
	UpdatedAt string
	UserLogin string
	UserID    int
}

type Issue struct {
	// cacheBody is this issue's marshaled JSON as last built, and cacheMut
	// the Store.mut it was built at: any mutating request bumps Store.mut
	// and so invalidates every cached body. The read workload is 2,000
	// GETs of one issue, so this turns each of them into a map lookup.
	cacheBody []byte
	cacheMut  uint64
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []*Label
	Comments  []*Comment
	CreatedAt string
	UpdatedAt string
	ClosedAt  string // "" means null
	UserLogin string
	UserID    int
	Seq       int // monotonic creation order within the repo
}

type PullRequest struct {
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       string // "" means null
	MergeCommitSha string // "" means null
	HeadRef        string
	HeadSha        string
	BaseRef        string
	BaseSha        string
	CreatedAt      string
	UpdatedAt      string
	Seq            int
	Comments       []*Comment
}

type fetchCacheEntry struct {
	Size int
	Body []byte
}

type Repo struct {
	ID            int
	Name          string
	Owner         string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	// git object store: sha -> object
	Objects map[string]*GitObject
	// ref name -> commit/tag sha (refs/heads/* and refs/tags/*)
	Refs   map[string]string
	Labels map[string]*Label
	Issues []*Issue
	Pulls  []*PullRequest
	// one shared counter: issues and PRs draw numbers from the same sequence
	NextNumber int
	Seq        int // creation-order counter for issues/PRs/comments
	// Cache of upload-pack fetch responses, keyed by the fetch args.
	// Each entry records the object-store size it was built at; objects are
	// append-only and content-addressed, so a matching size means the
	// reachable closure (and hence the response bytes) is unchanged.
	FetchCache map[string]*fetchCacheEntry
	// Serializes pack builds for this repo so concurrent clones of the same
	// tip build the pack once (see git.go handleUploadPack).
	buildMu sync.Mutex
}

type Store struct {
	// mu guards every repo and every repo field. Handlers take it (read for
	// reads, write for writes); the Store methods below assume it is held
	// and never lock themselves, so a handler can call several of them
	// under one lock without re-entering.
	mu         sync.RWMutex
	repos      map[string]*Repo
	nextRepoID int
	requestSeq int64
	// mut counts mutating REST requests. A cached read body records the mut
	// it was built at, so any write invalidates it without tracking fields.
	mut uint64
}

func NewStore() *Store {
	return &Store{repos: make(map[string]*Repo), nextRepoID: 1}
}

func (s *Store) requestID() string {
	n := atomic.AddInt64(&s.requestSeq, 1)
	return "req-" + itoa64(n)
}

func (s *Store) key(owner, name string) string { return owner + "/" + name }

// getRepo assumes s.mu is held (read or write).
func (s *Store) getRepo(owner, name string) *Repo {
	return s.repos[s.key(owner, name)]
}

// createRepo assumes s.mu is held for writing.
func (s *Store) createRepo(owner, name string, description string, private bool, defaultBranch string) *Repo {
	now := nowIso()
	if defaultBranch == "" {
		defaultBranch = DefaultBranch
	}
	repo := &Repo{
		ID:            s.nextRepoID,
		Name:          name,
		Owner:         owner,
		Description:   description,
		Private:       private,
		DefaultBranch: defaultBranch,
		CreatedAt:     now,
		UpdatedAt:     now,
		Objects:       make(map[string]*GitObject),
		Refs:          make(map[string]string),
		Labels:        make(map[string]*Label),
		FetchCache:    make(map[string]*fetchCacheEntry),
		NextNumber:    1,
	}
	s.nextRepoID++
	s.repos[s.key(owner, name)] = repo
	return repo
}

// deleteRepo assumes s.mu is held for writing.
func (s *Store) deleteRepo(owner, name string) bool {
	k := s.key(owner, name)
	if _, ok := s.repos[k]; !ok {
		return false
	}
	delete(s.repos, k)
	return true
}

// listReposFor assumes s.mu is held (read or write).
func (s *Store) listReposFor(owner string) []*Repo {
	var out []*Repo
	for _, r := range s.repos {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// repoByID assumes s.mu is held (read or write).
func (s *Store) repoByID(id int) *Repo {
	for _, r := range s.repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pack helpers (used by git.go)
// ---------------------------------------------------------------------------

// packObjectHeader encodes a pack object header: type + size, variable length.
func packObjectHeader(typeNum int, size int) []byte {
	var out []byte
	b := (typeNum << 4) | (size & 0x0f)
	size >>= 4
	if size > 0 {
		b |= 0x80
	}
	out = append(out, byte(b))
	for size > 0 {
		b = size & 0x7f
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		out = append(out, byte(b))
	}
	return out
}

func packTypeNum(t ObjType) int {
	switch t {
	case ObjCommit:
		return 1
	case ObjTree:
		return 2
	case ObjBlob:
		return 3
	case ObjTag:
		return 4
	}
	return 0
}
