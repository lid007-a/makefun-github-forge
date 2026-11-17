// mcp.go — Model Context Protocol at /mcp: JSON-RPC 2.0 over Streamable
// HTTP (plain JSON answers; SSE is accepted by the client but not needed).
// Port of mcp.ts.
//
// Two eras are served from the same endpoint:
// - modern 2026-07-28: `server/discover` returns supportedVersions,
//   capabilities and serverInfo in one request; no handshake.
// - legacy 2025-11-25 and earlier: `initialize` (+ the negotiated
//   revision echoed back) then `notifications/initialized`, with an
//   optional Mcp-Session-Id.
//
// Tools act on the same Store as REST and GraphQL, so ids and objects are
// identical across surfaces. Every tools/list answer is byte-identical:
// the tool list is one frozen constant.

package main

import (
	"encoding/json"
	"strings"
)

const mcpCurrent = "2026-07-28"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpServerName = "github-forge"
const mcpServerVersion = "1.0.0"

type toolProp struct {
	typ  string
	desc string
}

type toolDef struct {
	name        string
	description string
	properties  [][3]string // [name, type, description], in order
	required    []string
}

func (t toolDef) toJSON() map[string]any {
	props := make(map[string]any)
	for _, p := range t.properties {
		props[p[0]] = map[string]any{"type": p[1], "description": p[2]}
	}
	return map[string]any{
		"name":        t.name,
		"description": t.description,
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": props,
			"required":   t.required,
		},
	}
}

var mcpTools = []toolDef{
	{
		name:        "create_repo",
		description: "Create a new repository owned by the authenticated user.",
		properties: [][3]string{
			{"name", "string", "Repository name."},
			{"description", "string", "Repository description."},
			{"private", "boolean", "Whether the repository is private."},
		},
		required: []string{"name"},
	},
	{
		name:        "get_repo",
		description: "Get a repository by owner and name.",
		properties: [][3]string{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		properties: [][3]string{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"title", "string", "Issue title."},
			{"body", "string", "Issue body."},
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name:        "list_issues",
		description: "List issues in a repository, optionally filtered by state.",
		properties: [][3]string{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"state", "string", "Issue state: open, closed or all."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_pull_request",
		description: "Open a pull request from a head branch onto a base branch.",
		properties: [][3]string{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"title", "string", "Pull request title."},
			{"head", "string", "Head branch name."},
			{"base", "string", "Base branch name."},
			{"body", "string", "Pull request body."},
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name:        "merge_pull_request",
		description: "Squash-merge a pull request onto its base branch.",
		properties: [][3]string{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"pull_number", "integer", "Pull request number."},
		},
		required: []string{"owner", "repo", "pull_number"},
	},
	{
		name:        "list_repos",
		description: "List repositories owned by the authenticated user.",
		properties: [][3]string{
			{"owner", "string", "Owner login (defaults to the authenticated user)."},
		},
		required: []string{},
	},
	{
		name:        "get_issue",
		description: "Get one issue by repository and number.",
		properties: [][3]string{
			{"owner", "string", "Repository owner login."},
			{"repo", "string", "Repository name."},
			{"issue_number", "integer", "Issue number."},
		},
		required: []string{"owner", "repo", "issue_number"},
	},
}

func mcpServerInfo() map[string]any {
	return map[string]any{"name": mcpServerName, "version": mcpServerVersion}
}

// ---------------------------------------------------------------------------
// Argument picking: accept the documented names and their common synonyms,
// and a full_name ("owner/repo") when owner/repo are absent.
// ---------------------------------------------------------------------------

func mcpPick(args map[string]any, names ...string) any {
	for _, n := range names {
		if v, ok := args[n]; ok && v != nil && v != "" {
			return v
		}
	}
	return nil
}

func mcpPickOwnerRepo(args map[string]any) (owner, repo string) {
	owner = toStr(mcpPick(args, "owner", "owner_login", "org", "organization", "user", "login"))
	repo = toStr(mcpPick(args, "repo", "repository", "repo_name", "name"))
	if full := toStr(mcpPick(args, "full_name", "repo_full_name", "fullName")); (owner == "" || repo == "") && strings.Contains(full, "/") {
		parts := strings.SplitN(full, "/", 2)
		if owner == "" {
			owner = parts[0]
		}
		if repo == "" {
			repo = parts[1]
		}
	}
	return owner, repo
}

// ---------------------------------------------------------------------------
// Tool implementations
// ---------------------------------------------------------------------------

func mcpToolResult(data any, isError bool) map[string]any {
	var text string
	if s, ok := data.(string); ok {
		text = s
	} else {
		text = string(marshalJSON(data))
	}
	// structuredContent is the data object itself when it is an object,
	// else {result: data} — mirroring the Node port.
	var structured any
	switch data.(type) {
	case map[string]any, repoJSON, issueJSON, prJSON:
		structured = data
	default:
		structured = map[string]any{"result": data}
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": structured,
		"isError":           isError,
	}
}

func mcpErrResult(message string) map[string]any {
	return mcpToolResult(map[string]any{"error": message}, true)
}

func mcpRepoFor(args map[string]any, store *Store) (*Repo, string) {
	owner, repoName := mcpPickOwnerRepo(args)
	if owner == "" || repoName == "" {
		return nil, "owner and repo are required"
	}
	found := store.getRepo(owner, repoName)
	if found == nil {
		return nil, "Repository not found: " + owner + "/" + repoName
	}
	return found, ""
}

type mcpCallOut struct {
	result   map[string]any
	rpcError *mcpErrorInfo
}

type mcpErrorInfo struct {
	code    int
	message string
}

func mcpCallTool(store *Store, name string, args map[string]any) mcpCallOut {
	fail := func(message string) mcpCallOut {
		return mcpCallOut{result: mcpErrResult(message)}
	}
	switch name {
	case "create_repo":
		repoName := toStr(mcpPick(args, "name"))
		if repoName == "" {
			return fail("Missing required argument: name")
		}
		if store.getRepo(ViewerLogin, repoName) != nil {
			return fail("Repository already exists: " + ViewerLogin + "/" + repoName)
		}
		desc := toStr(mcpPick(args, "description"))
		private, _ := mcpPick(args, "private").(bool)
		repo := store.createRepo(ViewerLogin, repoName, desc, private, "")
		return mcpCallOut{result: mcpToolResult(makeRepoJSON(repo), false)}
	case "get_repo":
		repo, errMsg := mcpRepoFor(args, store)
		if errMsg != "" {
			return fail(errMsg)
		}
		return mcpCallOut{result: mcpToolResult(makeRepoJSON(repo), false)}
	case "create_issue":
		repo, errMsg := mcpRepoFor(args, store)
		if errMsg != "" {
			return fail(errMsg)
		}
		title := toStr(mcpPick(args, "title"))
		if title == "" {
			return fail("Missing required argument: title")
		}
		now := nowIso()
		issue := &Issue{
			Number:    repo.NextNumber,
			Title:     title,
			Body:      toStr(mcpPick(args, "body", "description")),
			State:     "open",
			Labels:    []*Label{},
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
		return mcpCallOut{result: mcpToolResult(makeIssueJSON(repo, issue), false)}
	case "list_issues":
		repo, errMsg := mcpRepoFor(args, store)
		if errMsg != "" {
			return fail(errMsg)
		}
		state := toStr(mcpPick(args, "state"))
		if state == "" {
			state = "open"
		}
		var list []issueJSON
		for _, i := range repo.Issues {
			if state == "all" || i.State == state {
				list = append(list, makeIssueJSON(repo, i))
			}
		}
		if list == nil {
			list = []issueJSON{}
		}
		return mcpCallOut{result: mcpToolResult(map[string]any{"issues": list}, false)}
	case "create_pull_request":
		repo, errMsg := mcpRepoFor(args, store)
		if errMsg != "" {
			return fail(errMsg)
		}
		title := toStr(mcpPick(args, "title"))
		head := toStr(mcpPick(args, "head", "head_ref", "head_branch"))
		base := toStr(mcpPick(args, "base", "base_ref", "base_branch"))
		if title == "" {
			return fail("Missing required argument: title")
		}
		if head == "" {
			return fail("Missing required argument: head")
		}
		if base == "" {
			return fail("Missing required argument: base")
		}
		headSha := branchTip(repo, head)
		baseSha := branchTip(repo, base)
		if headSha == "" || baseSha == "" {
			return fail("head or base branch does not exist: " + head + " / " + base)
		}
		now := nowIso()
		pr := &PullRequest{
			Number:    repo.NextNumber,
			Title:     title,
			Body:      toStr(mcpPick(args, "body", "description")),
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
		return mcpCallOut{result: mcpToolResult(makePRJSON(repo, pr), false)}
	case "merge_pull_request":
		repo, errMsg := mcpRepoFor(args, store)
		if errMsg != "" {
			return fail(errMsg)
		}
		numVal := mcpPick(args, "pull_number", "pullNumber", "number", "pr_number")
		if numVal == nil {
			return fail("Missing required argument: pull_number")
		}
		num := toInt(numVal)
		var pr *PullRequest
		for _, p := range repo.Pulls {
			if p.Number == num {
				pr = p
				break
			}
		}
		if pr == nil {
			return fail("Pull request not found: #" + itoa(num))
		}
		if pr.State != "open" {
			return fail("Pull request is already closed")
		}
		result := squashMerge(repo, pr)
		if result.conflict {
			return fail("Merge conflict")
		}
		pr.State = "closed"
		pr.Merged = true
		pr.MergedAt = nowIso()
		pr.MergeCommitSha = result.sha
		pr.UpdatedAt = nowIso()
		return mcpCallOut{result: mcpToolResult(map[string]any{
			"sha": result.sha, "merged": true, "message": result.message,
			"pull_request": makePRJSON(repo, pr),
		}, false)}
	case "list_repos":
		owner := toStr(mcpPick(args, "owner"))
		if owner == "" {
			owner = ViewerLogin
		}
		repos := store.listReposFor(owner)
		var list []repoJSON
		for _, r := range repos {
			list = append(list, makeRepoJSON(r))
		}
		if list == nil {
			list = []repoJSON{}
		}
		return mcpCallOut{result: mcpToolResult(map[string]any{"repositories": list}, false)}
	case "get_issue":
		repo, errMsg := mcpRepoFor(args, store)
		if errMsg != "" {
			return fail(errMsg)
		}
		numVal := mcpPick(args, "issue_number", "issueNumber", "number")
		if numVal == nil {
			return fail("Missing required argument: issue_number")
		}
		num := toInt(numVal)
		var issue *Issue
		for _, i := range repo.Issues {
			if i.Number == num {
				issue = i
				break
			}
		}
		if issue == nil {
			return fail("Issue not found: #" + itoa(num))
		}
		return mcpCallOut{result: mcpToolResult(makeIssueJSON(repo, issue), false)}
	default:
		return mcpCallOut{rpcError: &mcpErrorInfo{code: -32602, message: "Unknown tool: " + name}}
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC dispatch
// ---------------------------------------------------------------------------

type mcpResult struct {
	status  int
	body    any
	headers map[string]string
}

func mcpRPCResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func mcpRPCError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func mcpDiscoverResult() map[string]any {
	return map[string]any{
		"protocolVersion": mcpCurrent,
		"supportedVersions": append([]string(nil), mcpSupportedVersions...),
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"serverInfo":        mcpServerInfo(),
		"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": mcpServerInfo()},
	}
}

// handleMcp serves one MCP request. It takes the store write lock for the
// whole handler (tools write; MCP is conformance-only, never a timed
// workload, and two concurrent tools/call requests serialize briefly).
func handleMcp(store *Store, rawBody []byte, headers map[string]string) mcpResult {
	store.mu.Lock()
	defer store.mu.Unlock()
	protocolHeader := headers["mcp-protocol-version"]
	isModern := protocolHeader != "" && protocolHeader >= "2026-07-28"
	var parsed any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		// Non-JSON body: HTTP 400 with -32700 in the modern era, 200 before it.
		body := mcpRPCError(nil, -32700, "Parse error: request body is not valid JSON")
		if isModern {
			return mcpResult{status: 400, body: body}
		}
		return mcpResult{status: 200, body: body}
	}
	req, ok := parsed.(map[string]any)
	if !ok {
		return mcpResult{status: 400, body: mcpRPCError(nil, -32600, "Invalid Request")}
	}
	id, _ := req["id"]
	method, _ := req["method"].(string)
	params, _ := req["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	// Notifications (no id): acknowledge, no JSON-RPC response body.
	if id == nil {
		return mcpResult{status: 202, body: ""}
	}

	switch method {
	case "server/discover":
		return mcpResult{status: 200, body: mcpRPCResult(id, mcpDiscoverResult())}
	case "initialize":
		offered, _ := params["protocolVersion"].(string)
		chosen := "2025-11-25"
		for _, v := range mcpSupportedVersions {
			if v == offered {
				chosen = v
				break
			}
		}
		return mcpResult{
			status: 200,
			body: mcpRPCResult(id, map[string]any{
				"protocolVersion": chosen,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      mcpServerInfo(),
			}),
			headers: map[string]string{"Mcp-Session-Id": "forge-session-1"},
		}
	case "ping":
		return mcpResult{status: 200, body: mcpRPCResult(id, map[string]any{})}
	case "tools/list":
		// One frozen list: byte-identical on every call.
		tools := make([]any, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools = append(tools, t.toJSON())
		}
		return mcpResult{status: 200, body: mcpRPCResult(id, map[string]any{"tools": tools})}
	case "tools/call":
		toolName, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		if toolName == "" {
			return mcpResult{status: 200, body: mcpRPCError(id, -32602, "Missing required argument: name")}
		}
		known := false
		for _, t := range mcpTools {
			if t.name == toolName {
				known = true
				break
			}
		}
		if !known {
			return mcpResult{status: 200, body: mcpRPCError(id, -32602, "Unknown tool: "+toolName)}
		}
		out := mcpCallTool(store, toolName, args)
		if out.rpcError != nil {
			return mcpResult{status: 200, body: mcpRPCError(id, out.rpcError.code, out.rpcError.message)}
		}
		return mcpResult{status: 200, body: mcpRPCResult(id, out.result)}
	default:
		return mcpResult{status: 200, body: mcpRPCError(id, -32601, "Method not found: "+method)}
	}
}
