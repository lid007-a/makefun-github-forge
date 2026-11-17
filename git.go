// git.go — git smart HTTP: pkt-line codec, pack read/write, object graph
// walking, upload-pack (v0 + v2) and receive-pack (v0). Port of git.ts.
//
// Wire notes (verified against the suite's own git-client):
// - v2 fetch responses name their sections with literal pkt-lines
//   ("acknowledgments\n", "shallow-info\n", "packfile\n"), each section
//   ends with a flush, and there is NO response-end packet.
// - A v2 fetch with nothing to send still carries an empty 32-byte pack
//   as one sideband packet (the client errors if the packfile section
//   has zero packets).
// - v0 wants carry capabilities space-separated on the first want line.
// - Push reports are pkt-lined inside sideband band 1, then a flush.

package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

const ZeroOID = "0000000000000000000000000000000000000000"
const agentStr = "forge/1.0"

const uploadV0Caps = "side-band-64k thin-pack ofs-delta multi_ack_detailed shallow symref=HEAD:refs/heads/main agent=forge/1.0"
const receiveV0Caps = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

const hexDigits = "0123456789abcdef"

func pktLine(payload []byte) []byte {
	l := len(payload) + 4
	if l > 65520 {
		panic("pkt-line too long")
	}
	out := make([]byte, l)
	out[0] = hexDigits[(l>>12)&0xf]
	out[1] = hexDigits[(l>>8)&0xf]
	out[2] = hexDigits[(l>>4)&0xf]
	out[3] = hexDigits[l&0xf]
	copy(out[4:], payload)
	return out
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

func pktFlush() []byte { return []byte("0000") }
func pktDelim() []byte { return []byte("0001") }

type pktKind int

const (
	pktData pktKind = iota
	pktFlushKind
	pktDelimKind
	pktResponseEnd
)

type pkt struct {
	kind    pktKind
	payload []byte
}

// decodePktLines decodes a whole buffer of pkt-lines. Stops at the first
// non-pkt-line byte (raw pack data).
func decodePktLines(buf []byte) (packets []pkt, rest []byte) {
	i := 0
	for i+4 <= len(buf) {
		header := string(buf[i : i+4])
		if len(header) != 4 || !isHex4(header) {
			break
		}
		l := hexVal(header)
		if l == 0 {
			packets = append(packets, pkt{kind: pktFlushKind})
			i += 4
			continue
		}
		if l == 1 {
			packets = append(packets, pkt{kind: pktDelimKind})
			i += 4
			continue
		}
		if l == 2 {
			packets = append(packets, pkt{kind: pktResponseEnd})
			i += 4
			continue
		}
		if l < 4 || i+l > len(buf) {
			break
		}
		packets = append(packets, pkt{kind: pktData, payload: buf[i+4 : i+l]})
		i += l
	}
	return packets, buf[i:]
}

func isHex4(s string) bool {
	for i := 0; i < 4; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func hexVal(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		default:
			return -1
		}
		n = n*16 + v
	}
	return n
}

// sidebandPackets splits sideband pack bytes into pkt-lines with a band
// byte (chunk <= 32768 data bytes).
func sidebandPackets(data []byte, band byte) [][]byte {
	var out [][]byte
	const chunk = 32768
	if len(data) == 0 {
		out = append(out, pktLine([]byte{band}))
		return out
	}
	for i := 0; i < len(data); i += chunk {
		end := i + chunk
		if end > len(data) {
			end = len(data)
		}
		p := make([]byte, 0, 1+(end-i))
		p = append(p, band)
		p = append(p, data[i:end]...)
		out = append(out, pktLine(p))
	}
	return out
}

// ---------------------------------------------------------------------------
// Pack reading (with delta resolution, including thin packs)
// ---------------------------------------------------------------------------

type packEntry struct {
	sha  string
	typ  ObjType
	data []byte
}

func readVarint(buf []byte, off int) (int, int) {
	v := int(buf[off] & 0x7f)
	shift := 7
	i := off + 1
	for buf[i-1]&0x80 != 0 {
		v |= int(buf[i]&0x7f) << shift
		shift += 7
		i++
	}
	return v, i
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readDeltaVarint := func() int {
		v := 0
		shift := 0
		for {
			b := delta[pos]
			pos++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v
	}
	srcSize := readDeltaVarint()
	tgtSize := readDeltaVarint()
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	var out [][]byte
	written := 0
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			offset := 0
			size := 0
			if op&0x01 != 0 {
				offset = int(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				offset |= int(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				offset |= int(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				offset |= int(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				size = int(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				size |= int(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				size |= int(delta[pos]) << 16
				pos++
			}
			if size == 0 {
				size = 0x10000
			}
			out = append(out, base[offset:offset+size])
			written += size
		} else if op != 0 {
			out = append(out, delta[pos:pos+int(op)])
			pos += int(op)
			written += int(op)
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	result := bytes.Join(out, nil)
	if written != tgtSize || len(result) != tgtSize {
		return nil, fmt.Errorf("delta target size mismatch")
	}
	return result, nil
}

func readPack(buf []byte, resolveBase func(sha string) *GitObject) ([]packEntry, error) {
	if len(buf) < 12 || string(buf[0:4]) != "PACK" {
		return nil, fmt.Errorf("not a packfile")
	}
	version := binary.BigEndian.Uint32(buf[4:8])
	if version != 2 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := binary.BigEndian.Uint32(buf[8:12])
	off := 12
	var entries []packEntry
	byOffset := make(map[int]*packEntry)
	bySha := make(map[string]*packEntry)
	for n := uint32(0); n < count; n++ {
		start := off
		first := buf[off]
		off++
		typeNum := (first >> 4) & 0x07
		size := int(first & 0x0f)
		shift := 4
		for first&0x80 != 0 {
			b := buf[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
			first = b
		}
		var baseSha string
		var baseOffset int
		hasBaseOffset := false
		if typeNum == 6 {
			// OFS_DELTA: negative offset varint
			b := buf[off]
			off++
			offset := int(b & 0x7f)
			for b&0x80 != 0 {
				b = buf[off]
				off++
				offset = ((offset + 1) << 7) | int(b&0x7f)
			}
			baseOffset = start - offset
			hasBaseOffset = true
		} else if typeNum == 7 {
			baseSha = hex.EncodeToString(buf[off : off+20])
			off += 20
		}
		raw, next, err := inflateEntry(buf, off)
		if err != nil {
			return nil, err
		}
		off = next
		_ = size
		if typeNum >= 1 && typeNum <= 4 {
			typ := [...]ObjType{"", ObjCommit, ObjTree, ObjBlob, ObjTag}[typeNum]
			e := packEntry{sha: sha1Of(typ, raw), typ: typ, data: raw}
			entries = append(entries, e)
			ep := &entries[len(entries)-1]
			byOffset[start] = ep
			bySha[e.sha] = ep
		} else if typeNum == 6 || typeNum == 7 {
			var base *packEntry
			if hasBaseOffset {
				base = byOffset[baseOffset]
			} else if baseSha != "" {
				base = bySha[baseSha]
				if base == nil && resolveBase != nil {
					if obj := resolveBase(baseSha); obj != nil {
						base = &packEntry{sha: baseSha, typ: obj.Type, data: obj.Data}
					}
				}
			}
			if base == nil {
				return nil, fmt.Errorf("delta base not found")
			}
			data, err := applyDelta(base.data, raw)
			if err != nil {
				return nil, err
			}
			e := packEntry{sha: sha1Of(base.typ, data), typ: base.typ, data: data}
			entries = append(entries, e)
			ep := &entries[len(entries)-1]
			byOffset[start] = ep
			bySha[e.sha] = ep
		} else {
			return nil, fmt.Errorf("unknown pack object type %d", typeNum)
		}
	}
	return entries, nil
}

// flateReaderPool reuses flate decompressors. Each one owns a 32 KiB
// history window, and one push carries three objects, so pooling keeps the
// unpack path from allocating a window per object.
var flateReaderPool sync.Pool

type flateResetter interface {
	io.ReadCloser
	Reset(r io.Reader, dict []byte) error
}

func getFlateReader(r io.Reader) flateResetter {
	if v := flateReaderPool.Get(); v != nil {
		fr := v.(flateResetter)
		if err := fr.Reset(r, nil); err == nil {
			return fr
		}
	}
	return flate.NewReader(r).(flateResetter)
}

// inflateEntry inflates one zlib stream starting at off and returns the
// offset just past it. The deflate payload goes to a pooled flate reader
// over a bytes.Reader: a bytes.Reader is a flate.Reader, so the inflater
// consumes exactly the deflate stream and never reads ahead, and the bytes
// it left are the stream's 4-byte Adler-32 trailer. One inflate per object.
func inflateEntry(buf []byte, off int) ([]byte, int, error) {
	if off+2 > len(buf) {
		return nil, 0, fmt.Errorf("truncated zlib header")
	}
	if hdr := uint16(buf[off])<<8 | uint16(buf[off+1]); hdr%31 != 0 || buf[off]&0x0f != 8 || buf[off+1]&0x20 != 0 {
		return nil, 0, fmt.Errorf("bad zlib header")
	}
	br := bytes.NewReader(buf[off+2:])
	fr := getFlateReader(br)
	data, err := io.ReadAll(fr)
	fr.Close()
	flateReaderPool.Put(fr)
	if err != nil {
		return nil, 0, err
	}
	next := off + 2 + (len(buf) - off - 2 - br.Len()) + 4
	if next > len(buf) {
		return nil, 0, fmt.Errorf("truncated zlib trailer")
	}
	return data, next, nil
}

func deflate(data []byte) []byte {
	var b bytes.Buffer
	// BestSpeed (level 1): the packs here are small (depth-1 clone packs,
	// single pushes), so the slightly larger output costs almost nothing
	// on the wire, while pure-Go flate at level 1 is several times faster
	// than at the default level 6 — and the pack trailer is recomputed
	// over the bytes actually sent, so the client still verifies it.
	w, _ := zlib.NewWriterLevel(&b, zlib.BestSpeed)
	w.Write(data)
	w.Close()
	return b.Bytes()
}

// ---------------------------------------------------------------------------
// Pack writing (plain objects only — no deltas)
// ---------------------------------------------------------------------------

func writePack(objects []*GitObject) []byte {
	var parts [][]byte
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objects)))
	parts = append(parts, header)
	for _, obj := range objects {
		typeNum := packTypeNum(obj.Type)
		parts = append(parts, packObjectHeader(typeNum, len(obj.Data)))
		parts = append(parts, deflate(obj.Data))
	}
	body := bytes.Join(parts, nil)
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

func emptyPack() []byte { return writePack(nil) }

// ---------------------------------------------------------------------------
// Object graph: reachability with depth limits and have-exclusion
// ---------------------------------------------------------------------------

// orderedObjs is an insertion-ordered object set: the pack object order
// must be deterministic (Go maps iterate randomly), so insertion order is
// kept explicitly, matching the Node Map it ports.
type orderedObjs struct {
	m     map[string]*GitObject
	order []string
}

func newOrderedObjs() *orderedObjs { return &orderedObjs{m: make(map[string]*GitObject)} }

func (o *orderedObjs) set(sha string, obj *GitObject) {
	if _, ok := o.m[sha]; !ok {
		o.m[sha] = obj
		o.order = append(o.order, sha)
	}
}

func (o *orderedObjs) has(sha string) bool { _, ok := o.m[sha]; return ok }

func (o *orderedObjs) values() []*GitObject {
	out := make([]*GitObject, 0, len(o.order))
	for _, sha := range o.order {
		out = append(out, o.m[sha])
	}
	return out
}

type collectResult struct {
	objects   []*GitObject
	shallow   []string
	unshallow []string
}

// peelToCommit resolves a tag object to its ultimate target commit.
func peelToCommit(repo *Repo, sha string) (string, bool) {
	cur := sha
	seen := make(map[string]bool)
	for {
		if seen[cur] {
			return "", false
		}
		seen[cur] = true
		obj := repo.Objects[cur]
		if obj == nil {
			return "", false
		}
		if obj.Type == ObjCommit {
			return cur, true
		}
		if obj.Type == ObjTag {
			cur = parseTag(obj.Data).Object
			continue
		}
		return "", false
	}
}

func collectTreeObjects(repo *Repo, treeSha string, out *orderedObjs, exclude map[string]bool) {
	obj := repo.Objects[treeSha]
	if obj == nil || obj.Type != ObjTree || out.has(treeSha) {
		return
	}
	if exclude != nil && exclude[treeSha] {
		return
	}
	out.set(treeSha, obj)
	for _, entry := range parseTree(obj.Data) {
		if exclude != nil && exclude[entry.Sha] {
			continue
		}
		child := repo.Objects[entry.Sha]
		if child == nil {
			continue
		}
		if entry.Mode == "40000" {
			collectTreeObjects(repo, entry.Sha, out, exclude)
		} else if !out.has(entry.Sha) {
			out.set(entry.Sha, child)
		}
	}
}

func reachableCommits(repo *Repo, tips []string, stopAt map[string]bool) map[string]bool {
	seen := make(map[string]bool)
	stack := append([]string(nil), tips...)
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[sha] {
			continue
		}
		obj := repo.Objects[sha]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		seen[sha] = true
		// A shallow boundary is a commit the client holds but whose parents
		// it does not: the walk stops there, it does not cross it.
		if stopAt != nil && stopAt[sha] {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			stack = append(stack, p)
		}
	}
	return seen
}

// reachableObjects returns every object (commit/tree/blob) reachable from
// the given commits.
func reachableObjects(repo *Repo, tips []string, stopAt map[string]bool) map[string]bool {
	out := make(map[string]bool)
	commits := reachableCommits(repo, tips, stopAt)
	for sha := range commits {
		out[sha] = true
		obj := repo.Objects[sha]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		found := newOrderedObjs()
		collectTreeObjects(repo, parseCommit(obj.Data).Tree, found, nil)
		for _, key := range found.order {
			out[key] = true
		}
	}
	return out
}

type collectOpts struct {
	depth         *int
	haves         []string
	clientShallow []string
}

func collectObjects(repo *Repo, wants []string, opts collectOpts) collectResult {
	// Objects the client already holds: everything reachable from its haves,
	// stopping at its shallow boundaries (it holds the boundary commit, not
	// the boundary's parents).
	stopAt := make(map[string]bool)
	for _, s := range opts.clientShallow {
		stopAt[s] = true
	}
	var haveReachable, haveObjects map[string]bool
	if len(opts.haves) > 0 {
		haveReachable = reachableCommits(repo, opts.haves, stopAt)
		haveObjects = reachableObjects(repo, opts.haves, stopAt)
	} else {
		haveReachable = make(map[string]bool)
		haveObjects = make(map[string]bool)
	}
	// Traversal set: the depth-limited walk from the wants, ignoring haves.
	// Haves prune the PACK, never the walk — a deepen fetch must still reach
	// the parent of a have that is a shallow boundary.
	sent := make(map[string]bool)
	var sentOrder []string
	type qItem struct {
		sha   string
		depth int
	}
	var queue []qItem
	var tagObjects []string
	for _, want := range wants {
		obj := repo.Objects[want]
		if obj == nil {
			continue
		}
		if obj.Type == ObjTag {
			tagObjects = append(tagObjects, want)
			if target, ok := peelToCommit(repo, want); ok {
				queue = append(queue, qItem{sha: target, depth: 0})
			}
			continue
		}
		if obj.Type == ObjCommit {
			queue = append(queue, qItem{sha: want, depth: 0})
		}
	}
	maxDepth := opts.depth
	for head := 0; head < len(queue); head++ {
		sha, depth := queue[head].sha, queue[head].depth
		if sent[sha] {
			continue
		}
		obj := repo.Objects[sha]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		sent[sha] = true
		sentOrder = append(sentOrder, sha)
		if maxDepth == nil || depth+1 < *maxDepth {
			for _, p := range parseCommit(obj.Data).Parents {
				queue = append(queue, qItem{sha: p, depth: depth + 1})
			}
		}
	}
	// New shallow boundaries: sent commits with an unsent parent that exists
	// and that the client does not already hold.
	var shallow []string
	for _, sha := range sentOrder {
		obj := repo.Objects[sha]
		for _, p := range parseCommit(obj.Data).Parents {
			if !sent[p] && repo.Objects[p] != nil && !haveReachable[p] {
				shallow = append(shallow, sha)
				break
			}
		}
	}
	// Unshallow: client boundaries whose parents are now all sent or held.
	var unshallow []string
	for _, b := range opts.clientShallow {
		if !sent[b] {
			continue
		}
		obj := repo.Objects[b]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		parents := parseCommit(obj.Data).Parents
		all := true
		for _, p := range parents {
			if !sent[p] && !haveReachable[p] {
				all = false
				break
			}
		}
		if all {
			unshallow = append(unshallow, b)
		}
	}
	out := newOrderedObjs()
	for _, tagSha := range tagObjects {
		obj := repo.Objects[tagSha]
		if obj != nil && !haveObjects[tagSha] {
			out.set(tagSha, obj)
		}
	}
	for _, sha := range sentOrder {
		if haveReachable[sha] {
			continue
		}
		obj := repo.Objects[sha]
		out.set(sha, obj)
		collectTreeObjects(repo, parseCommit(obj.Data).Tree, out, haveObjects)
	}
	return collectResult{objects: out.values(), shallow: shallow, unshallow: unshallow}
}

// ---------------------------------------------------------------------------
// Advertisements
// ---------------------------------------------------------------------------

type refPair struct {
	name string
	sha  string
}

func sortedRefs(repo *Repo) []refPair {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]refPair, 0, len(names))
	for _, name := range names {
		out = append(out, refPair{name: name, sha: repo.Refs[name]})
	}
	return out
}

func headTarget(repo *Repo) string { return "refs/heads/" + repo.DefaultBranch }

func refLinesV0(repo *Repo, caps string) [][]byte {
	var lines [][]byte
	refs := sortedRefs(repo)
	head := headTarget(repo)
	headSha, hasHead := repo.Refs[head]
	if len(refs) == 0 {
		lines = append(lines, pktLineStr(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
		return lines
	}
	var headLine string
	if hasHead {
		headLine = headSha + " HEAD\x00" + caps + "\n"
	} else {
		headLine = ZeroOID + " HEAD\x00" + caps + "\n"
	}
	lines = append(lines, pktLineStr(headLine))
	for _, ref := range refs {
		lines = append(lines, pktLineStr(ref.sha+" "+ref.name+"\n"))
		obj := repo.Objects[ref.sha]
		if obj != nil && obj.Type == ObjTag {
			target := parseTag(obj.Data).Object
			peeled, ok := peelToCommit(repo, target)
			if !ok {
				peeled = target
			}
			lines = append(lines, pktLineStr(peeled+" "+ref.name+"^{}\n"))
		}
	}
	return lines
}

func infoRefsUploadPack(repo *Repo, version2 bool) []byte {
	preamble := pktLineStr("# service=git-upload-pack\n")
	if version2 {
		return bytes.Join([][]byte{
			preamble,
			pktFlush(),
			pktLineStr("version 2\n"),
			pktLineStr("agent=" + agentStr + "\n"),
			pktLineStr("ls-refs=unborn\n"),
			pktLineStr("fetch=shallow filter\n"),
			pktLineStr("object-format=sha1\n"),
			pktFlush(),
		}, nil)
	}
	parts := [][]byte{preamble, pktFlush()}
	parts = append(parts, refLinesV0(repo, uploadV0Caps)...)
	parts = append(parts, pktFlush())
	return bytes.Join(parts, nil)
}

func infoRefsReceivePack(repo *Repo) []byte {
	parts := [][]byte{pktLineStr("# service=git-receive-pack\n"), pktFlush()}
	parts = append(parts, refLinesV0(repo, receiveV0Caps)...)
	parts = append(parts, pktFlush())
	return bytes.Join(parts, nil)
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

type gitResult struct {
	status      int
	contentType string
	body        []byte
}

type fetchArgs struct {
	wants   []string
	haves   []string
	shallow []string
	depth   *int
	filter  string
	// done: the client sent `done`: no further negotiation rounds. Real git
	// then expects NO acknowledgments section — the response goes straight
	// to shallow-info (if any) and packfile. Without `done` the server must
	// answer the acknowledgments section and no packfile.
	done bool
}

func parseFetchArgsV2(body []byte) fetchArgs {
	packets, _ := decodePktLines(body)
	args := fetchArgs{}
	seenDelim := false
	for _, p := range packets {
		if p.kind == pktFlushKind {
			break
		}
		if p.kind == pktDelimKind {
			seenDelim = true
			continue
		}
		if p.kind != pktData || p.payload == nil {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if !seenDelim {
			continue // command= / agent= / object-format= lines
		}
		switch {
		case strings.HasPrefix(line, "want "):
			args.wants = append(args.wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			args.haves = append(args.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if d, err := parseIntStr(strings.TrimSpace(line[7:])); err == nil && d != 0 {
				args.depth = &d
			}
		case strings.HasPrefix(line, "filter "):
			args.filter = strings.TrimSpace(line[7:])
		case line == "done":
			args.done = true
		}
	}
	return args
}

func parseIntStr(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(s[i]-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

func parseFetchArgsV0(body []byte) fetchArgs {
	packets, _ := decodePktLines(body)
	args := fetchArgs{}
	section := 0 // 0 = wants, 1 = haves
	for _, p := range packets {
		if p.kind == pktFlushKind {
			section++
			if section > 1 {
				break
			}
			continue
		}
		if p.kind != pktData || p.payload == nil {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if section == 0 {
			switch {
			case strings.HasPrefix(line, "want "):
				// first want line may carry space-separated capabilities
				rest := line[5:]
				if sp := strings.Index(rest, " "); sp >= 0 {
					rest = rest[:sp]
				}
				args.wants = append(args.wants, strings.TrimSpace(rest))
			case strings.HasPrefix(line, "shallow "):
				args.shallow = append(args.shallow, strings.TrimSpace(line[8:]))
			case strings.HasPrefix(line, "deepen "):
				if d, err := parseIntStr(strings.TrimSpace(line[7:])); err == nil && d != 0 {
					args.depth = &d
				}
			case strings.HasPrefix(line, "filter "):
				args.filter = strings.TrimSpace(line[7:])
			}
		} else {
			switch {
			case strings.HasPrefix(line, "have "):
				args.haves = append(args.haves, strings.TrimSpace(line[5:]))
			case line == "done":
				args.done = true
			}
		}
	}
	return args
}

func buildFetchResponseV2(repo *Repo, args fetchArgs) []byte {
	// Negotiation round (no `done`): acknowledgments only, no packfile.
	if !args.done {
		return bytes.Join([][]byte{pktLineStr("acknowledgments\n"), pktLineStr("NAK\n"), pktFlush()}, nil)
	}
	collected := collectObjects(repo, args.wants, collectOpts{depth: args.depth, haves: args.haves, clientShallow: args.shallow})
	var parts [][]byte
	// shallow-info only when there are lines to send (real git omits the
	// section otherwise); it ends with a delim, not a flush.
	if len(collected.shallow) > 0 || len(collected.unshallow) > 0 {
		parts = append(parts, pktLineStr("shallow-info\n"))
		for _, s := range collected.shallow {
			parts = append(parts, pktLineStr("shallow "+s+"\n"))
		}
		for _, s := range collected.unshallow {
			parts = append(parts, pktLineStr("unshallow "+s+"\n"))
		}
		parts = append(parts, pktDelim())
	}
	parts = append(parts, pktLineStr("packfile\n"))
	pack := writePack(collected.objects)
	parts = append(parts, sidebandPackets(pack, 1)...)
	parts = append(parts, pktFlush())
	return bytes.Join(parts, nil)
}

func buildFetchResponseV0(repo *Repo, args fetchArgs) []byte {
	collected := collectObjects(repo, args.wants, collectOpts{depth: args.depth, haves: args.haves, clientShallow: args.shallow})
	var parts [][]byte
	parts = append(parts, pktLineStr("NAK\n"))
	for _, s := range collected.shallow {
		parts = append(parts, pktLineStr("shallow "+s+"\n"))
	}
	for _, s := range collected.unshallow {
		parts = append(parts, pktLineStr("unshallow "+s+"\n"))
	}
	pack := writePack(collected.objects)
	parts = append(parts, sidebandPackets(pack, 1)...)
	parts = append(parts, pktFlush())
	return bytes.Join(parts, nil)
}

func fetchCacheKey(version2 bool, args fetchArgs) string {
	var sb strings.Builder
	if version2 {
		sb.WriteString("2|")
	} else {
		sb.WriteString("0|")
	}
	sb.WriteString(strings.Join(args.wants, ","))
	sb.WriteString("|")
	sb.WriteString(strings.Join(args.haves, ","))
	sb.WriteString("|")
	sb.WriteString(strings.Join(args.shallow, ","))
	sb.WriteString("|")
	if args.depth != nil {
		sb.WriteString(itoa(*args.depth))
	} else {
		sb.WriteString("null")
	}
	sb.WriteString("|")
	sb.WriteString(args.filter)
	sb.WriteString("|")
	if args.done {
		sb.WriteString("1")
	} else {
		sb.WriteString("0")
	}
	return sb.String()
}

func buildEmptyFetchResponse(version2 bool) []byte {
	if version2 {
		parts := [][]byte{pktLineStr("packfile\n")}
		parts = append(parts, sidebandPackets(emptyPack(), 1)...)
		parts = append(parts, pktFlush())
		return bytes.Join(parts, nil)
	}
	parts := [][]byte{pktLineStr("NAK\n")}
	parts = append(parts, sidebandPackets(emptyPack(), 1)...)
	parts = append(parts, pktFlush())
	return bytes.Join(parts, nil)
}

// handleUploadPack serves a fetch. It manages the store lock itself: the
// fetch cache is checked under a read lock, and pack builds for one repo
// are serialized by repo.buildMu so 50 concurrent clones of the same tip
// build the pack once (the rest wait on buildMu, then hit the cache).
func handleUploadPack(store *Store, repo *Repo, body []byte, version2 bool) (out gitResult) {
	defer func() {
		if recover() != nil {
			out = gitResult{status: 400, contentType: "application/x-git-upload-pack-result", body: []byte{}}
		}
	}()
	var args fetchArgs
	if version2 {
		args = parseFetchArgsV2(body)
	} else {
		args = parseFetchArgsV0(body)
	}
	key := fetchCacheKey(version2, args)

	// Fast path: cache hit under a read lock.
	store.mu.RLock()
	if e, ok := repo.FetchCache[key]; ok && e.Size == len(repo.Objects) {
		b := e.Body
		store.mu.RUnlock()
		return gitResult{status: 200, contentType: "application/x-git-upload-pack-result", body: b}
	}
	store.mu.RUnlock()

	// Serialize pack builds for this repo: the first builder fills the
	// cache; the rest wait, then re-check it.
	repo.buildMu.Lock()
	defer repo.buildMu.Unlock()

	store.mu.RLock()
	if e, ok := repo.FetchCache[key]; ok && e.Size == len(repo.Objects) {
		b := e.Body
		store.mu.RUnlock()
		return gitResult{status: 200, contentType: "application/x-git-upload-pack-result", body: b}
	}
	store.mu.RUnlock()

	var resp []byte
	var size int
	// Build under a read lock; the closure's deferred RUnlock runs even if
	// the build panics, so the outer recover never leaves the lock held.
	resp, size = func() ([]byte, int) {
		store.mu.RLock()
		defer store.mu.RUnlock()
		var r []byte
		if len(args.wants) == 0 {
			// No wants: answer an empty pack so the client still sees a packfile.
			r = buildEmptyFetchResponse(version2)
		} else if version2 {
			r = buildFetchResponseV2(repo, args)
		} else {
			r = buildFetchResponseV0(repo, args)
		}
		return r, len(repo.Objects)
	}()

	// Bound the cache (evict an arbitrary entry past 32) so a repo with
	// many distinct fetches does not grow it without limit.
	store.mu.Lock()
	if len(repo.FetchCache) >= 32 {
		for k := range repo.FetchCache {
			delete(repo.FetchCache, k)
			break
		}
	}
	repo.FetchCache[key] = &fetchCacheEntry{Size: size, Body: resp}
	store.mu.Unlock()
	return gitResult{status: 200, contentType: "application/x-git-upload-pack-result", body: resp}
}

func handleLsRefs(store *Store, repo *Repo, body []byte) (out gitResult) {
	defer func() {
		if recover() != nil {
			out = gitResult{status: 400, contentType: "application/x-git-upload-pack-result", body: []byte{}}
		}
	}()
	store.mu.RLock()
	defer store.mu.RUnlock()
	packets, _ := decodePktLines(body)
	peel := false
	symrefs := false
	unborn := false
	var prefixes []string
	seenDelim := false
	for _, p := range packets {
		if p.kind == pktFlushKind {
			break
		}
		if p.kind == pktDelimKind {
			seenDelim = true
			continue
		}
		if p.kind != pktData || p.payload == nil || !seenDelim {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[11:])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, pre := range prefixes {
			if strings.HasPrefix(name, pre) {
				return true
			}
		}
		return false
	}
	var parts [][]byte
	head := headTarget(repo)
	headSha, hasHead := repo.Refs[head]
	if keep("HEAD") {
		if hasHead {
			extra := ""
			if symrefs {
				extra = " symref-target:" + head
			}
			parts = append(parts, pktLineStr(headSha+" HEAD"+extra+"\n"))
		} else if unborn {
			parts = append(parts, pktLineStr("unborn HEAD symref-target:"+head+"\n"))
		}
	}
	for _, ref := range sortedRefs(repo) {
		if !keep(ref.name) {
			continue
		}
		line := ref.sha + " " + ref.name
		if peel {
			obj := repo.Objects[ref.sha]
			if obj != nil && obj.Type == ObjTag {
				if peeled, ok := peelToCommit(repo, parseTag(obj.Data).Object); ok {
					line += " peeled:" + peeled
				}
			}
		}
		parts = append(parts, pktLineStr(line+"\n"))
	}
	parts = append(parts, pktFlush())
	return gitResult{status: 200, contentType: "application/x-git-upload-pack-result", body: bytes.Join(parts, nil)}
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func isDescendant(repo *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{descendant}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if sha == ancestor {
			return true
		}
		if seen[sha] {
			continue
		}
		seen[sha] = true
		obj := repo.Objects[sha]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			stack = append(stack, p)
		}
	}
	return false
}

func handleReceivePack(store *Store, repo *Repo, body []byte) (out gitResult) {
	reportLines := []string{}
	fail := func(reason string) gitResult {
		reportLines = append(reportLines, "unpack "+reason)
		// The report stream ends with a flush packet INSIDE band 1; a raw
		// flush then terminates the sideband stream.
		var lines [][]byte
		for _, l := range reportLines {
			lines = append(lines, pktLineStr(l+"\n"))
		}
		lines = append(lines, pktFlush())
		report := bytes.Join(lines, nil)
		parts := sidebandPackets(report, 1)
		parts = append(parts, pktFlush())
		return gitResult{status: 200, contentType: "application/x-git-receive-pack-result", body: bytes.Join(parts, nil)}
	}
	defer func() {
		if r := recover(); r != nil {
			msg := "unpack error"
			if err, ok := r.(error); ok {
				msg = "unpack error: " + err.Error()
			}
			out = fail(msg)
		}
	}()

	// The whole push (unpack + ref updates) holds the write lock: pushes
	// are sequential in the workloads, and the object store must not change
	// under the unpack.
	store.mu.Lock()
	defer store.mu.Unlock()

	packets, rest := decodePktLines(body)
	var commands []pushCommand
	for _, p := range packets {
		if p.kind == pktFlushKind {
			break
		}
		if p.kind != pktData || p.payload == nil {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		parts := strings.Split(line, " ")
		if len(parts) < 3 {
			continue
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}
	// Unpack the pack (if any) before applying ref updates.
	hasPack := false
	for _, c := range commands {
		if c.newOID != ZeroOID {
			hasPack = true
			break
		}
	}
	if hasPack && len(rest) > 0 {
		entries, err := readPack(rest, func(sha string) *GitObject { return repo.Objects[sha] })
		if err != nil {
			return fail("unpack error: " + err.Error())
		}
		for _, e := range entries {
			storeObjectSha(repo.Objects, e.sha, e.typ, e.data)
		}
	}
	reportLines = append(reportLines, "unpack ok")
	for _, cmd := range commands {
		current, exists := repo.Refs[cmd.ref]
		if cmd.newOID == ZeroOID {
			// delete
			if !exists {
				reportLines = append(reportLines, "ng "+cmd.ref+" remote ref does not exist")
			} else {
				delete(repo.Refs, cmd.ref)
				reportLines = append(reportLines, "ok "+cmd.ref)
			}
			continue
		}
		if cmd.oldOID == ZeroOID {
			// create
			if exists {
				reportLines = append(reportLines, "ng "+cmd.ref+" already exists")
				continue
			}
			if repo.Objects[cmd.newOID] == nil {
				reportLines = append(reportLines, "ng "+cmd.ref+" object not found")
				continue
			}
			repo.Refs[cmd.ref] = cmd.newOID
			reportLines = append(reportLines, "ok "+cmd.ref)
			continue
		}
		// update: fast-forward only
		if current != cmd.oldOID {
			reportLines = append(reportLines, "ng "+cmd.ref+" non-fast-forward")
			continue
		}
		if repo.Objects[cmd.newOID] == nil {
			reportLines = append(reportLines, "ng "+cmd.ref+" object not found")
			continue
		}
		if !isDescendant(repo, cmd.oldOID, cmd.newOID) {
			reportLines = append(reportLines, "ng "+cmd.ref+" non-fast-forward")
			continue
		}
		repo.Refs[cmd.ref] = cmd.newOID
		reportLines = append(reportLines, "ok "+cmd.ref)
	}
	var lines [][]byte
	for _, l := range reportLines {
		lines = append(lines, pktLineStr(l+"\n"))
	}
	lines = append(lines, pktFlush())
	report := bytes.Join(lines, nil)
	parts := sidebandPackets(report, 1)
	parts = append(parts, pktFlush())
	return gitResult{status: 200, contentType: "application/x-git-receive-pack-result", body: bytes.Join(parts, nil)}
}
