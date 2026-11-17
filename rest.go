// rest.go — GitHub REST API v3 surface under /api/v3. Port of rest.ts.
//
// Every reply is JSON with a non-empty X-GitHub-Request-Id. Errors are
// JSON {message}. A repo under any owner other than the token owner is a
// 404, never a 403. Numbers for issues and pull requests come from one
// shared per-repo counter, like real GitHub.
//
// Locking: handleRest takes the store write lock for the whole handler
// (REST calls are sequential in the timed workloads, and even a GET can
// store tree objects via the directory listing, so a read lock is not
// enough). All helpers below assume the lock is held.

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type restResult struct {
	status  int
	body    any
	headers map[string]string
}

func restOK(status int, body any, headers map[string]string) restResult {
	return restResult{status: status, body: body, headers: headers}
}

func restErr(status int, message string) restResult {
	return restResult{status: status, body: map[string]any{"message": message}}
}

// ---------------------------------------------------------------------------
// JSON shapes (structs keep field order identical to the Node objects and
// marshal faster than maps on the hot read/write paths)
// ---------------------------------------------------------------------------

type actorJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
}

type userJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Type  string `json:"type"`
}

type ownerJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type labelJSON struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type repoJSON struct {
	ID            int       `json:"id"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	Owner         ownerJSON `json:"owner"`
	Private       bool      `json:"private"`
	Description   string    `json:"description"`
	DefaultBranch string    `json:"default_branch"`
	CreatedAt     string    `json:"created_at"`
	UpdatedAt     string    `json:"updated_at"`
	HTMLURL       string    `json:"html_url"`
	CloneURL      string    `json:"clone_url"`
	HasIssues     bool      `json:"has_issues"`
	HasProjects   bool      `json:"has_projects"`
	HasWiki       bool      `json:"has_wiki"`
}

type issueJSON struct {
	ID        int         `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelJSON `json:"labels"`
	User      actorJSON   `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	ClosedAt  *string     `json:"closed_at"`
	Comments  int         `json:"comments"`
	HTMLURL   string      `json:"html_url"`
}

type prRefJSON struct {
	Ref string `json:"ref"`
	Sha string `json:"sha"`
}

type prJSON struct {
	ID             int       `json:"id"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	State          string    `json:"state"`
	Merged         bool      `json:"merged"`
	MergedAt       *string   `json:"merged_at"`
	MergeCommitSha *string   `json:"merge_commit_sha"`
	Head           prRefJSON `json:"head"`
	Base           prRefJSON `json:"base"`
	User           actorJSON `json:"user"`
	CreatedAt      string    `json:"created_at"`
	UpdatedAt      string    `json:"updated_at"`
	Draft          bool      `json:"draft"`
	HTMLURL        string    `json:"html_url"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func userJson() userJSON {
	return userJSON{Login: ViewerLogin, ID: ViewerID, Name: ViewerName, Email: ViewerEmail, Type: "User"}
}

func makeRepoJSON(r *Repo) repoJSON {
	return repoJSON{
		ID:            r.ID,
		Name:          r.Name,
		FullName:      r.Owner + "/" + r.Name,
		Owner:         ownerJSON{Login: r.Owner, ID: ViewerID, Type: "User"},
		Private:       r.Private,
		Description:   r.Description,
		DefaultBranch: r.DefaultBranch,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		HTMLURL:       "https://github.com/" + r.Owner + "/" + r.Name,
		CloneURL:      "https://github.com/" + r.Owner + "/" + r.Name + ".git",
		HasIssues:     true,
		HasProjects:   true,
		HasWiki:       true,
	}
}

func makeLabelJSON(l *Label) labelJSON {
	return labelJSON{Name: l.Name, Color: l.Color, Description: l.Description}
}

func makeIssueJSON(r *Repo, i *Issue) issueJSON {
	labels := make([]labelJSON, 0, len(i.Labels))
	for _, l := range i.Labels {
		labels = append(labels, makeLabelJSON(l))
	}
	return issueJSON{
		ID:        r.ID*1000000 + i.Number,
		Number:    i.Number,
		Title:     i.Title,
		Body:      i.Body,
		State:     i.State,
		Labels:    labels,
		User:      actorJSON{Login: i.UserLogin, ID: i.UserID},
		CreatedAt: i.CreatedAt,
		UpdatedAt: i.UpdatedAt,
		ClosedAt:  strPtr(i.ClosedAt),
		Comments:  len(i.Comments),
		HTMLURL:   "https://github.com/" + r.Owner + "/" + r.Name + "/issues/" + itoa(i.Number),
	}
}

func makePRJSON(r *Repo, p *PullRequest) prJSON {
	return prJSON{
		ID:             r.ID*1000000 + p.Number,
		Number:         p.Number,
		Title:          p.Title,
		Body:           p.Body,
		State:          p.State,
		Merged:         p.Merged,
		MergedAt:       strPtr(p.MergedAt),
		MergeCommitSha: strPtr(p.MergeCommitSha),
		Head:           prRefJSON{Ref: p.HeadRef, Sha: p.HeadSha},
		Base:           prRefJSON{Ref: p.BaseRef, Sha: p.BaseSha},
		User:           actorJSON{Login: ViewerLogin, ID: ViewerID},
		CreatedAt:      p.CreatedAt,
		UpdatedAt:      p.UpdatedAt,
		Draft:          false,
		HTMLURL:        "https://github.com/" + r.Owner + "/" + r.Name + "/pull/" + itoa(p.Number),
	}
}

// ---------------------------------------------------------------------------
// Tree / commit helpers (shared with the GraphQL and MCP layers)
// ---------------------------------------------------------------------------

type flatFile struct {
	mode string
	sha  string
}

// flattenTree flattens a tree object into path -> {mode, sha} (blobs only).
func flattenTree(repo *Repo, treeSha string, prefix string, out map[string]flatFile) map[string]flatFile {
	if out == nil {
		out = make(map[string]flatFile)
	}
	obj := repo.Objects[treeSha]
	if obj == nil || obj.Type != ObjTree {
		return out
	}
	for _, entry := range parseTree(obj.Data) {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			flattenTree(repo, entry.Sha, path, out)
		} else {
			out[path] = flatFile{mode: entry.Mode, sha: entry.Sha}
		}
	}
	return out
}

// buildTree builds nested tree objects from a flat file map; returns the
// root tree sha.
func buildTree(repo *Repo, files map[string]flatFile) string {
	var build func(prefix string) string
	build = func(prefix string) string {
		var entries []TreeEntry
		subdirs := make(map[string]bool)
		var subdirList []string
		for path, file := range files {
			if prefix != "" && !strings.HasPrefix(path, prefix+"/") {
				continue
			}
			rest := path
			if prefix != "" {
				rest = path[len(prefix)+1:]
			}
			if slash := strings.Index(rest, "/"); slash < 0 {
				entries = append(entries, TreeEntry{Mode: file.mode, Name: rest, Sha: file.sha})
			} else {
				dir := rest[:slash]
				if !subdirs[dir] {
					subdirs[dir] = true
					subdirList = append(subdirList, dir)
				}
			}
		}
		for _, dir := range subdirList {
			sub := dir
			if prefix != "" {
				sub = prefix + "/" + dir
			}
			sha := build(sub)
			entries = append(entries, TreeEntry{Mode: "40000", Name: dir, Sha: sha})
		}
		return storeObject(repo.Objects, ObjTree, encodeTree(entries))
	}
	return build("")
}

func commitTree(repo *Repo, treeSha string, parents []string, message string) string {
	ident := identLine()
	data := encodeCommit(CommitInfo{Tree: treeSha, Parents: parents, Author: ident, Committer: ident, Message: message})
	return storeObject(repo.Objects, ObjCommit, data)
}

// branchTip returns the tip commit sha of a branch (or "" when absent).
func branchTip(repo *Repo, branch string) string {
	return repo.Refs["refs/heads/"+branch]
}

func resolveRefToCommit(repo *Repo, ref string) string {
	if tip := branchTip(repo, ref); tip != "" {
		return tip
	}
	if isHex40(ref) {
		if obj := repo.Objects[ref]; obj != nil && obj.Type == ObjCommit {
			return ref
		}
	}
	return ""
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

type contentCommit struct {
	commitSha string
	blobSha   string
	treeSha   string
	stale     bool
	missing   bool
}

// putContentCommit creates (or updates) a file on a branch, making the
// branch if needed. Returns the new commit sha. expectedSha is enforced on
// update: a mismatch is a stale-sha failure (stale: true).
func putContentCommit(repo *Repo, branch, path string, content []byte, message, expectedSha string, hasExpected bool) contentCommit {
	tip := branchTip(repo, branch)
	files := make(map[string]flatFile)
	if tip != "" {
		commit := parseCommit(repo.Objects[tip].Data)
		flattenTree(repo, commit.Tree, "", files)
	}
	existing, ok := files[path]
	if ok {
		if hasExpected && expectedSha != existing.sha {
			return contentCommit{stale: true}
		}
	} else if hasExpected {
		return contentCommit{stale: true}
	}
	blobSha := storeObject(repo.Objects, ObjBlob, content)
	files[path] = flatFile{mode: "100644", sha: blobSha}
	treeSha := buildTree(repo, files)
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	commitSha := commitTree(repo, treeSha, parents, message)
	repo.Refs["refs/heads/"+branch] = commitSha
	repo.UpdatedAt = nowIso()
	return contentCommit{commitSha: commitSha, blobSha: blobSha, treeSha: treeSha}
}

func deleteContentCommit(repo *Repo, branch, path, message, expectedSha string, hasExpected bool) contentCommit {
	tip := branchTip(repo, branch)
	if tip == "" {
		return contentCommit{missing: true}
	}
	commit := parseCommit(repo.Objects[tip].Data)
	files := flattenTree(repo, commit.Tree, "", nil)
	existing, ok := files[path]
	if !ok {
		return contentCommit{missing: true}
	}
	if hasExpected && expectedSha != existing.sha {
		return contentCommit{stale: true}
	}
	delete(files, path)
	treeSha := buildTree(repo, files)
	commitSha := commitTree(repo, treeSha, []string{tip}, message)
	repo.Refs["refs/heads/"+branch] = commitSha
	repo.UpdatedAt = nowIso()
	return contentCommit{commitSha: commitSha, blobSha: existing.sha, treeSha: treeSha}
}

// mergeBase returns the first common ancestor of two commits (or "").
func mergeBase(repo *Repo, a, b string) string {
	ancestorsA := make(map[string]bool)
	stackA := []string{a}
	for len(stackA) > 0 {
		sha := stackA[len(stackA)-1]
		stackA = stackA[:len(stackA)-1]
		if ancestorsA[sha] {
			continue
		}
		obj := repo.Objects[sha]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		ancestorsA[sha] = true
		for _, p := range parseCommit(obj.Data).Parents {
			stackA = append(stackA, p)
		}
	}
	stackB := []string{b}
	seenB := make(map[string]bool)
	for len(stackB) > 0 {
		sha := stackB[len(stackB)-1]
		stackB = stackB[:len(stackB)-1]
		if ancestorsA[sha] {
			return sha
		}
		if seenB[sha] {
			continue
		}
		seenB[sha] = true
		obj := repo.Objects[sha]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			stackB = append(stackB, p)
		}
	}
	return ""
}

type fileChange struct {
	Filename  string
	Status    string // "added" | "modified" | "changed"
	Additions int
	Deletions int
	Changes   int
}

func countLines(buf []byte) []string {
	if len(buf) == 0 {
		return nil
	}
	text := string(buf)
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func lineDiff(oldBuf, newBuf []byte) (additions, deletions int) {
	var oldLines, newLines []string
	if oldBuf != nil {
		oldLines = countLines(oldBuf)
	}
	if newBuf != nil {
		newLines = countLines(newBuf)
	}
	oldCount := make(map[string]int)
	for _, l := range oldLines {
		oldCount[l]++
	}
	common := 0
	for _, l := range newLines {
		if n := oldCount[l]; n > 0 {
			oldCount[l] = n - 1
			common++
		}
	}
	return len(newLines) - common, len(oldLines) - common
}

// diffTrees diffs two trees; deletions are reported as "modified" (the
// suite's file status enum is added/modified/changed).
func diffTrees(repo *Repo, baseTree, headTree string) []fileChange {
	base := flattenTree(repo, baseTree, "", nil)
	head := flattenTree(repo, headTree, "", nil)
	pathSet := make(map[string]bool)
	for p := range base {
		pathSet[p] = true
	}
	for p := range head {
		pathSet[p] = true
	}
	var out []fileChange
	for path := range pathSet {
		b, hasB := base[path]
		h, hasH := head[path]
		switch {
		case hasB && !hasH:
			var oldData []byte
			if obj := repo.Objects[b.sha]; obj != nil {
				oldData = obj.Data
			}
			_, d := lineDiff(oldData, nil)
			out = append(out, fileChange{Filename: path, Status: "modified", Additions: 0, Deletions: d, Changes: d})
		case !hasB && hasH:
			var newData []byte
			if obj := repo.Objects[h.sha]; obj != nil {
				newData = obj.Data
			}
			a, _ := lineDiff(nil, newData)
			out = append(out, fileChange{Filename: path, Status: "added", Additions: a, Deletions: 0, Changes: a})
		case hasB && hasH && b.sha != h.sha:
			var oldData, newData []byte
			if obj := repo.Objects[b.sha]; obj != nil {
				oldData = obj.Data
			}
			if obj := repo.Objects[h.sha]; obj != nil {
				newData = obj.Data
			}
			a, d := lineDiff(oldData, newData)
			out = append(out, fileChange{Filename: path, Status: "modified", Additions: a, Deletions: d, Changes: a + d})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}

type squashResult struct {
	sha      string
	message  string
	conflict bool
}

// squashMerge squash-merges a pull request onto its base branch.
func squashMerge(repo *Repo, pr *PullRequest) squashResult {
	baseTip := branchTip(repo, pr.BaseRef)
	headTip := branchTip(repo, pr.HeadRef)
	if baseTip == "" || headTip == "" {
		return squashResult{conflict: true}
	}
	baseCommit := parseCommit(repo.Objects[baseTip].Data)
	headCommit := parseCommit(repo.Objects[headTip].Data)
	if base := mergeBase(repo, baseTip, headTip); base != "" {
		baseTree := parseCommit(repo.Objects[base].Data).Tree
		headChanges := diffTrees(repo, baseTree, headCommit.Tree)
		baseChanges := diffTrees(repo, baseTree, baseCommit.Tree)
		baseChanged := make(map[string]fileChange)
		for _, c := range baseChanges {
			baseChanged[c.Filename] = c
		}
		headFiles := flattenTree(repo, headCommit.Tree, "", nil)
		baseFiles := flattenTree(repo, baseCommit.Tree, "", nil)
		for _, change := range headChanges {
			if _, ok := baseChanged[change.Filename]; !ok {
				continue
			}
			// Both sides changed the same file: conflict unless the resulting
			// blobs are identical.
			headFile, hasH := headFiles[change.Filename]
			baseFile, hasB := baseFiles[change.Filename]
			if !hasH || !hasB || headFile.sha != baseFile.sha {
				return squashResult{conflict: true}
			}
		}
	}
	message := pr.Title
	sha := commitTree(repo, headCommit.Tree, []string{baseTip}, message)
	repo.Refs["refs/heads/"+pr.BaseRef] = sha
	repo.UpdatedAt = nowIso()
	return squashResult{sha: sha, message: message}
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

type jsonBody = map[string]any

func strVal(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func parseBody(raw []byte) (jsonBody, bool) {
	if len(raw) == 0 {
		return jsonBody{}, false
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, true
	}
	if m, ok := parsed.(map[string]any); ok {
		return m, false
	}
	return nil, true
}

func paginate[T any](items []T, query url.Values, basePath, baseURL string) (page []T, link string) {
	perPage := 30
	if v := query.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			perPage = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	pageNum := 1
	if v := query.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			pageNum = n
		}
	}
	if pageNum < 1 {
		pageNum = 1
	}
	start := (pageNum - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	page = items[start:end]
	if start+perPage < len(items) {
		next := baseURL + basePath + "?page=" + itoa(pageNum+1) + "&per_page=" + itoa(perPage)
		link = "<" + next + ">; rel=\"next\""
		lastPage := (len(items) + perPage - 1) / perPage
		link += ", <" + baseURL + basePath + "?page=" + itoa(lastPage) + "&per_page=" + itoa(perPage) + ">; rel=\"last\""
	}
	return page, link
}

func unesc(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// handleRest serves /api/v3/*. It takes the store write lock for the whole
// handler (see the file header). The lock is held while routing and while
// the repo helpers run; the JSON body is parsed before the lock is taken
// only by the caller reading it — here it is parsed under the lock, which
// is cheap.
func handleRest(store *Store, method, path string, query url.Values, rawBody []byte, baseURL string) restResult {
	store.mu.Lock()
	// Any request that can change state bumps mut, which invalidates every
	// cached read body; GET leaves them warm.
	if method != "GET" {
		store.mut++
	}
	defer store.mu.Unlock()
	return handleRestLocked(store, method, path, query, rawBody, baseURL)
}

// rawJSON is a response body that is already marshaled: writeJSON sends it
// as is instead of encoding it again.
type rawJSON []byte

// issueCreateBody is the subset of a create-issue body the handler reads.
type issueCreateBody struct {
	Title  string            `json:"title"`
	Body   *string           `json:"body"`
	Labels []json.RawMessage `json:"labels"`
}

func handleRestLocked(store *Store, method, path string, query url.Values, rawBody []byte, baseURL string) restResult {
	var segments []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	// /api/v3[/...]
	if len(segments) < 2 || segments[0] != "api" || segments[1] != "v3" {
		return restErr(404, "Not Found")
	}
	rest := segments[2:]

	// GET /api/v3/ — root
	if len(rest) == 0 {
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		return restOK(200, map[string]any{
			"current_user_url":      "/api/v3/user",
			"repository_url":        "/api/v3/repos/{owner}/{repo}",
			"user_repositories_url": "/api/v3/user/repos",
		}, map[string]string{"X-OAuth-Scopes": "repo, user", "X-Accepted-OAuth-Scopes": "repo, user"})
	}

	// GET /api/v3/meta
	if len(rest) == 1 && rest[0] == "meta" {
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		return restOK(200, map[string]any{"verifiable": true, "installed_version": "1.0.0", "version": "1.0.0"}, nil)
	}

	// GET /api/v3/user and /api/v3/user/repos
	if rest[0] == "user" {
		if len(rest) == 1 {
			if method != "GET" {
				return restErr(404, "Not Found")
			}
			return restOK(200, userJson(), nil)
		}
		if len(rest) == 2 && rest[1] == "repos" {
			if method == "GET" {
				repos := store.listReposFor(ViewerLogin)
				page, link := paginate(repos, query, "/api/v3/user/repos", baseURL)
				out := make([]repoJSON, 0, len(page))
				for _, r := range page {
					out = append(out, makeRepoJSON(r))
				}
				var headers map[string]string
				if link != "" {
					headers = map[string]string{"Link": link}
				}
				return restOK(200, out, headers)
			}
			if method == "POST" {
				body, malformed := parseBody(rawBody)
				if malformed || body == nil {
					return restErr(400, "Problems parsing JSON")
				}
				name, _ := strVal(body["name"])
				if name == "" {
					return restErr(422, "name is required")
				}
				if store.getRepo(ViewerLogin, name) != nil {
					return restErr(422, "Repository already exists")
				}
				desc, _ := strVal(body["description"])
				private, _ := body["private"].(bool)
				defBranch, _ := strVal(body["default_branch"])
				repo := store.createRepo(ViewerLogin, name, desc, private, defBranch)
				return restOK(201, makeRepoJSON(repo), nil)
			}
			return restErr(404, "Not Found")
		}
		return restErr(404, "Not Found")
	}

	// /api/v3/repos/{owner}/{repo}[/...]
	if rest[0] == "repos" {
		if len(rest) < 3 {
			return restErr(404, "Not Found")
		}
		owner := unesc(rest[1])
		name := unesc(rest[2])
		if owner != ViewerLogin {
			return restErr(404, "Not Found")
		}
		repo := store.getRepo(owner, name)
		tail := rest[3:]

		if len(tail) == 0 {
			if repo == nil {
				return restErr(404, "Not Found")
			}
			if method == "GET" {
				return restOK(200, makeRepoJSON(repo), nil)
			}
			if method == "DELETE" {
				store.deleteRepo(owner, name)
				return restResult{status: 204, body: nil}
			}
			if method == "PATCH" {
				body, malformed := parseBody(rawBody)
				if malformed || body == nil {
					return restErr(400, "Problems parsing JSON")
				}
				if desc, ok := strVal(body["description"]); ok {
					repo.Description = desc
				}
				repo.UpdatedAt = nowIso()
				return restOK(200, makeRepoJSON(repo), nil)
			}
			return restErr(404, "Not Found")
		}

		if repo == nil {
			return restErr(404, "Not Found")
		}
		return handleRepoSub(store, repo, tail, method, query, rawBody, baseURL)
	}

	return restErr(404, "Not Found")
}

func handleRepoSub(store *Store, repo *Repo, tail []string, method string, query url.Values, rawBody []byte, baseURL string) restResult {
	section := tail[0]
	rest := tail[1:]

	// ---- git/refs ----
	if section == "git" && len(rest) > 0 && rest[0] == "refs" {
		if len(rest) == 1 {
			if method == "POST" {
				body, malformed := parseBody(rawBody)
				if malformed || body == nil {
					return restErr(400, "Problems parsing JSON")
				}
				ref, _ := strVal(body["ref"])
				sha, _ := strVal(body["sha"])
				if ref == "" || sha == "" {
					return restErr(422, "ref and sha are required")
				}
				if _, exists := repo.Refs[ref]; exists {
					return restErr(422, "Reference already exists")
				}
				if repo.Objects[sha] == nil {
					return restErr(422, "Object does not exist")
				}
				repo.Refs[ref] = sha
				return restOK(201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha}}, nil)
			}
			if method == "GET" {
				refs := sortedRefs(repo)
				out := make([]map[string]any, 0, len(refs))
				for _, r := range refs {
					out = append(out, map[string]any{"ref": r.name, "object": map[string]any{"sha": r.sha}})
				}
				return restOK(200, out, nil)
			}
			return restErr(404, "Not Found")
		}
		// GET /git/refs/heads/{branch} (branch may contain slashes)
		if method == "GET" && len(rest) >= 2 {
			ref := "refs/" + strings.Join(rest[1:], "/")
			sha, ok := repo.Refs[ref]
			if !ok {
				return restErr(404, "Not Found")
			}
			return restOK(200, map[string]any{"ref": ref, "object": map[string]any{"sha": sha}}, nil)
		}
		return restErr(404, "Not Found")
	}

	// ---- branches ----
	if section == "branches" {
		var branches []string
		for r := range repo.Refs {
			if strings.HasPrefix(r, "refs/heads/") {
				branches = append(branches, r[len("refs/heads/"):])
			}
		}
		sort.Strings(branches)
		if len(rest) == 0 {
			if method != "GET" {
				return restErr(404, "Not Found")
			}
			out := make([]map[string]any, 0, len(branches))
			for _, b := range branches {
				out = append(out, map[string]any{"name": b, "commit": map[string]any{"sha": repo.Refs["refs/heads/"+b]}})
			}
			return restOK(200, out, nil)
		}
		branch := unesc(strings.Join(rest, "/"))
		sha, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			return restErr(404, "Not Found")
		}
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		return restOK(200, map[string]any{"name": branch, "commit": map[string]any{"sha": sha}}, nil)
	}

	// ---- commits ----
	if section == "commits" {
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		ref := query.Get("sha")
		if ref == "" {
			ref = repo.DefaultBranch
		}
		tip := resolveRefToCommit(repo, ref)
		if tip == "" {
			return restOK(200, []any{}, nil)
		}
		var out []map[string]any
		seen := make(map[string]bool)
		stack := []string{tip}
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
			info := parseCommit(obj.Data)
			out = append(out, map[string]any{"sha": sha, "commit": map[string]any{"message": info.Message}})
			for _, p := range info.Parents {
				stack = append(stack, p)
			}
		}
		if out == nil {
			out = []map[string]any{}
		}
		return restOK(200, out, nil)
	}

	// ---- contents ----
	if section == "contents" {
		var parts []string
		for _, s := range rest {
			parts = append(parts, unesc(s))
		}
		filePath := strings.Join(parts, "/")
		if method == "GET" {
			ref := query.Get("ref")
			if ref == "" {
				ref = repo.DefaultBranch
			}
			tip := resolveRefToCommit(repo, ref)
			if tip == "" {
				return restErr(404, "Not Found")
			}
			commit := parseCommit(repo.Objects[tip].Data)
			files := flattenTree(repo, commit.Tree, "", nil)
			if filePath == "" {
				entries := make([]map[string]any, 0, len(files))
				for p, f := range files {
					entries = append(entries, map[string]any{"type": "file", "name": p, "path": p, "sha": f.sha})
				}
				sort.Slice(entries, func(i, j int) bool { return entries[i]["name"].(string) < entries[j]["name"].(string) })
				return restOK(200, entries, nil)
			}
			if file, ok := files[filePath]; ok {
				blob := repo.Objects[file.sha]
				return restOK(200, map[string]any{
					"type":     "file",
					"encoding": "base64",
					"content":  base64.StdEncoding.EncodeToString(blob.Data),
					"sha":      file.sha,
					"size":     len(blob.Data),
					"name":     filePath[strings.LastIndex(filePath, "/")+1:],
					"path":     filePath,
				}, nil)
			}
			// directory listing
			prefix := filePath + "/"
			dirs := make(map[string]bool)
			var entries []map[string]any
			for p, f := range files {
				if !strings.HasPrefix(p, prefix) {
					continue
				}
				restPath := p[len(prefix):]
				if slash := strings.Index(restPath, "/"); slash < 0 {
					entries = append(entries, map[string]any{"type": "file", "name": restPath, "path": p, "sha": f.sha})
				} else {
					dirs[restPath[:slash]] = true
				}
			}
			if len(entries) == 0 && len(dirs) == 0 {
				return restErr(404, "Not Found")
			}
			var dirNames []string
			for d := range dirs {
				dirNames = append(dirNames, d)
			}
			sort.Strings(dirNames)
			for _, d := range dirNames {
				// directory sha: the tree object for this subdir
				subFiles := make(map[string]flatFile)
				for p, f := range files {
					if strings.HasPrefix(p, filePath+"/"+d+"/") || p == filePath+"/"+d {
						subFiles[p[len(prefix):]] = f
					}
				}
				treeSha := buildTree(repo, subFiles)
				entries = append(entries, map[string]any{"type": "dir", "name": d, "path": filePath + "/" + d, "sha": treeSha})
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i]["name"].(string) < entries[j]["name"].(string) })
			return restOK(200, entries, nil)
		}
		if method == "PUT" {
			body, malformed := parseBody(rawBody)
			if malformed || body == nil {
				return restErr(400, "Problems parsing JSON")
			}
			contentB64, ok := strVal(body["content"])
			if !ok {
				return restErr(422, "content is required")
			}
			branch, _ := strVal(body["branch"])
			if branch == "" {
				branch = repo.DefaultBranch
			}
			message, _ := strVal(body["message"])
			if message == "" {
				message = "Update " + filePath
			}
			content, _ := base64.StdEncoding.DecodeString(contentB64)
			// 201 when the file is created, 200 when it already existed.
			tipBefore := branchTip(repo, branch)
			existed := false
			if tipBefore != "" {
				_, existed = flattenTree(repo, parseCommit(repo.Objects[tipBefore].Data).Tree, "", nil)[filePath]
			}
			shaArg, hasSha := strVal(body["sha"])
			result := putContentCommit(repo, branch, filePath, content, message, shaArg, hasSha)
			if result.stale {
				return restErr(409, "sha does not match the current file")
			}
			status := 201
			if existed {
				status = 200
			}
			return restOK(status, map[string]any{
				"content": map[string]any{
					"type":     "file",
					"encoding": "base64",
					"content":  contentB64,
					"sha":      result.blobSha,
					"size":     len(content),
					"name":     filePath[strings.LastIndex(filePath, "/")+1:],
					"path":     filePath,
				},
				"commit": map[string]any{"sha": result.commitSha, "message": message},
			}, nil)
		}
		if method == "DELETE" {
			body, malformed := parseBody(rawBody)
			if malformed || body == nil {
				return restErr(400, "Problems parsing JSON")
			}
			branch, _ := strVal(body["branch"])
			if branch == "" {
				branch = repo.DefaultBranch
			}
			message, _ := strVal(body["message"])
			if message == "" {
				message = "Delete " + filePath
			}
			shaArg, hasSha := strVal(body["sha"])
			result := deleteContentCommit(repo, branch, filePath, message, shaArg, hasSha)
			if result.missing {
				return restErr(404, "Not Found")
			}
			if result.stale {
				return restErr(409, "sha does not match the current file")
			}
			return restOK(200, map[string]any{"commit": map[string]any{"sha": result.commitSha, "message": message}}, nil)
		}
		return restErr(404, "Not Found")
	}

	// ---- labels ----
	if section == "labels" {
		if method == "GET" && len(rest) == 0 {
			out := make([]labelJSON, 0, len(repo.Labels))
			for _, l := range repo.Labels {
				out = append(out, makeLabelJSON(l))
			}
			return restOK(200, out, nil)
		}
		if method == "POST" && len(rest) == 0 {
			body, malformed := parseBody(rawBody)
			if malformed || body == nil {
				return restErr(400, "Problems parsing JSON")
			}
			name, _ := strVal(body["name"])
			if name == "" {
				return restErr(422, "name is required")
			}
			if _, exists := repo.Labels[name]; exists {
				return restErr(422, "Label already exists")
			}
			color, _ := strVal(body["color"])
			if color == "" {
				color = "ededed"
			}
			desc, _ := strVal(body["description"])
			label := &Label{Name: name, Color: color, Description: desc}
			repo.Labels[name] = label
			return restOK(201, makeLabelJSON(label), nil)
		}
		return restErr(404, "Not Found")
	}

	// ---- issues ----
	if section == "issues" {
		if len(rest) == 0 {
			if method == "GET" {
				state := query.Get("state")
				if state == "" {
					state = "open"
				}
				sortParam := query.Get("sort")
				if sortParam == "" {
					sortParam = "created"
				}
				direction := query.Get("direction")
				if direction == "" {
					direction = "desc"
				}
				var list []*Issue
				for _, i := range repo.Issues {
					if state == "all" || i.State == state {
						list = append(list, i)
					}
				}
				if sortParam == "created" {
					sort.Slice(list, func(a, b int) bool {
						if direction == "asc" {
							return list[a].Seq < list[b].Seq
						}
						return list[a].Seq > list[b].Seq
					})
				}
				page, link := paginate(list, query, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues", baseURL)
				out := make([]issueJSON, 0, len(page))
				for _, i := range page {
					out = append(out, makeIssueJSON(repo, i))
				}
				var headers map[string]string
				if link != "" {
					headers = map[string]string{"Link": link}
				}
				return restOK(200, out, headers)
			}
			if method == "POST" {
				// Decode straight into the three fields the create reads: a
				// map[string]any parse allocates a value per key, and this is
				// the write workload's every request.
				var in issueCreateBody
				if len(rawBody) == 0 {
					return restErr(400, "Problems parsing JSON")
				}
				if err := json.Unmarshal(rawBody, &in); err != nil {
					return restErr(400, "Problems parsing JSON")
				}
				title := in.Title
				if title == "" {
					return restErr(422, "title is required")
				}
				var labels []*Label
				for _, raw := range in.Labels {
					var n string
					if len(raw) > 0 && raw[0] == '"' {
						if err := json.Unmarshal(raw, &n); err != nil {
							continue
						}
					} else {
						var ref struct {
							Name string `json:"name"`
						}
						if err := json.Unmarshal(raw, &ref); err != nil {
							continue
						}
						n = ref.Name
					}
					l, ok := repo.Labels[n]
					if !ok {
						return restErr(422, "Label '"+n+"' does not exist")
					}
					labels = append(labels, l)
				}
				now := nowIso()
				issueBody := ""
				if in.Body != nil {
					issueBody = *in.Body
				}
				issue := &Issue{
					Number:    repo.NextNumber,
					Title:     title,
					Body:      issueBody,
					State:     "open",
					Labels:    labels,
					Comments:  []*Comment{},
					CreatedAt: now,
					UpdatedAt: now,
					ClosedAt:  "",
					UserLogin: ViewerLogin,
					UserID:    ViewerID,
				}
				repo.NextNumber++
				repo.Seq++
				issue.Seq = repo.Seq
				repo.Issues = append(repo.Issues, issue)
				return restOK(201, makeIssueJSON(repo, issue), nil)
			}
			return restErr(404, "Not Found")
		}
		number, err := strconv.Atoi(rest[0])
		if err != nil {
			return restErr(404, "Not Found")
		}
		var issue *Issue
		for _, i := range repo.Issues {
			if i.Number == number {
				issue = i
				break
			}
		}
		if issue == nil {
			return restErr(404, "Not Found")
		}
		if len(rest) == 1 {
			if method == "GET" {
				if issue.cacheBody != nil && issue.cacheMut == store.mut {
					return restResult{status: 200, body: rawJSON(issue.cacheBody)}
				}
				out := marshalJSON(makeIssueJSON(repo, issue))
				issue.cacheBody = out
				issue.cacheMut = store.mut
				return restResult{status: 200, body: rawJSON(out)}
			}
			if method == "PATCH" {
				body, malformed := parseBody(rawBody)
				if malformed || body == nil {
					return restErr(400, "Problems parsing JSON")
				}
				if title, ok := strVal(body["title"]); ok {
					issue.Title = title
				}
				if b, ok := strVal(body["body"]); ok {
					issue.Body = b
				}
				if state, ok := strVal(body["state"]); ok && (state == "closed" || state == "open") {
					issue.State = state
					if state == "closed" {
						issue.ClosedAt = nowIso()
					} else {
						issue.ClosedAt = ""
					}
				}
				issue.UpdatedAt = nowIso()
				return restOK(200, makeIssueJSON(repo, issue), nil)
			}
			return restErr(404, "Not Found")
		}
		if rest[1] == "comments" {
			if method == "GET" {
				out := make([]map[string]any, 0, len(issue.Comments))
				for _, c := range issue.Comments {
					out = append(out, map[string]any{
						"id": c.ID, "body": c.Body,
						"user":       map[string]any{"login": c.UserLogin, "id": c.UserID},
						"created_at": c.CreatedAt, "updated_at": c.UpdatedAt,
					})
				}
				return restOK(200, out, nil)
			}
			if method == "POST" {
				body, malformed := parseBody(rawBody)
				if malformed || body == nil {
					return restErr(400, "Problems parsing JSON")
				}
				text, ok := strVal(body["body"])
				if !ok || text == "" {
					return restErr(422, "body is required")
				}
				now := nowIso()
				repo.Seq++
				comment := &Comment{
					ID:        repo.Seq,
					Body:      text,
					CreatedAt: now,
					UpdatedAt: now,
					UserLogin: ViewerLogin,
					UserID:    ViewerID,
				}
				issue.Comments = append(issue.Comments, comment)
				return restOK(201, map[string]any{
					"id": comment.ID, "body": comment.Body,
					"user":       map[string]any{"login": comment.UserLogin, "id": comment.UserID},
					"created_at": comment.CreatedAt, "updated_at": comment.UpdatedAt,
				}, nil)
			}
			return restErr(404, "Not Found")
		}
		return restErr(404, "Not Found")
	}

	// ---- pulls ----
	if section == "pulls" {
		if len(rest) == 0 {
			if method == "GET" {
				state := query.Get("state")
				if state == "" {
					state = "open"
				}
				var list []*PullRequest
				for _, p := range repo.Pulls {
					if state == "all" || p.State == state {
						list = append(list, p)
					}
				}
				page, link := paginate(list, query, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/pulls", baseURL)
				out := make([]prJSON, 0, len(page))
				for _, p := range page {
					out = append(out, makePRJSON(repo, p))
				}
				var headers map[string]string
				if link != "" {
					headers = map[string]string{"Link": link}
				}
				return restOK(200, out, headers)
			}
			if method == "POST" {
				body, malformed := parseBody(rawBody)
				if malformed || body == nil {
					return restErr(400, "Problems parsing JSON")
				}
				title, _ := strVal(body["title"])
				head, _ := strVal(body["head"])
				base, _ := strVal(body["base"])
				if title == "" || head == "" || base == "" {
					return restErr(422, "title, head and base are required")
				}
				headSha := branchTip(repo, head)
				baseSha := branchTip(repo, base)
				if headSha == "" || baseSha == "" {
					return restErr(422, "head or base branch does not exist")
				}
				now := nowIso()
				bodyStr, _ := strVal(body["body"])
				pr := &PullRequest{
					Number:    repo.NextNumber,
					Title:     title,
					Body:      bodyStr,
					State:     "open",
					Merged:    false,
					MergedAt:  "",
					HeadRef:   head,
					HeadSha:   headSha,
					BaseRef:   base,
					BaseSha:   baseSha,
					CreatedAt: now,
					UpdatedAt: now,
					Comments:  []*Comment{},
				}
				repo.NextNumber++
				repo.Seq++
				pr.Seq = repo.Seq
				repo.Pulls = append(repo.Pulls, pr)
				return restOK(201, makePRJSON(repo, pr), nil)
			}
			return restErr(404, "Not Found")
		}
		number, err := strconv.Atoi(rest[0])
		if err != nil {
			return restErr(404, "Not Found")
		}
		var pr *PullRequest
		for _, p := range repo.Pulls {
			if p.Number == number {
				pr = p
				break
			}
		}
		if pr == nil {
			return restErr(404, "Not Found")
		}
		if len(rest) == 1 {
			if method == "GET" {
				return restOK(200, makePRJSON(repo, pr), nil)
			}
			return restErr(404, "Not Found")
		}
		if rest[1] == "files" {
			if method != "GET" {
				return restErr(404, "Not Found")
			}
			baseTip := branchTip(repo, pr.BaseRef)
			headTip := branchTip(repo, pr.HeadRef)
			if baseTip == "" || headTip == "" {
				return restErr(404, "Not Found")
			}
			base := mergeBase(repo, baseTip, headTip)
			baseTree := ""
			if base != "" {
				baseTree = parseCommit(repo.Objects[base].Data).Tree
			} else {
				baseTree = buildTree(repo, make(map[string]flatFile))
			}
			headTree := parseCommit(repo.Objects[headTip].Data).Tree
			changes := diffTrees(repo, baseTree, headTree)
			out := make([]map[string]any, 0, len(changes))
			for _, c := range changes {
				out = append(out, map[string]any{
					"filename": c.Filename, "status": c.Status,
					"additions": c.Additions, "deletions": c.Deletions, "changes": c.Changes,
				})
			}
			return restOK(200, out, nil)
		}
		if rest[1] == "merge" {
			if method != "PUT" {
				return restErr(404, "Not Found")
			}
			if pr.State != "open" {
				return restErr(405, "Pull request is already closed")
			}
			result := squashMerge(repo, pr)
			if result.conflict {
				return restErr(405, "Merge conflict")
			}
			pr.State = "closed"
			pr.Merged = true
			pr.MergedAt = nowIso()
			pr.MergeCommitSha = result.sha
			pr.UpdatedAt = nowIso()
			return restOK(200, map[string]any{"sha": result.sha, "merged": true, "message": result.message}, nil)
		}
		return restErr(404, "Not Found")
	}

	return restErr(404, "Not Found")
}

// ---------------------------------------------------------------------------
// Small helpers the other surfaces reuse
// ---------------------------------------------------------------------------

func findIssue(repo *Repo, number int) *Issue {
	for _, i := range repo.Issues {
		if i.Number == number {
			return i
		}
	}
	return nil
}

func findPull(repo *Repo, number int) *PullRequest {
	for _, p := range repo.Pulls {
		if p.Number == number {
			return p
		}
	}
	return nil
}

func shaShape(s string) bool { return isHex40(s) }
