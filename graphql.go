// graphql.go — GitHub GraphQL v4 surface at /api/graphql. Port of graphql.ts.
//
// A small, purpose-built parser + executor for the documents gh and the
// suite send (queries, mutations, variables, aliases, inline fragments,
// __type introspection, operation batching). It is not a general GraphQL
// server: unknown fields are reported as errors with HTTP 200, unknown
// repos resolve to data.repository = null with no errors, and two
// operation documents in one JSON array body answer two results in order.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Global ids: opaque, stable across reads (base64 of a typed key).
// ---------------------------------------------------------------------------

func gidRepo(repo *Repo) string {
	return base64.StdEncoding.EncodeToString([]byte("R:" + itoa(repo.ID)))
}

func gidIssue(repo *Repo, n int) string {
	return base64.StdEncoding.EncodeToString([]byte("I:" + itoa(repo.ID) + ":" + itoa(n)))
}

func gidPull(repo *Repo, n int) string {
	return base64.StdEncoding.EncodeToString([]byte("P:" + itoa(repo.ID) + ":" + itoa(n)))
}

func gidComment(repo *Repo, n int, commentID int) string {
	return base64.StdEncoding.EncodeToString([]byte("C:" + itoa(repo.ID) + ":" + itoa(n) + ":" + itoa(commentID)))
}

type decodedGid struct {
	kind   string
	repoID int
	number int
	extra  int
}

func decodeGid(id string) *decodedGid {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return nil
	}
	repoID, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil
	}
	d := &decodedGid{kind: parts[0], repoID: repoID}
	if len(parts) > 2 {
		d.number, _ = strconv.Atoi(parts[2])
	}
	if len(parts) > 3 {
		d.extra, _ = strconv.Atoi(parts[3])
	}
	return d
}

// ---------------------------------------------------------------------------
// Tokenizer + parser
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tokName tokenKind = iota
	tokString
	tokInt
	tokFloat
	tokPunct
	tokSpread
)

type token struct {
	kind  tokenKind
	value string
}

func tokenize(src string) ([]token, error) {
	var tokens []token
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+2 < len(src) && src[i:i+3] == "..." {
			tokens = append(tokens, token{kind: tokSpread})
			i += 3
			continue
		}
		if strings.ContainsRune("{}()[]:!=$@", rune(c)) {
			tokens = append(tokens, token{kind: tokPunct, value: string(c)})
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					esc := src[j+1]
					switch esc {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					default:
						sb.WriteByte(esc)
					}
					j += 2
				} else {
					sb.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, token{kind: tokString, value: sb.String()})
			i = j + 1
			continue
		}
		if (c >= '0' && c <= '9') || c == '-' {
			j := i
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
				j++
			}
			text := src[i:j]
			if strings.Contains(text, ".") {
				tokens = append(tokens, token{kind: tokFloat, value: text})
			} else {
				tokens = append(tokens, token{kind: tokInt, value: text})
			}
			i = j
			continue
		}
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			j := i
			for j < len(src) && ((src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			tokens = append(tokens, token{kind: tokName, value: src[i:j]})
			i = j
			continue
		}
		return nil, fmt.Errorf("unexpected character %q in GraphQL document", string(c))
	}
	return tokens, nil
}

type valueKind int

const (
	valVariable valueKind = iota
	valString
	valInt
	valFloat
	valBoolean
	valNull
	valEnum
	valList
	valObject
)

type valueNode struct {
	kind   valueKind
	str    string
	num    float64
	b      bool
	values []*valueNode
	fields []valueField
	name   string // variable name
}

type valueField struct {
	name  string
	value *valueNode
}

type fieldNode struct {
	alias     string
	name      string
	args      []argNode
	selection []selectionNode
}

type argNode struct {
	name  string
	value *valueNode
}

type fragmentNode struct {
	typeCondition string
	selection     []selectionNode
}

type selectionNode struct {
	isFragment bool
	field      *fieldNode
	fragment   *fragmentNode
}

type operationNode struct {
	kind      string // "query" | "mutation"
	name      string
	variables []varDef
	selection []selectionNode
}

type varDef struct {
	name    string
	def     *valueNode
	hasDef  bool
}

type gqlParser struct {
	pos    int
	tokens []token
}

func (p *gqlParser) peek() *token {
	if p.pos >= len(p.tokens) {
		return nil
	}
	return &p.tokens[p.pos]
}

func (p *gqlParser) next() (token, error) {
	if p.pos >= len(p.tokens) {
		return token{}, fmt.Errorf("unexpected end of GraphQL document")
	}
	t := p.tokens[p.pos]
	p.pos++
	return t, nil
}

func (p *gqlParser) expectPunct(v string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != tokPunct || t.value != v {
		return fmt.Errorf("expected %s in GraphQL document", v)
	}
	return nil
}

func (p *gqlParser) isPunct(v string) bool {
	t := p.peek()
	return t != nil && t.kind == tokPunct && t.value == v
}

func (p *gqlParser) expectName() (string, error) {
	t, err := p.next()
	if err != nil {
		return "", err
	}
	if t.kind != tokName {
		return "", fmt.Errorf("expected a name in GraphQL document")
	}
	return t.value, nil
}

func (p *gqlParser) parseDocument() ([]*operationNode, error) {
	var ops []*operationNode
	for p.pos < len(p.tokens) {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *gqlParser) parseOperation() (*operationNode, error) {
	kind := "query"
	var name string
	if t := p.peek(); t != nil && t.kind == tokName && (t.value == "query" || t.value == "mutation") {
		kind = t.value
		p.pos++
		if n := p.peek(); n != nil && n.kind == tokName {
			name = n.value
			p.pos++
		}
	}
	var variables []varDef
	if p.isPunct("(") {
		p.pos++
		for !p.isPunct(")") {
			if err := p.expectPunct("$"); err != nil {
				return nil, err
			}
			vname, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			if err := p.parseType(); err != nil {
				return nil, err
			}
			vd := varDef{name: vname}
			if p.isPunct("=") {
				p.pos++
				def, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				vd.def = def
				vd.hasDef = true
			}
			variables = append(variables, vd)
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	// directives (ignored)
	for p.isPunct("@") {
		p.pos++
		if _, err := p.expectName(); err != nil {
			return nil, err
		}
		if p.isPunct("(") {
			if _, err := p.parseArguments(); err != nil {
				return nil, err
			}
		}
	}
	selection, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &operationNode{kind: kind, name: name, variables: variables, selection: selection}, nil
}

func (p *gqlParser) parseType() error {
	if p.isPunct("[") {
		p.pos++
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(); err != nil {
			return err
		}
	}
	if p.isPunct("!") {
		p.pos++
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]selectionNode, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []selectionNode
	for !p.isPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *gqlParser) parseSelection() (selectionNode, error) {
	if t := p.peek(); t != nil && t.kind == tokSpread {
		p.pos++
		if n := p.peek(); n != nil && n.kind == tokName && n.value == "on" {
			p.pos++
			tc, err := p.expectName()
			if err != nil {
				return selectionNode{}, err
			}
			for p.isPunct("@") {
				p.pos++
				if _, err := p.expectName(); err != nil {
					return selectionNode{}, err
				}
				if p.isPunct("(") {
					if _, err := p.parseArguments(); err != nil {
						return selectionNode{}, err
					}
				}
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return selectionNode{}, err
			}
			return selectionNode{isFragment: true, fragment: &fragmentNode{typeCondition: tc, selection: sel}}, nil
		}
		// named fragment spread (not used by the suite); skip its selections
		if _, err := p.expectName(); err != nil {
			return selectionNode{}, err
		}
		return selectionNode{isFragment: true, fragment: &fragmentNode{selection: nil}}, nil
	}
	name, err := p.expectName()
	if err != nil {
		return selectionNode{}, err
	}
	alias := ""
	if p.isPunct(":") {
		p.pos++
		alias = name
		name, err = p.expectName()
		if err != nil {
			return selectionNode{}, err
		}
	}
	var args []argNode
	if p.isPunct("(") {
		args, err = p.parseArguments()
		if err != nil {
			return selectionNode{}, err
		}
	}
	for p.isPunct("@") {
		p.pos++
		if _, err := p.expectName(); err != nil {
			return selectionNode{}, err
		}
		if p.isPunct("(") {
			if _, err := p.parseArguments(); err != nil {
				return selectionNode{}, err
			}
		}
	}
	f := &fieldNode{alias: alias, name: name, args: args}
	if p.isPunct("{") {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return selectionNode{}, err
		}
		f.selection = sel
	}
	return selectionNode{field: f}, nil
}

func (p *gqlParser) parseArguments() ([]argNode, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var args []argNode
	for !p.isPunct(")") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args = append(args, argNode{name: name, value: v})
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue() (*valueNode, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	if t.kind == tokPunct && t.value == "$" {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &valueNode{kind: valVariable, name: name}, nil
	}
	if t.kind == tokPunct && t.value == "[" {
		var values []*valueNode
		for {
			nt := p.peek()
			if nt != nil && nt.kind == tokPunct && nt.value == "]" {
				break
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		if err := p.expectPunct("]"); err != nil {
			return nil, err
		}
		return &valueNode{kind: valList, values: values}, nil
	}
	if t.kind == tokPunct && t.value == "{" {
		var fields []valueField
		for {
			nt := p.peek()
			if nt != nil && nt.kind == tokPunct && nt.value == "}" {
				break
			}
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			fields = append(fields, valueField{name: name, value: v})
		}
		if err := p.expectPunct("}"); err != nil {
			return nil, err
		}
		return &valueNode{kind: valObject, fields: fields}, nil
	}
	if t.kind == tokString {
		return &valueNode{kind: valString, str: t.value}, nil
	}
	if t.kind == tokInt {
		n, _ := strconv.ParseFloat(t.value, 64)
		return &valueNode{kind: valInt, num: n}, nil
	}
	if t.kind == tokFloat {
		n, _ := strconv.ParseFloat(t.value, 64)
		return &valueNode{kind: valFloat, num: n}, nil
	}
	if t.kind == tokName {
		switch t.value {
		case "true":
			return &valueNode{kind: valBoolean, b: true}, nil
		case "false":
			return &valueNode{kind: valBoolean, b: false}, nil
		case "null":
			return &valueNode{kind: valNull}, nil
		default:
			return &valueNode{kind: valEnum, str: t.value}, nil
		}
	}
	return nil, fmt.Errorf("unexpected value in GraphQL document")
}

func parseGraphQL(src string) ([]*operationNode, error) {
	tokens, err := tokenize(src)
	if err != nil {
		return nil, err
	}
	return (&gqlParser{tokens: tokens}).parseDocument()
}

// ---------------------------------------------------------------------------
// Schema (for validation + introspection)
// ---------------------------------------------------------------------------

type typeInfo struct {
	kind       string // "OBJECT" | "ENUM"
	fields     []string
	enumValues []string
}

var gqlSchema = map[string]typeInfo{
	"Query":                 {kind: "OBJECT", fields: []string{"viewer", "repository", "node", "__type", "__schema"}},
	"Node":                  {kind: "OBJECT", fields: []string{"id"}},
	"Mutation":              {kind: "OBJECT", fields: []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"}},
	"User":                  {kind: "OBJECT", fields: []string{"id", "login", "name", "email"}},
	"Repository":            {kind: "OBJECT", fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}},
	"Issue":                 {kind: "OBJECT", fields: []string{"id", "number", "title", "body", "state", "createdAt", "updatedAt", "closedAt", "author", "comments"}},
	"PullRequest":           {kind: "OBJECT", fields: []string{"id", "number", "title", "body", "state", "merged", "mergedAt", "headRefName", "baseRefName", "mergeable", "files", "createdAt", "updatedAt"}},
	"Ref":                   {kind: "OBJECT", fields: []string{"name"}},
	"IssueConnection":       {kind: "OBJECT", fields: []string{"nodes", "edges", "pageInfo", "totalCount"}},
	"IssueCommentConnection": {kind: "OBJECT", fields: []string{"nodes", "edges", "pageInfo", "totalCount"}},
	"IssueCommentEdge":      {kind: "OBJECT", fields: []string{"node", "cursor"}},
	"PullRequestConnection": {kind: "OBJECT", fields: []string{"nodes", "edges", "pageInfo", "totalCount"}},
	"PullRequestFileConnection": {kind: "OBJECT", fields: []string{"nodes", "edges", "pageInfo", "totalCount"}},
	"IssueEdge":             {kind: "OBJECT", fields: []string{"node", "cursor"}},
	"PullRequestEdge":       {kind: "OBJECT", fields: []string{"node", "cursor"}},
	"PullRequestFileEdge":   {kind: "OBJECT", fields: []string{"node", "cursor"}},
	"PageInfo":              {kind: "OBJECT", fields: []string{"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"}},
	"PullRequestFile":       {kind: "OBJECT", fields: []string{"path", "additions", "deletions", "changeType"}},
	"Comment":               {kind: "OBJECT", fields: []string{"id", "body", "createdAt", "updatedAt", "author"}},
	"AddedCommentEdge":      {kind: "OBJECT", fields: []string{"node", "cursor"}},
	"CreateIssuePayload":    {kind: "OBJECT", fields: []string{"issue", "clientMutationId"}},
	"CloseIssuePayload":     {kind: "OBJECT", fields: []string{"issue", "clientMutationId"}},
	"CreatePullRequestPayload": {kind: "OBJECT", fields: []string{"pullRequest", "clientMutationId"}},
	"MergePullRequestPayload":  {kind: "OBJECT", fields: []string{"pullRequest", "clientMutationId"}},
	"AddCommentPayload":        {kind: "OBJECT", fields: []string{"commentEdge", "clientMutationId"}},
	"__Type":                {kind: "OBJECT", fields: []string{"name", "kind", "fields", "enumValues"}},
	"__Field":               {kind: "OBJECT", fields: []string{"name"}},
	"__EnumValue":           {kind: "OBJECT", fields: []string{"name"}},
	"__Schema":              {kind: "OBJECT", fields: []string{"queryType", "mutationType", "types"}},
	"SearchType":            {kind: "ENUM", enumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"}},
	"IssueState":            {kind: "ENUM", enumValues: []string{"OPEN", "CLOSED"}},
	"IssueOrderField":       {kind: "ENUM", enumValues: []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}},
	"OrderDirection":        {kind: "ENUM", enumValues: []string{"ASC", "DESC"}},
	"PullRequestState":      {kind: "ENUM", enumValues: []string{"OPEN", "CLOSED", "MERGED"}},
	"MergeableState":        {kind: "ENUM", enumValues: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
	"MergeMethod":           {kind: "ENUM", enumValues: []string{"MERGE", "SQUASH", "REBASE"}},
}

var gqlFieldType = map[string]map[string]string{
	"Query": {"viewer": "User", "repository": "Repository", "node": "Node", "__type": "__Type", "__schema": "__Schema"},
	"Issue": {"comments": "IssueCommentConnection"},
	"IssueCommentConnection": {"nodes": "Comment", "edges": "IssueCommentEdge", "pageInfo": "PageInfo"},
	"IssueCommentEdge": {"node": "Comment"},
	"Repository": {
		"owner": "User", "defaultBranchRef": "Ref", "issue": "Issue",
		"issues": "IssueConnection", "pullRequests": "PullRequestConnection", "pullRequest": "PullRequest",
	},
	"IssueConnection": {"nodes": "Issue", "edges": "IssueEdge", "pageInfo": "PageInfo"},
	"PullRequestConnection": {"nodes": "PullRequest", "edges": "PullRequestEdge", "pageInfo": "PageInfo"},
	"PullRequestFileConnection": {"nodes": "PullRequestFile", "edges": "PullRequestFileEdge", "pageInfo": "PageInfo"},
	"IssueEdge": {"node": "Issue"},
	"PullRequestEdge": {"node": "PullRequest"},
	"PullRequestFileEdge": {"node": "PullRequestFile"},
	"PullRequest": {"files": "PullRequestFileConnection"},
	"Mutation": {
		"createIssue": "CreateIssuePayload", "closeIssue": "CloseIssuePayload",
		"createPullRequest": "CreatePullRequestPayload", "mergePullRequest": "MergePullRequestPayload",
		"addComment": "AddCommentPayload",
	},
	"CreateIssuePayload": {"issue": "Issue"},
	"CloseIssuePayload": {"issue": "Issue"},
	"CreatePullRequestPayload": {"pullRequest": "PullRequest"},
	"MergePullRequestPayload": {"pullRequest": "PullRequest"},
	"AddCommentPayload": {"commentEdge": "AddedCommentEdge"},
	"AddedCommentEdge": {"node": "Comment"},
	"__Type": {"fields": "__Field", "enumValues": "__EnumValue"},
	"__Schema": {"queryType": "__Type", "mutationType": "__Type", "types": "__Type"},
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

type gqlCtx struct {
	store     *Store
	variables map[string]any
	errors    []map[string]any
}

// gqlObj is the resolved value of an object; its GraphQL type rides in
// "__typename" and the backing store objects in "__repo"/"__issue"/"__pr".
type gqlObj = map[string]any

func resolveValue(node *valueNode, ctx *gqlCtx) any {
	switch node.kind {
	case valVariable:
		if v, ok := ctx.variables[node.name]; ok {
			return v
		}
		return nil
	case valString:
		return node.str
	case valInt:
		return int(node.num)
	case valFloat:
		return node.num
	case valBoolean:
		return node.b
	case valNull:
		return nil
	case valEnum:
		return node.str
	case valList:
		out := make([]any, 0, len(node.values))
		for _, v := range node.values {
			out = append(out, resolveValue(v, ctx))
		}
		return out
	case valObject:
		out := make(map[string]any)
		for _, f := range node.fields {
			out[f.name] = resolveValue(f.value, ctx)
		}
		return out
	}
	return nil
}

func argsOf(field *fieldNode, ctx *gqlCtx) map[string]any {
	out := make(map[string]any)
	for _, a := range field.args {
		out[a.name] = resolveValue(a.value, ctx)
	}
	return out
}

func cursorFor(seq int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + itoa(seq)))
}

func seqFromCursor(cursor string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(text[7:])
	if err != nil {
		return 0, false
	}
	return n, true
}

type pageArgs struct {
	first    int
	hasFirst bool
	after    string
}

type connItem struct {
	node   any
	cursor string
}

func gqlConnection(items []connItem, args pageArgs) map[string]any {
	total := len(items)
	start := 0
	if args.after != "" {
		if _, ok := seqFromCursor(args.after); ok {
			for i, it := range items {
				if it.cursor == args.after {
					start = i + 1
					break
				}
			}
		}
	}
	first := total
	if args.hasFirst {
		first = args.first
	}
	if start > total {
		start = total
	}
	end := start + first
	if end > total {
		end = total
	}
	slice := items[start:end]
	var endCursor any
	var startCursor any
	if len(slice) > 0 {
		endCursor = slice[len(slice)-1].cursor
		startCursor = slice[0].cursor
	}
	nodes := make([]any, 0, len(slice))
	edges := make([]any, 0, len(slice))
	for _, it := range slice {
		nodes = append(nodes, it.node)
		edges = append(edges, map[string]any{"node": it.node, "cursor": it.cursor})
	}
	return map[string]any{
		"nodes": nodes,
		"edges": edges,
		"totalCount": total,
		"pageInfo": map[string]any{
			"hasNextPage":     start+len(slice) < total,
			"hasPreviousPage": start > 0,
			"startCursor":     startCursor,
			"endCursor":       endCursor,
		},
	}
}

func issueStateOut(state string) string {
	if state == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func prStateOut(p *PullRequest) string {
	if p.Merged {
		return "MERGED"
	}
	if p.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func mergeableOf(repo *Repo, pr *PullRequest) string {
	baseTip := branchTip(repo, pr.BaseRef)
	headTip := branchTip(repo, pr.HeadRef)
	if baseTip == "" || headTip == "" {
		return "UNKNOWN"
	}
	base := mergeBase(repo, baseTip, headTip)
	if base == "" {
		return "MERGEABLE"
	}
	baseTree := parseCommit(repo.Objects[base].Data).Tree
	headTree := parseCommit(repo.Objects[headTip].Data).Tree
	baseCommitTree := parseCommit(repo.Objects[baseTip].Data).Tree
	headChanges := diffTrees(repo, baseTree, headTree)
	baseChanges := diffTrees(repo, baseTree, baseCommitTree)
	baseMap := make(map[string]fileChange)
	for _, c := range baseChanges {
		baseMap[c.Filename] = c
	}
	headFiles := flattenTree(repo, headTree, "", nil)
	baseFiles := flattenTree(repo, baseCommitTree, "", nil)
	for _, change := range headChanges {
		if _, ok := baseMap[change.Filename]; !ok {
			continue
		}
		h, hasH := headFiles[change.Filename]
		b, hasB := baseFiles[change.Filename]
		if !hasH || !hasB || h.sha != b.sha {
			return "CONFLICTING"
		}
	}
	return "MERGEABLE"
}

func prFiles(repo *Repo, pr *PullRequest) []fileChange {
	baseTip := branchTip(repo, pr.BaseRef)
	headTip := branchTip(repo, pr.HeadRef)
	if baseTip == "" || headTip == "" {
		return nil
	}
	base := mergeBase(repo, baseTip, headTip)
	headTree := parseCommit(repo.Objects[headTip].Data).Tree
	if base == "" {
		return diffTrees(repo, "", headTree)
	}
	baseTree := parseCommit(repo.Objects[base].Data).Tree
	return diffTrees(repo, baseTree, headTree)
}

func repoObj(repo *Repo) gqlObj {
	return gqlObj{
		"__typename": "Repository",
		"__repo":     repo,
		"id":         gidRepo(repo),
		"databaseId": repo.ID,
		"name":       repo.Name,
		"nameWithOwner": repo.Owner + "/" + repo.Name,
		"owner":         gqlObj{"__typename": "User", "login": repo.Owner, "id": ViewerID},
		"viewerPermission": "ADMIN",
		"hasIssuesEnabled": true,
	}
}

func issueObj(repo *Repo, issue *Issue) gqlObj {
	return gqlObj{
		"__typename": "Issue",
		"__repo":     repo,
		"__issue":    issue,
		"id":         gidIssue(repo, issue.Number),
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issueStateOut(issue.State),
		"createdAt":  issue.CreatedAt,
		"updatedAt":  issue.UpdatedAt,
		"closedAt":   strOrNil(issue.ClosedAt),
		"author":     gqlObj{"__typename": "User", "login": issue.UserLogin, "id": issue.UserID},
	}
}

func prObj(repo *Repo, pr *PullRequest) gqlObj {
	return gqlObj{
		"__typename": "PullRequest",
		"__repo":     repo,
		"__pr":       pr,
		"id":         gidPull(repo, pr.Number),
		"number":     pr.Number,
		"title":      pr.Title,
		"body":       pr.Body,
		"state":      prStateOut(pr),
		"merged":     pr.Merged,
		"mergedAt":   strOrNil(pr.MergedAt),
		"headRefName": pr.HeadRef,
		"baseRefName": pr.BaseRef,
	}
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// Field resolution
// ---------------------------------------------------------------------------

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case int64:
		return int(n)
	}
	return 0
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func resolveField(parentType string, parent gqlObj, field *fieldNode, ctx *gqlCtx) any {
	args := argsOf(field, ctx)
	// __typename is valid on every object type.
	if field.name == "__typename" {
		if t, ok := parent["__typename"].(string); ok {
			return t
		}
		return parentType
	}

	switch parentType {
	case "Query":
		switch field.name {
		case "viewer":
			return gqlObj{"__typename": "User", "id": ViewerID, "login": ViewerLogin, "name": "ada iversen", "email": "adaiversen@example.invalid"}
		case "repository":
			owner := toStr(args["owner"])
			name := toStr(args["name"])
			if owner == "" || name == "" {
				return nil
			}
			repo := ctx.store.getRepo(owner, name)
			if repo == nil {
				return nil
			}
			return repoObj(repo)
		case "node":
			decoded := decodeGid(toStr(args["id"]))
			if decoded == nil {
				return nil
			}
			repo := ctx.store.repoByID(decoded.repoID)
			if repo == nil {
				return nil
			}
			switch decoded.kind {
			case "R":
				return repoObj(repo)
			case "I":
				for _, issue := range repo.Issues {
					if issue.Number == decoded.number {
						return issueObj(repo, issue)
					}
				}
				return nil
			case "P":
				for _, pr := range repo.Pulls {
					if pr.Number == decoded.number {
						return prObj(repo, pr)
					}
				}
				return nil
			}
			return nil
		case "__type":
			info, ok := gqlSchema[toStr(args["name"])]
			if !ok {
				return nil
			}
			return gqlObj{
				"__typename": "__Type",
				"name":       args["name"],
				"kind":       info.kind,
				"__fields":   info.fields,
				"__enumValues": info.enumValues,
			}
		case "__schema":
			return gqlObj{"__typename": "__Schema", "__queryType": "Query", "__mutationType": "Mutation"}
		}
		return nil
	case "Repository":
		repo := parent["__repo"].(*Repo)
		switch field.name {
		case "defaultBranchRef":
			if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; !ok {
				return nil
			}
			return gqlObj{"__typename": "Ref", "name": repo.DefaultBranch}
		case "issue":
			num := toInt(args["number"])
			for _, issue := range repo.Issues {
				if issue.Number == num {
					return issueObj(repo, issue)
				}
			}
			return nil
		case "issues":
			list := append([]*Issue(nil), repo.Issues...)
			if states, ok := args["states"].([]any); ok && len(states) > 0 {
				want := make(map[string]bool)
				for _, s := range states {
					want[toStr(s)] = true
				}
				var filtered []*Issue
				for _, i := range list {
					if want[issueStateOut(i.State)] {
						filtered = append(filtered, i)
					}
				}
				list = filtered
			}
			dir := "DESC"
			if orderBy, ok := args["orderBy"].(map[string]any); ok {
				if d, ok := orderBy["direction"].(string); ok && d != "" {
					dir = d
				}
			}
			sortIssuesBySeq(list, dir == "ASC")
			pa := pageArgsFrom(args)
			var items []connItem
			for _, i := range list {
				items = append(items, connItem{node: issueObj(repo, i), cursor: cursorFor(i.Seq)})
			}
			conn := gqlConnection(items, pa)
			conn["__typename"] = "IssueConnection"
			return conn
		case "pullRequests":
			list := append([]*PullRequest(nil), repo.Pulls...)
			sort.Slice(list, func(a, b int) bool { return list[a].Seq > list[b].Seq })
			pa := pageArgsFrom(args)
			var items []connItem
			for _, p := range list {
				items = append(items, connItem{node: prObj(repo, p), cursor: cursorFor(p.Seq)})
			}
			conn := gqlConnection(items, pa)
			conn["__typename"] = "PullRequestConnection"
			return conn
		case "pullRequest":
			num := toInt(args["number"])
			for _, pr := range repo.Pulls {
				if pr.Number == num {
					return prObj(repo, pr)
				}
			}
			return nil
		}
		if v, ok := parent[field.name]; ok {
			return v
		}
		return nil
	case "PullRequest":
		repo := parent["__repo"].(*Repo)
		pr := parent["__pr"].(*PullRequest)
		switch field.name {
		case "mergeable":
			return mergeableOf(repo, pr)
		case "files":
			changes := prFiles(repo, pr)
			pa := pageArgsFrom(args)
			var items []connItem
			for i, c := range changes {
				ct := "MODIFIED"
				if c.Status == "added" {
					ct = "ADDED"
				}
				items = append(items, connItem{
					node: gqlObj{
						"__typename": "PullRequestFile",
						"path":       c.Filename,
						"additions":  c.Additions,
						"deletions":  c.Deletions,
						"changeType": ct,
					},
					cursor: cursorFor(i + 1),
				})
			}
			conn := gqlConnection(items, pa)
			conn["__typename"] = "PullRequestFileConnection"
			return conn
		}
		if v, ok := parent[field.name]; ok {
			return v
		}
		return nil
	case "Issue":
		if field.name == "comments" {
			issue := parent["__issue"].(*Issue)
			repo := parent["__repo"].(*Repo)
			pa := pageArgsFrom(args)
			// The connection's edges keep the raw comment (as the Node port
			// does); only `nodes` are mapped to Comment objects.
			var rawItems []connItem
			for _, c := range issue.Comments {
				rawItems = append(rawItems, connItem{
					node: map[string]any{
						"id": c.ID, "body": c.Body,
						"created_at": c.CreatedAt, "updated_at": c.UpdatedAt,
						"user": map[string]any{"login": c.UserLogin, "id": c.UserID},
					},
					cursor: cursorFor(c.ID),
				})
			}
			conn := gqlConnection(rawItems, pa)
			nodes := conn["nodes"].([]any)
			mapped := make([]any, len(nodes))
			for i, raw := range nodes {
				m := raw.(map[string]any)
				id := m["id"].(int)
				mapped[i] = gqlObj{
					"__typename": "Comment",
					"id":         gidComment(repo, issue.Number, id),
					"body":       m["body"],
					"createdAt":  m["created_at"],
					"updatedAt":  m["updated_at"],
				}
			}
			conn["nodes"] = mapped
			conn["__typename"] = "IssueCommentConnection"
			return conn
		}
		if v, ok := parent[field.name]; ok {
			return v
		}
		return nil
	case "Mutation":
		return resolveMutation(field, ctx)
	case "__Type":
		switch field.name {
		case "fields":
			fields, _ := parent["__fields"].([]string)
			out := make([]any, 0, len(fields))
			for _, name := range fields {
				out = append(out, gqlObj{"__typename": "__Field", "name": name})
			}
			return out
		case "enumValues":
			vals, _ := parent["__enumValues"].([]string)
			out := make([]any, 0, len(vals))
			for _, name := range vals {
				out = append(out, gqlObj{"__typename": "__EnumValue", "name": name})
			}
			return out
		}
		if v, ok := parent[field.name]; ok {
			return v
		}
		return nil
	case "__Schema":
		switch field.name {
		case "queryType":
			return gqlObj{"__typename": "__Type", "name": "Query", "kind": "OBJECT", "__fields": []string{}, "__enumValues": []string{}}
		case "mutationType":
			return gqlObj{"__typename": "__Type", "name": "Mutation", "kind": "OBJECT", "__fields": []string{}, "__enumValues": []string{}}
		case "types":
			var names []string
			for name := range gqlSchema {
				names = append(names, name)
			}
			sort.Strings(names)
			out := make([]any, 0, len(names))
			for _, name := range names {
				info := gqlSchema[name]
				out = append(out, gqlObj{
					"__typename": "__Type",
					"name":       name,
					"kind":       info.kind,
					"__fields":   info.fields,
					"__enumValues": info.enumValues,
				})
			}
			return out
		}
		if v, ok := parent[field.name]; ok {
			return v
		}
		return nil
	default:
		if v, ok := parent[field.name]; ok {
			return v
		}
		return nil
	}
}

func pageArgsFrom(args map[string]any) pageArgs {
	pa := pageArgs{}
	if f, ok := args["first"]; ok {
		pa.first = toInt(f)
		pa.hasFirst = true
	}
	if a, ok := args["after"].(string); ok {
		pa.after = a
	}
	return pa
}

func sortIssuesBySeq(list []*Issue, asc bool) {
	sort.Slice(list, func(a, b int) bool {
		if asc {
			return list[a].Seq < list[b].Seq
		}
		return list[a].Seq > list[b].Seq
	})
}

func resolveMutation(field *fieldNode, ctx *gqlCtx) any {
	args := argsOf(field, ctx)
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	now := nowIso()

	pushErr := func(msg string) {
		ctx.errors = append(ctx.errors, map[string]any{"message": msg})
	}

	switch field.name {
	case "createIssue":
		decoded := decodeGid(toStr(input["repositoryId"]))
		var repo *Repo
		if decoded != nil && decoded.kind == "R" {
			repo = ctx.store.repoByID(decoded.repoID)
		}
		if repo == nil {
			pushErr("Could not resolve to a Repository with the id provided.")
			return gqlObj{"__typename": "CreateIssuePayload", "issue": nil}
		}
		issue := &Issue{
			Number:    repo.NextNumber,
			Title:     toStr(input["title"]),
			Body:      toStr(input["body"]),
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
		return gqlObj{"__typename": "CreateIssuePayload", "issue": issueObj(repo, issue)}
	case "closeIssue":
		decoded := decodeGid(toStr(input["issueId"]))
		var repo *Repo
		var issue *Issue
		if decoded != nil && decoded.kind == "I" {
			repo = ctx.store.repoByID(decoded.repoID)
			if repo != nil {
				for _, i := range repo.Issues {
					if i.Number == decoded.number {
						issue = i
						break
					}
				}
			}
		}
		if repo == nil || issue == nil {
			pushErr("Could not resolve to an Issue with the id provided.")
			return gqlObj{"__typename": "CloseIssuePayload", "issue": nil}
		}
		issue.State = "closed"
		issue.ClosedAt = now
		issue.UpdatedAt = now
		return gqlObj{"__typename": "CloseIssuePayload", "issue": issueObj(repo, issue)}
	case "createPullRequest":
		decoded := decodeGid(toStr(input["repositoryId"]))
		var repo *Repo
		if decoded != nil && decoded.kind == "R" {
			repo = ctx.store.repoByID(decoded.repoID)
		}
		baseRef := toStr(input["baseRefName"])
		headRef := toStr(input["headRefName"])
		var headSha, baseSha string
		if repo != nil {
			headSha = branchTip(repo, headRef)
			baseSha = branchTip(repo, baseRef)
		}
		if repo == nil || headSha == "" || baseSha == "" {
			pushErr("Could not resolve to a repository or branch with the input provided.")
			return gqlObj{"__typename": "CreatePullRequestPayload", "pullRequest": nil}
		}
		pr := &PullRequest{
			Number:    repo.NextNumber,
			Title:     toStr(input["title"]),
			Body:      toStr(input["body"]),
			State:     "open",
			Merged:    false,
			MergedAt:  "",
			HeadRef:   headRef,
			HeadSha:   headSha,
			BaseRef:   baseRef,
			BaseSha:   baseSha,
			CreatedAt: now,
			UpdatedAt: now,
			Comments:  []*Comment{},
		}
		repo.NextNumber++
		repo.Seq++
		pr.Seq = repo.Seq
		repo.Pulls = append(repo.Pulls, pr)
		return gqlObj{"__typename": "CreatePullRequestPayload", "pullRequest": prObj(repo, pr)}
	case "mergePullRequest":
		decoded := decodeGid(toStr(input["pullRequestId"]))
		var repo *Repo
		var pr *PullRequest
		if decoded != nil && decoded.kind == "P" {
			repo = ctx.store.repoByID(decoded.repoID)
			if repo != nil {
				for _, p := range repo.Pulls {
					if p.Number == decoded.number {
						pr = p
						break
					}
				}
			}
		}
		if repo == nil || pr == nil {
			pushErr("Could not resolve to a PullRequest with the id provided.")
			return gqlObj{"__typename": "MergePullRequestPayload", "pullRequest": nil}
		}
		if pr.State != "open" {
			pushErr("Pull request is already closed.")
			return gqlObj{"__typename": "MergePullRequestPayload", "pullRequest": prObj(repo, pr)}
		}
		result := squashMerge(repo, pr)
		if result.conflict {
			pushErr("Merge conflict.")
			return gqlObj{"__typename": "MergePullRequestPayload", "pullRequest": prObj(repo, pr)}
		}
		pr.State = "closed"
		pr.Merged = true
		pr.MergedAt = now
		pr.MergeCommitSha = result.sha
		pr.UpdatedAt = now
		return gqlObj{"__typename": "MergePullRequestPayload", "pullRequest": prObj(repo, pr)}
	case "addComment":
		decoded := decodeGid(toStr(input["subjectId"]))
		var repo *Repo
		if decoded != nil && (decoded.kind == "I" || decoded.kind == "P") {
			repo = ctx.store.repoByID(decoded.repoID)
		}
		if repo == nil {
			pushErr("Could not resolve to a subject with the id provided.")
			return gqlObj{"__typename": "AddCommentPayload", "commentEdge": nil}
		}
		body := toStr(input["body"])
		repo.Seq++
		comment := &Comment{
			ID:        repo.Seq,
			Body:      body,
			CreatedAt: now,
			UpdatedAt: now,
			UserLogin: ViewerLogin,
			UserID:    ViewerID,
		}
		targetNumber := decoded.number
		if decoded.kind == "I" {
			var issue *Issue
			for _, i := range repo.Issues {
				if i.Number == decoded.number {
					issue = i
					break
				}
			}
			if issue == nil {
				pushErr("Could not resolve to an Issue with the id provided.")
				return gqlObj{"__typename": "AddCommentPayload", "commentEdge": nil}
			}
			issue.Comments = append(issue.Comments, comment)
		} else {
			var pr *PullRequest
			for _, p := range repo.Pulls {
				if p.Number == decoded.number {
					pr = p
					break
				}
			}
			if pr == nil {
				pushErr("Could not resolve to a PullRequest with the id provided.")
				return gqlObj{"__typename": "AddCommentPayload", "commentEdge": nil}
			}
			pr.Comments = append(pr.Comments, comment)
		}
		return gqlObj{
			"__typename": "AddCommentPayload",
			"commentEdge": gqlObj{
				"node":   gqlObj{"__typename": "Comment", "id": gidComment(repo, targetNumber, comment.ID), "body": body},
				"cursor": cursorFor(comment.ID),
			},
		}
	}
	return nil
}

func executeSelection(parentType string, parent gqlObj, selection []selectionNode, ctx *gqlCtx) map[string]any {
	out := make(map[string]any)
	for _, sel := range selection {
		if sel.isFragment {
			actual := parentType
			if t, ok := parent["__typename"].(string); ok {
				actual = t
			}
			if sel.fragment.typeCondition != "" && sel.fragment.typeCondition != actual {
				continue
			}
			pt := parentType
			if sel.fragment.typeCondition != "" {
				pt = sel.fragment.typeCondition
			}
			for k, v := range executeSelection(pt, parent, sel.fragment.selection, ctx) {
				out[k] = v
			}
			continue
		}
		f := sel.field
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		typeInfo, hasType := gqlSchema[parentType]
		known := false
		if hasType {
			for _, fn := range typeInfo.fields {
				if fn == f.name {
					known = true
					break
				}
			}
		}
		if f.name == "__typename" {
			known = true
		}
		if !known {
			ctx.errors = append(ctx.errors, map[string]any{"message": fmt.Sprintf("Cannot query field %q on type %q.", f.name, parentType)})
			out[key] = nil
			continue
		}
		value := resolveField(parentType, parent, f, ctx)
		if f.selection != nil {
			if value == nil {
				out[key] = nil
			} else if arr, ok := value.([]any); ok {
				childType := parentType
				if ft, ok := gqlFieldType[parentType]; ok {
					if ct, ok := ft[f.name]; ok {
						childType = ct
					}
				}
				mapped := make([]any, len(arr))
				for i, v := range arr {
					if vm, ok := v.(map[string]any); ok {
						mapped[i] = executeSelection(childType, vm, f.selection, ctx)
					} else {
						mapped[i] = v
					}
				}
				out[key] = mapped
			} else if vm, ok := value.(map[string]any); ok {
				childType := parentType
				if ft, ok := gqlFieldType[parentType]; ok {
					if ct, ok := ft[f.name]; ok {
						childType = ct
					}
				}
				out[key] = executeSelection(childType, vm, f.selection, ctx)
			} else {
				out[key] = value
			}
		} else {
			out[key] = value
		}
	}
	return out
}

func executeOperation(store *Store, op *operationNode, variables map[string]any) map[string]any {
	ctx := &gqlCtx{store: store, variables: variables}
	// Apply variable defaults.
	for _, v := range op.variables {
		if _, ok := ctx.variables[v.name]; !ok && v.hasDef {
			ctx.variables[v.name] = resolveValue(v.def, ctx)
		}
	}
	rootType := "Query"
	if op.kind == "mutation" {
		rootType = "Mutation"
	}
	data := executeSelection(rootType, gqlObj{"__typename": rootType}, op.selection, ctx)
	result := map[string]any{"data": data}
	if len(ctx.errors) > 0 {
		result["errors"] = ctx.errors
	}
	return result
}

// handleGraphQL executes one GraphQL request body (object or array of
// operations). It takes the store write lock for the whole handler
// (mutations write; GraphQL is conformance-only, never a timed workload).
func handleGraphQL(store *Store, rawBody []byte) (status int, body any) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var parsed any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return 400, map[string]any{"errors": []any{map[string]any{"message": "Request body is not valid JSON."}}}
	}
	runOne := func(entry any) map[string]any {
		m, ok := entry.(map[string]any)
		if !ok {
			return map[string]any{"errors": []any{map[string]any{"message": "Request body must be a JSON object with a query."}}}
		}
		query, ok := m["query"].(string)
		if !ok {
			return map[string]any{"errors": []any{map[string]any{"message": "Request body must include a query string."}}}
		}
		ops, err := parseGraphQL(query)
		if err != nil {
			return map[string]any{"errors": []any{map[string]any{"message": "GraphQL syntax error: " + err.Error()}}}
		}
		vars := map[string]any{}
		if v, ok := m["variables"].(map[string]any); ok {
			vars = v
		}
		if len(ops) == 0 {
			return map[string]any{"errors": []any{map[string]any{"message": "Empty GraphQL document."}}}
		}
		return executeOperation(store, ops[0], vars)
	}
	if arr, ok := parsed.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, e := range arr {
			out = append(out, runOne(e))
		}
		return 200, out
	}
	return 200, runOne(parsed)
}
