// graphql.ts — GitHub GraphQL v4 surface at /api/graphql.
//
// A small, purpose-built parser + executor for the documents gh and the
// suite send (queries, mutations, variables, aliases, inline fragments,
// __type introspection, operation batching). It is not a general GraphQL
// server: unknown fields are reported as errors with HTTP 200, unknown
// repos resolve to data.repository = null with no errors, and two
// operation documents in one JSON array body answer two results in order.

import {
  branchTip,
  diffTrees,
  flattenTree,
  mergeBase,
  squashMerge,
  type FileChange,
} from "./rest.ts";
import {
  nowIso,
  parseCommit as parseCommitRaw,
  VIEWER_ID,
  VIEWER_LOGIN,
  type Issue,
  type PullRequest,
  type Repo,
  type Store,
} from "./store.ts";

// ---------------------------------------------------------------------------
// Global ids: opaque, stable across reads (base64 of a typed key).
// ---------------------------------------------------------------------------

export function gidRepo(repo: Repo): string {
  return Buffer.from(`R:${repo.id}`, "utf8").toString("base64");
}
export function gidIssue(repo: Repo, n: number): string {
  return Buffer.from(`I:${repo.id}:${n}`, "utf8").toString("base64");
}
export function gidPull(repo: Repo, n: number): string {
  return Buffer.from(`P:${repo.id}:${n}`, "utf8").toString("base64");
}
export function gidComment(repo: Repo, n: number, commentId: number): string {
  return Buffer.from(`C:${repo.id}:${n}:${commentId}`, "utf8").toString("base64");
}

function decodeGid(id: string): { kind: string; repoId: number; number: number; extra: number } | null {
  try {
    const text = Buffer.from(id, "base64").toString("utf8");
    const parts = text.split(":");
    if (parts.length < 2) return null;
    return {
      kind: parts[0],
      repoId: parseInt(parts[1], 10),
      number: parts.length > 2 ? parseInt(parts[2], 10) : 0,
      extra: parts.length > 3 ? parseInt(parts[3], 10) : 0,
    };
  } catch {
    return null;
  }
}

function repoById(store: Store, id: number): Repo | undefined {
  for (const r of store.repos.values()) if (r.id === id) return r;
  return undefined;
}

// ---------------------------------------------------------------------------
// Tokenizer + parser
// ---------------------------------------------------------------------------

type Token =
  | { kind: "name"; value: string }
  | { kind: "string"; value: string }
  | { kind: "int"; value: string }
  | { kind: "float"; value: string }
  | { kind: "punct"; value: string }
  | { kind: "spread" };

function tokenize(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const c = src[i];
    if (c === " " || c === "\t" || c === "\n" || c === "\r" || c === ",") {
      i += 1;
      continue;
    }
    if (c === "#") {
      while (i < src.length && src[i] !== "\n") i += 1;
      continue;
    }
    if (c === "." && src.slice(i, i + 3) === "...") {
      tokens.push({ kind: "spread" });
      i += 3;
      continue;
    }
    if ("{}()[]:!=$@".includes(c)) {
      tokens.push({ kind: "punct", value: c });
      i += 1;
      continue;
    }
    if (c === '"') {
      let j = i + 1;
      let out = "";
      while (j < src.length && src[j] !== '"') {
        if (src[j] === "\\" && j + 1 < src.length) {
          const esc = src[j + 1];
          out += esc === "n" ? "\n" : esc === "t" ? "\t" : esc === "r" ? "\r" : esc === '"' ? '"' : esc === "\\" ? "\\" : esc;
          j += 2;
        } else {
          out += src[j];
          j += 1;
        }
      }
      tokens.push({ kind: "string", value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(c)) {
      let j = i;
      while (j < src.length && /[0-9.]/.test(src[j])) j += 1;
      const text = src.slice(i, j);
      tokens.push(text.includes(".") ? { kind: "float", value: text } : { kind: "int", value: text });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i;
      while (j < src.length && /[A-Za-z0-9_]/.test(src[j])) j += 1;
      tokens.push({ kind: "name", value: src.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(c)} in GraphQL document`);
  }
  return tokens;
}

export type ValueNode =
  | { kind: "variable"; name: string }
  | { kind: "string"; value: string }
  | { kind: "int"; value: number }
  | { kind: "float"; value: number }
  | { kind: "boolean"; value: boolean }
  | { kind: "null" }
  | { kind: "enum"; value: string }
  | { kind: "list"; values: ValueNode[] }
  | { kind: "object"; fields: { name: string; value: ValueNode }[] };

export interface FieldNode {
  kind: "field";
  alias?: string;
  name: string;
  args: { name: string; value: ValueNode }[];
  selection?: SelectionNode[];
}
export interface FragmentNode {
  kind: "fragment";
  typeCondition?: string;
  selection: SelectionNode[];
}
export type SelectionNode = FieldNode | FragmentNode;

export interface OperationNode {
  kind: "query" | "mutation";
  name?: string;
  variables: { name: string; default?: ValueNode }[];
  selection: SelectionNode[];
}

class Parser {
  private pos = 0;
  private tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token | undefined {
    return this.tokens[this.pos];
  }
  private next(): Token {
    const t = this.tokens[this.pos];
    if (!t) throw new Error("unexpected end of GraphQL document");
    this.pos += 1;
    return t;
  }
  private expectPunct(v: string): void {
    const t = this.next();
    if (t.kind !== "punct" || t.value !== v) throw new Error(`expected ${v} in GraphQL document`);
  }
  private isPunct(v: string): boolean {
    const t = this.peek();
    return !!t && t.kind === "punct" && t.value === v;
  }
  private expectName(): string {
    const t = this.next();
    if (t.kind !== "name") throw new Error("expected a name in GraphQL document");
    return t.value;
  }

  parseDocument(): OperationNode[] {
    const ops: OperationNode[] = [];
    while (this.pos < this.tokens.length) {
      ops.push(this.parseOperation());
    }
    return ops;
  }

  private parseOperation(): OperationNode {
    let kind: "query" | "mutation" = "query";
    let name: string | undefined;
    const t = this.peek();
    if (t?.kind === "name" && (t.value === "query" || t.value === "mutation")) {
      kind = this.next().value as "query" | "mutation";
      const n = this.peek();
      if (n?.kind === "name") name = this.next().value;
    }
    const variables: OperationNode["variables"] = [];
    if (this.isPunct("(")) {
      this.next();
      while (!this.isPunct(")")) {
        this.expectPunct("$");
        const vname = this.expectName();
        this.expectPunct(":");
        this.parseType();
        let def: ValueNode | undefined;
        if (this.isPunct("=")) {
          this.next();
          def = this.parseValue();
        }
        variables.push({ name: vname, default: def });
      }
      this.expectPunct(")");
    }
    // directives (ignored)
    while (this.isPunct("@")) {
      this.next();
      this.expectName();
      if (this.isPunct("(")) this.parseArguments();
    }
    const selection = this.parseSelectionSet();
    return { kind, name, variables, selection };
  }

  private parseType(): void {
    if (this.isPunct("[")) {
      this.next();
      this.parseType();
      this.expectPunct("]");
    } else {
      this.expectName();
    }
    if (this.isPunct("!")) this.next();
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expectPunct("{");
    const out: SelectionNode[] = [];
    while (!this.isPunct("}")) {
      out.push(this.parseSelection());
    }
    this.expectPunct("}");
    return out;
  }

  private parseSelection(): SelectionNode {
    const t = this.peek();
    if (t?.kind === "spread") {
      this.next();
      const n = this.peek();
      if (n?.kind === "name" && n.value === "on") {
        this.next();
        const typeCondition = this.expectName();
        while (this.isPunct("@")) {
          this.next();
          this.expectName();
          if (this.isPunct("(")) this.parseArguments();
        }
        return { kind: "fragment", typeCondition, selection: this.parseSelectionSet() };
      }
      // named fragment spread (not used by the suite); skip its selections
      this.expectName();
      return { kind: "fragment", selection: [] };
    }
    let alias: string | undefined;
    let name = this.expectName();
    if (this.isPunct(":")) {
      this.next();
      alias = name;
      name = this.expectName();
    }
    let args: FieldNode["args"] = [];
    if (this.isPunct("(")) args = this.parseArguments();
    while (this.isPunct("@")) {
      this.next();
      this.expectName();
      if (this.isPunct("(")) this.parseArguments();
    }
    let selection: SelectionNode[] | undefined;
    if (this.isPunct("{")) selection = this.parseSelectionSet();
    return { kind: "field", alias, name, args, selection };
  }

  private parseArguments(): FieldNode["args"] {
    this.expectPunct("(");
    const args: FieldNode["args"] = [];
    while (!this.isPunct(")")) {
      const name = this.expectName();
      this.expectPunct(":");
      args.push({ name, value: this.parseValue() });
    }
    this.expectPunct(")");
    return args;
  }

  private parseValue(): ValueNode {
    const t = this.next();
    if (t.kind === "punct" && t.value === "$") return { kind: "variable", name: this.expectName() };
    if (t.kind === "punct" && t.value === "[") {
      const values: ValueNode[] = [];
      while (!(this.peek()?.kind === "punct" && this.peek()?.value === "]")) values.push(this.parseValue());
      this.expectPunct("]");
      return { kind: "list", values };
    }
    if (t.kind === "punct" && t.value === "{") {
      const fields: { name: string; value: ValueNode }[] = [];
      while (!(this.peek()?.kind === "punct" && this.peek()?.value === "}")) {
        const name = this.expectName();
        this.expectPunct(":");
        fields.push({ name, value: this.parseValue() });
      }
      this.expectPunct("}");
      return { kind: "object", fields };
    }
    if (t.kind === "string") return { kind: "string", value: t.value };
    if (t.kind === "int") return { kind: "int", value: parseInt(t.value, 10) };
    if (t.kind === "float") return { kind: "float", value: parseFloat(t.value) };
    if (t.kind === "name") {
      if (t.value === "true") return { kind: "boolean", value: true };
      if (t.value === "false") return { kind: "boolean", value: false };
      if (t.value === "null") return { kind: "null" };
      return { kind: "enum", value: t.value };
    }
    throw new Error("unexpected value in GraphQL document");
  }
}

export function parseGraphQL(src: string): OperationNode[] {
  return new Parser(tokenize(src)).parseDocument();
}

// ---------------------------------------------------------------------------
// Schema (for validation + introspection)
// ---------------------------------------------------------------------------

interface TypeInfo {
  kind: "OBJECT" | "ENUM";
  fields?: string[];
  enumValues?: string[];
}

const SCHEMA: Record<string, TypeInfo> = {
  Query: { kind: "OBJECT", fields: ["viewer", "repository", "node", "__type", "__schema"] },
  Node: { kind: "OBJECT", fields: ["id"] },
  Mutation: { kind: "OBJECT", fields: ["createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"] },
  User: { kind: "OBJECT", fields: ["id", "login", "name", "email"] },
  Repository: {
    kind: "OBJECT",
    fields: ["id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"],
  },
  Issue: { kind: "OBJECT", fields: ["id", "number", "title", "body", "state", "createdAt", "updatedAt", "closedAt", "author", "comments"] },
  PullRequest: {
    kind: "OBJECT",
    fields: ["id", "number", "title", "body", "state", "merged", "mergedAt", "headRefName", "baseRefName", "mergeable", "files", "createdAt", "updatedAt"],
  },
  Ref: { kind: "OBJECT", fields: ["name"] },
  IssueConnection: { kind: "OBJECT", fields: ["nodes", "edges", "pageInfo", "totalCount"] },
  IssueCommentConnection: { kind: "OBJECT", fields: ["nodes", "edges", "pageInfo", "totalCount"] },
  IssueCommentEdge: { kind: "OBJECT", fields: ["node", "cursor"] },
  PullRequestConnection: { kind: "OBJECT", fields: ["nodes", "edges", "pageInfo", "totalCount"] },
  PullRequestFileConnection: { kind: "OBJECT", fields: ["nodes", "edges", "pageInfo", "totalCount"] },
  IssueEdge: { kind: "OBJECT", fields: ["node", "cursor"] },
  PullRequestEdge: { kind: "OBJECT", fields: ["node", "cursor"] },
  PullRequestFileEdge: { kind: "OBJECT", fields: ["node", "cursor"] },
  PageInfo: { kind: "OBJECT", fields: ["hasNextPage", "hasPreviousPage", "startCursor", "endCursor"] },
  PullRequestFile: { kind: "OBJECT", fields: ["path", "additions", "deletions", "changeType"] },
  Comment: { kind: "OBJECT", fields: ["id", "body", "createdAt", "updatedAt", "author"] },
  AddedCommentEdge: { kind: "OBJECT", fields: ["node", "cursor"] },
  CreateIssuePayload: { kind: "OBJECT", fields: ["issue", "clientMutationId"] },
  CloseIssuePayload: { kind: "OBJECT", fields: ["issue", "clientMutationId"] },
  CreatePullRequestPayload: { kind: "OBJECT", fields: ["pullRequest", "clientMutationId"] },
  MergePullRequestPayload: { kind: "OBJECT", fields: ["pullRequest", "clientMutationId"] },
  AddCommentPayload: { kind: "OBJECT", fields: ["commentEdge", "clientMutationId"] },
  __Type: { kind: "OBJECT", fields: ["name", "kind", "fields", "enumValues"] },
  __Field: { kind: "OBJECT", fields: ["name"] },
  __EnumValue: { kind: "OBJECT", fields: ["name"] },
  __Schema: { kind: "OBJECT", fields: ["queryType", "mutationType", "types"] },
  SearchType: { kind: "ENUM", enumValues: ["ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"] },
  IssueState: { kind: "ENUM", enumValues: ["OPEN", "CLOSED"] },
  IssueOrderField: { kind: "ENUM", enumValues: ["CREATED_AT", "UPDATED_AT", "COMMENTS"] },
  OrderDirection: { kind: "ENUM", enumValues: ["ASC", "DESC"] },
  PullRequestState: { kind: "ENUM", enumValues: ["OPEN", "CLOSED", "MERGED"] },
  MergeableState: { kind: "ENUM", enumValues: ["MERGEABLE", "CONFLICTING", "UNKNOWN"] },
  MergeMethod: { kind: "ENUM", enumValues: ["MERGE", "SQUASH", "REBASE"] },
};

const FIELD_TYPE: Record<string, Record<string, string>> = {
  Query: { viewer: "User", repository: "Repository", node: "Node", __type: "__Type", __schema: "__Schema" },
  Issue: { comments: "IssueCommentConnection" },
  IssueCommentConnection: { nodes: "Comment", edges: "IssueCommentEdge", pageInfo: "PageInfo" },
  IssueCommentEdge: { node: "Comment" },
  Repository: {
    owner: "User",
    defaultBranchRef: "Ref",
    issue: "Issue",
    issues: "IssueConnection",
    pullRequests: "PullRequestConnection",
    pullRequest: "PullRequest",
  },
  IssueConnection: { nodes: "Issue", edges: "IssueEdge", pageInfo: "PageInfo" },
  PullRequestConnection: { nodes: "PullRequest", edges: "PullRequestEdge", pageInfo: "PageInfo" },
  PullRequestFileConnection: { nodes: "PullRequestFile", edges: "PullRequestFileEdge", pageInfo: "PageInfo" },
  IssueEdge: { node: "Issue" },
  PullRequestEdge: { node: "PullRequest" },
  PullRequestFileEdge: { node: "PullRequestFile" },
  PullRequest: { files: "PullRequestFileConnection" },
  Mutation: {
    createIssue: "CreateIssuePayload",
    closeIssue: "CloseIssuePayload",
    createPullRequest: "CreatePullRequestPayload",
    mergePullRequest: "MergePullRequestPayload",
    addComment: "AddCommentPayload",
  },
  CreateIssuePayload: { issue: "Issue" },
  CloseIssuePayload: { issue: "Issue" },
  CreatePullRequestPayload: { pullRequest: "PullRequest" },
  MergePullRequestPayload: { pullRequest: "PullRequest" },
  AddCommentPayload: { commentEdge: "AddedCommentEdge" },
  AddedCommentEdge: { node: "Comment" },
  __Type: { fields: "__Field", enumValues: "__EnumValue" },
  __Schema: { queryType: "__Type", mutationType: "__Type", types: "__Type" },
};

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

interface Ctx {
  store: Store;
  variables: Record<string, unknown>;
  errors: { message: string }[];
}

type Value = unknown;

function resolveValue(node: ValueNode, ctx: Ctx): Value {
  switch (node.kind) {
    case "variable": {
      const v = ctx.variables[node.name];
      return v === undefined ? null : v;
    }
    case "string":
    case "int":
    case "float":
    case "boolean":
      return node.value;
    case "null":
      return null;
    case "enum":
      return node.value;
    case "list":
      return node.values.map((v) => resolveValue(v, ctx));
    case "object": {
      const out: Record<string, unknown> = {};
      for (const f of node.fields) out[f.name] = resolveValue(f.value, ctx);
      return out;
    }
  }
}

function argsOf(field: FieldNode, ctx: Ctx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const a of field.args) out[a.name] = resolveValue(a.value, ctx);
  return out;
}

function cursorFor(seq: number): string {
  return Buffer.from(`cursor:${seq}`, "utf8").toString("base64");
}
function seqFromCursor(cursor: string): number | null {
  try {
    const text = Buffer.from(cursor, "base64").toString("utf8");
    if (!text.startsWith("cursor:")) return null;
    const n = parseInt(text.slice(7), 10);
    return Number.isNaN(n) ? null : n;
  } catch {
    return null;
  }
}

interface PageArgs {
  first?: number;
  after?: string;
}

function connection<T>(items: T[], args: PageArgs, cursorOf: (item: T, index: number) => string): Record<string, unknown> {
  let start = 0;
  if (args.after) {
    const seq = seqFromCursor(args.after);
    if (seq !== null) {
      const idx = items.findIndex((it, i) => cursorOf(it, i) === args.after);
      start = idx >= 0 ? idx + 1 : 0;
    }
  }
  const first = args.first ?? items.length;
  const slice = items.slice(start, start + first);
  const endCursor = slice.length ? cursorOf(slice[slice.length - 1], start + slice.length - 1) : null;
  return {
    nodes: slice,
    edges: slice.map((node, i) => ({ node, cursor: cursorOf(node, start + i) })),
    totalCount: items.length,
    pageInfo: {
      hasNextPage: start + slice.length < items.length,
      hasPreviousPage: start > 0,
      startCursor: slice.length ? cursorOf(slice[0], start) : null,
      endCursor,
    },
  };
}

function issueStateOut(state: string): string {
  return state === "open" ? "OPEN" : "CLOSED";
}
function prStateOut(p: PullRequest): string {
  if (p.merged) return "MERGED";
  return p.state === "open" ? "OPEN" : "CLOSED";
}

function mergeableOf(repo: Repo, pr: PullRequest): string {
  const baseTip = branchTip(repo, pr.base.ref);
  const headTip = branchTip(repo, pr.head.ref);
  if (!baseTip || !headTip) return "UNKNOWN";
  const base = mergeBase(repo, baseTip, headTip);
  if (!base) return "MERGEABLE";
  const baseTree = parseCommitRaw(repo.objects.get(base)!.data).tree;
  const headTree = parseCommitRaw(repo.objects.get(headTip)!.data).tree;
  const baseCommitTree = parseCommitRaw(repo.objects.get(baseTip)!.data).tree;
  const headChanges = diffTrees(repo, baseTree, headTree);
  const baseChanges = diffTrees(repo, baseTree, baseCommitTree);
  const baseMap = new Map(baseChanges.map((c) => [c.filename, c]));
  const headFiles = flattenTree(repo, headTree);
  const baseFiles = flattenTree(repo, baseCommitTree);
  for (const change of headChanges) {
    if (!baseMap.has(change.filename)) continue;
    const h = headFiles.get(change.filename);
    const b = baseFiles.get(change.filename);
    if (!h || !b || h.sha !== b.sha) return "CONFLICTING";
  }
  return "MERGEABLE";
}

function prFiles(repo: Repo, pr: PullRequest): FileChange[] {
  const baseTip = branchTip(repo, pr.base.ref);
  const headTip = branchTip(repo, pr.head.ref);
  if (!baseTip || !headTip) return [];
  const base = mergeBase(repo, baseTip, headTip);
  const baseTree = base ? parseCommitRaw(repo.objects.get(base)!.data).tree : "";
  const headTree = parseCommitRaw(repo.objects.get(headTip)!.data).tree;
  if (!baseTree) return diffTrees(repo, "", headTree);
  return diffTrees(repo, baseTree, headTree);
}

// The resolved value of an object carries its GraphQL type in __typename.
type Obj = Record<string, unknown>;

function repoObj(repo: Repo): Obj {
  return {
    __typename: "Repository",
    __repo: repo,
    id: gidRepo(repo),
    databaseId: repo.id,
    name: repo.name,
    nameWithOwner: `${repo.owner}/${repo.name}`,
    owner: { __typename: "User", login: repo.owner, id: VIEWER_ID },
    viewerPermission: "ADMIN",
    hasIssuesEnabled: true,
  };
}

function issueObj(repo: Repo, issue: Issue): Obj {
  return {
    __typename: "Issue",
    __repo: repo,
    __issue: issue,
    id: gidIssue(repo, issue.number),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issueStateOut(issue.state),
    createdAt: issue.created_at,
    updatedAt: issue.updated_at,
    closedAt: issue.closed_at,
    author: { __typename: "User", login: issue.user.login, id: issue.user.id },
  };
}

function prObj(repo: Repo, pr: PullRequest): Obj {
  return {
    __typename: "PullRequest",
    __repo: repo,
    __pr: pr,
    id: gidPull(repo, pr.number),
    number: pr.number,
    title: pr.title,
    body: pr.body,
    state: prStateOut(pr),
    merged: pr.merged,
    mergedAt: pr.merged_at,
    headRefName: pr.head.ref,
    baseRefName: pr.base.ref,
  };
}

function resolveField(parentType: string, parent: Obj, field: FieldNode, ctx: Ctx): Value {
  const args = argsOf(field, ctx);
  // __typename is valid on every object type.
  if (field.name === "__typename") return parent.__typename ?? parentType;

  switch (parentType) {
    case "Query": {
      if (field.name === "viewer") {
        return { __typename: "User", id: VIEWER_ID, login: VIEWER_LOGIN, name: "ada iversen", email: "adaiversen@example.invalid" };
      }
      if (field.name === "repository") {
        const owner = args.owner as string | undefined;
        const name = args.name as string | undefined;
        if (!owner || !name) return null;
        const repo = ctx.store.getRepo(owner, name);
        return repo ? repoObj(repo) : null;
      }
      if (field.name === "node") {
        const decoded = decodeGid(String(args.id ?? ""));
        if (!decoded) return null;
        const repo = repoById(ctx.store, decoded.repoId);
        if (!repo) return null;
        if (decoded.kind === "R") return repoObj(repo);
        if (decoded.kind === "I") {
          const issue = repo.issues.find((i) => i.number === decoded.number);
          return issue ? issueObj(repo, issue) : null;
        }
        if (decoded.kind === "P") {
          const pr = repo.pulls.find((p) => p.number === decoded.number);
          return pr ? prObj(repo, pr) : null;
        }
        return null;
      }
      if (field.name === "__type") {
        const info = SCHEMA[String(args.name ?? "")];
        if (!info) return null;
        return {
          __typename: "__Type",
          name: args.name,
          kind: info.kind,
          __fields: info.fields ?? [],
          __enumValues: info.enumValues ?? [],
        };
      }
      if (field.name === "__schema") {
        return { __typename: "__Schema", __queryType: "Query", __mutationType: "Mutation" };
      }
      return null;
    }
    case "Repository": {
      const repo = parent.__repo as Repo;
      if (field.name === "defaultBranchRef") {
        if (!repo.refs.has(`refs/heads/${repo.default_branch}`)) return null;
        return { __typename: "Ref", name: repo.default_branch };
      }
      if (field.name === "issue") {
        const issue = repo.issues.find((i) => i.number === (args.number as number));
        return issue ? issueObj(repo, issue) : null;
      }
      if (field.name === "issues") {
        let list = [...repo.issues];
        const states = args.states as string[] | undefined;
        if (states?.length) list = list.filter((i) => states.includes(issueStateOut(i.state)));
        const orderBy = args.orderBy as { field?: string; direction?: string } | undefined;
        const dir = orderBy?.direction ?? "DESC";
        if (orderBy?.field === "UPDATED_AT") {
          list.sort((a, b) => (dir === "ASC" ? a.seq - b.seq : b.seq - a.seq));
        } else {
          // CREATED_AT (default): creation order == number order
          list.sort((a, b) => (dir === "ASC" ? a.seq - b.seq : b.seq - a.seq));
        }
        const pageArgs: PageArgs = { first: args.first as number | undefined, after: args.after as string | undefined };
        const conn = connection(list, pageArgs, (it) => cursorFor(it.seq));
        return { __typename: "IssueConnection", ...conn, nodes: (conn.nodes as Issue[]).map((i) => issueObj(repo, i)), edges: (conn.edges as { node: Issue; cursor: string }[]).map((e) => ({ node: issueObj(repo, e.node), cursor: e.cursor })) };
      }
      if (field.name === "pullRequests") {
        const list = [...repo.pulls].sort((a, b) => b.seq - a.seq);
        const pageArgs: PageArgs = { first: args.first as number | undefined, after: args.after as string | undefined };
        const conn = connection(list, pageArgs, (it) => cursorFor(it.seq));
        return { __typename: "PullRequestConnection", ...conn, nodes: (conn.nodes as PullRequest[]).map((p) => prObj(repo, p)), edges: (conn.edges as { node: PullRequest; cursor: string }[]).map((e) => ({ node: prObj(repo, e.node), cursor: e.cursor })) };
      }
      if (field.name === "pullRequest") {
        const pr = repo.pulls.find((p) => p.number === (args.number as number));
        return pr ? prObj(repo, pr) : null;
      }
      return parent[field.name] ?? null;
    }
    case "PullRequest": {
      const repo = parent.__repo as Repo;
      const pr = parent.__pr as PullRequest;
      if (field.name === "mergeable") return mergeableOf(repo, pr);
      if (field.name === "files") {
        const changes = prFiles(repo, pr);
        const pageArgs: PageArgs = { first: args.first as number | undefined, after: args.after as string | undefined };
        const conn = connection(changes, pageArgs, (_it, i) => cursorFor(i + 1));
        const nodes = (conn.nodes as FileChange[]).map((c) => ({
          __typename: "PullRequestFile",
          path: c.filename,
          additions: c.additions,
          deletions: c.deletions,
          changeType: c.status === "added" ? "ADDED" : "MODIFIED",
        }));
        return { __typename: "PullRequestFileConnection", ...conn, nodes, edges: (conn.edges as { node: FileChange; cursor: string }[]).map((e, i) => ({ node: nodes[i], cursor: e.cursor })) };
      }
      return parent[field.name] ?? null;
    }
    case "Issue": {
      if (field.name === "comments") {
        const issue = parent.__issue as Issue;
        const repo = parent.__repo as Repo;
        const pageArgs: PageArgs = { first: args.first as number | undefined, after: args.after as string | undefined };
        const conn = connection(issue.comments, pageArgs, (c) => cursorFor(c.id));
        return {
          __typename: "IssueCommentConnection",
          ...conn,
          nodes: (conn.nodes as Issue["comments"][number][]).map((c) => ({ __typename: "Comment", id: gidComment(repo, issue.number, c.id), body: c.body, createdAt: c.created_at, updatedAt: c.updated_at })),
        };
      }
      return parent[field.name] ?? null;
    }
    case "Mutation": {
      return resolveMutation(field, ctx);
    }
    case "__Type": {
      if (field.name === "fields") {
        return (parent.__fields as string[]).map((name) => ({ __typename: "__Field", name }));
      }
      if (field.name === "enumValues") {
        return (parent.__enumValues as string[]).map((name) => ({ __typename: "__EnumValue", name }));
      }
      return parent[field.name] ?? null;
    }
    case "__Schema": {
      if (field.name === "queryType") return { __typename: "__Type", name: "Query", kind: "OBJECT", __fields: [], __enumValues: [] };
      if (field.name === "mutationType") return { __typename: "__Type", name: "Mutation", kind: "OBJECT", __fields: [], __enumValues: [] };
      if (field.name === "types") {
        return Object.keys(SCHEMA).map((name) => ({
          __typename: "__Type",
          name,
          kind: SCHEMA[name]!.kind,
          __fields: SCHEMA[name]!.fields ?? [],
          __enumValues: SCHEMA[name]!.enumValues ?? [],
        }));
      }
      return parent[field.name] ?? null;
    }
    default:
      return parent[field.name] ?? null;
  }
}

function resolveMutation(field: FieldNode, ctx: Ctx): Value {
  const args = argsOf(field, ctx);
  const input = (args.input ?? {}) as Record<string, unknown>;
  const now = nowIso();

  if (field.name === "createIssue") {
    const decoded = decodeGid(String(input.repositoryId ?? ""));
    const repo = decoded?.kind === "R" ? repoById(ctx.store, decoded.repoId) : undefined;
    if (!repo) {
      ctx.errors.push({ message: "Could not resolve to a Repository with the id provided." });
      return { __typename: "CreateIssuePayload", issue: null };
    }
    const issue: Issue = {
      number: repo.nextNumber++,
      title: String(input.title ?? ""),
      body: typeof input.body === "string" ? input.body : "",
      state: "open",
      labels: [],
      comments: [],
      created_at: now,
      updated_at: now,
      closed_at: null,
      user: { login: VIEWER_LOGIN, id: VIEWER_ID },
      seq: ++repo.seq,
    };
    repo.issues.push(issue);
    return { __typename: "CreateIssuePayload", issue: issueObj(repo, issue) };
  }
  if (field.name === "closeIssue") {
    const decoded = decodeGid(String(input.issueId ?? ""));
    const repo = decoded?.kind === "I" ? repoById(ctx.store, decoded.repoId) : undefined;
    const issue = repo?.issues.find((i) => i.number === decoded?.number);
    if (!repo || !issue) {
      ctx.errors.push({ message: "Could not resolve to an Issue with the id provided." });
      return { __typename: "CloseIssuePayload", issue: null };
    }
    issue.state = "closed";
    issue.closed_at = now;
    issue.updated_at = now;
    return { __typename: "CloseIssuePayload", issue: issueObj(repo, issue) };
  }
  if (field.name === "createPullRequest") {
    const decoded = decodeGid(String(input.repositoryId ?? ""));
    const repo = decoded?.kind === "R" ? repoById(ctx.store, decoded.repoId) : undefined;
    const baseRef = String(input.baseRefName ?? "");
    const headRef = String(input.headRefName ?? "");
    const headSha = repo ? branchTip(repo, headRef) : undefined;
    const baseSha = repo ? branchTip(repo, baseRef) : undefined;
    if (!repo || !headSha || !baseSha) {
      ctx.errors.push({ message: "Could not resolve to a repository or branch with the input provided." });
      return { __typename: "CreatePullRequestPayload", pullRequest: null };
    }
    const pr: PullRequest = {
      number: repo.nextNumber++,
      title: String(input.title ?? ""),
      body: typeof input.body === "string" ? input.body : "",
      state: "open",
      merged: false,
      merged_at: null,
      merge_commit_sha: null,
      head: { ref: headRef, sha: headSha },
      base: { ref: baseRef, sha: baseSha },
      created_at: now,
      updated_at: now,
      seq: ++repo.seq,
      comments: [],
    };
    repo.pulls.push(pr);
    return { __typename: "CreatePullRequestPayload", pullRequest: prObj(repo, pr) };
  }
  if (field.name === "mergePullRequest") {
    const decoded = decodeGid(String(input.pullRequestId ?? ""));
    const repo = decoded?.kind === "P" ? repoById(ctx.store, decoded.repoId) : undefined;
    const pr = repo?.pulls.find((p) => p.number === decoded?.number);
    if (!repo || !pr) {
      ctx.errors.push({ message: "Could not resolve to a PullRequest with the id provided." });
      return { __typename: "MergePullRequestPayload", pullRequest: null };
    }
    if (pr.state !== "open") {
      ctx.errors.push({ message: "Pull request is already closed." });
      return { __typename: "MergePullRequestPayload", pullRequest: prObj(repo, pr) };
    }
    const result = squashMerge(repo, pr);
    if ("conflict" in result) {
      ctx.errors.push({ message: "Merge conflict." });
      return { __typename: "MergePullRequestPayload", pullRequest: prObj(repo, pr) };
    }
    pr.state = "closed";
    pr.merged = true;
    pr.merged_at = now;
    pr.merge_commit_sha = result.sha;
    pr.updated_at = now;
    return { __typename: "MergePullRequestPayload", pullRequest: prObj(repo, pr) };
  }
  if (field.name === "addComment") {
    const decoded = decodeGid(String(input.subjectId ?? ""));
    const repo = decoded && (decoded.kind === "I" || decoded.kind === "P") ? repoById(ctx.store, decoded.repoId) : undefined;
    if (!repo) {
      ctx.errors.push({ message: "Could not resolve to a subject with the id provided." });
      return { __typename: "AddCommentPayload", commentEdge: null };
    }
    const body = String(input.body ?? "");
    const comment = {
      id: ++repo.seq,
      body,
      created_at: now,
      updated_at: now,
      user: { login: VIEWER_LOGIN, id: VIEWER_ID },
    };
    let targetNumber = decoded!.number;
    if (decoded!.kind === "I") {
      const issue = repo.issues.find((i) => i.number === decoded!.number);
      if (!issue) {
        ctx.errors.push({ message: "Could not resolve to an Issue with the id provided." });
        return { __typename: "AddCommentPayload", commentEdge: null };
      }
      issue.comments.push(comment);
    } else {
      const pr = repo.pulls.find((p) => p.number === decoded!.number);
      if (!pr) {
        ctx.errors.push({ message: "Could not resolve to a PullRequest with the id provided." });
        return { __typename: "AddCommentPayload", commentEdge: null };
      }
      pr.comments.push(comment);
    }
    return {
      __typename: "AddCommentPayload",
      commentEdge: {
        node: { __typename: "Comment", id: gidComment(repo, targetNumber, comment.id), body },
        cursor: cursorFor(comment.id),
      },
    };
  }
  return null;
}

function executeSelection(
  parentType: string,
  parent: Obj,
  selection: SelectionNode[],
  ctx: Ctx,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of selection) {
    if (sel.kind === "fragment") {
      const actual = (parent.__typename as string | undefined) ?? parentType;
      if (sel.typeCondition && sel.typeCondition !== actual) continue;
      // Fields inside `... on T` validate against T.
      Object.assign(out, executeSelection(sel.typeCondition ?? parentType, parent, sel.selection, ctx));
      continue;
    }
    const key = sel.alias ?? sel.name;
    const typeInfo = SCHEMA[parentType];
    const known = typeInfo?.fields?.includes(sel.name) || sel.name === "__typename";
    if (!known) {
      ctx.errors.push({ message: `Cannot query field "${sel.name}" on type "${parentType}".` });
      out[key] = null;
      continue;
    }
    const value = resolveField(parentType, parent, sel, ctx);
    if (sel.selection && value !== null && typeof value === "object") {
      const childType = FIELD_TYPE[parentType]?.[sel.name] ?? parentType;
      if (Array.isArray(value)) {
        out[key] = value.map((v) =>
          v !== null && typeof v === "object"
            ? executeSelection(childType, v as Obj, sel.selection!, ctx)
            : v,
        );
      } else {
        out[key] = executeSelection(childType, value as Obj, sel.selection, ctx);
      }
    } else if (sel.selection && value === null) {
      out[key] = null;
    } else {
      out[key] = value;
    }
  }
  return out;
}

function executeOperation(store: Store, op: OperationNode, variables: Record<string, unknown>): Record<string, unknown> {
  const ctx: Ctx = { store, variables, errors: [] };
  // Apply variable defaults.
  for (const v of op.variables) {
    if (ctx.variables[v.name] === undefined && v.default) {
      ctx.variables[v.name] = resolveValue(v.default, ctx);
    }
  }
  const rootType = op.kind === "mutation" ? "Mutation" : "Query";
  const data = executeSelection(rootType, { __typename: rootType } as Obj, op.selection, ctx);
  const result: Record<string, unknown> = { data };
  if (ctx.errors.length) result.errors = ctx.errors;
  return result;
}

/** Execute one GraphQL request body (object or array of operations). */
export function handleGraphQL(store: Store, rawBody: Buffer): { status: number; body: unknown } {
  let parsed: unknown;
  try {
    parsed = JSON.parse(rawBody.toString("utf8"));
  } catch {
    return { status: 400, body: { errors: [{ message: "Request body is not valid JSON." }] } };
  }
  const runOne = (entry: unknown): Record<string, unknown> => {
    if (!entry || typeof entry !== "object" || Array.isArray(entry)) {
      return { errors: [{ message: "Request body must be a JSON object with a query." }] };
    }
    const query = (entry as Record<string, unknown>).query;
    const variables = (entry as Record<string, unknown>).variables;
    if (typeof query !== "string") {
      return { errors: [{ message: "Request body must include a query string." }] };
    }
    let ops: OperationNode[];
    try {
      ops = parseGraphQL(query);
    } catch (e) {
      return { errors: [{ message: `GraphQL syntax error: ${(e as Error).message}` }] };
    }
    const vars = variables !== null && typeof variables === "object" && !Array.isArray(variables)
      ? (variables as Record<string, unknown>)
      : {};
    const op = ops[0];
    if (!op) return { errors: [{ message: "Empty GraphQL document." }] };
    return executeOperation(store, op, { ...vars });
  };
  if (Array.isArray(parsed)) {
    return { status: 200, body: parsed.map(runOne) };
  }
  return { status: 200, body: runOne(parsed) };
}
