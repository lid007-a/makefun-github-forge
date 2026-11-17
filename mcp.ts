// mcp.ts — Model Context Protocol at /mcp: JSON-RPC 2.0 over Streamable
// HTTP (plain JSON answers; SSE is accepted by the client but not needed).
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

import { branchTip, issueJson, prJson, repoJson, squashMerge } from "./rest.ts";
import {
  nowIso,
  VIEWER_ID,
  VIEWER_LOGIN,
  type Issue,
  type PullRequest,
  type Repo,
  type Store,
} from "./store.ts";

export const MCP_CURRENT = "2026-07-28";
const SUPPORTED_VERSIONS = ["2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"];
const SERVER_NAME = "github-forge";
const SERVER_VERSION = "1.0.0";

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: "object";
    properties: Record<string, { type: string; description: string }>;
    required: string[];
  };
}

const TOOLS: ToolDef[] = [
  {
    name: "create_repo",
    description: "Create a new repository owned by the authenticated user.",
    inputSchema: {
      type: "object",
      properties: {
        name: { type: "string", description: "Repository name." },
        description: { type: "string", description: "Repository description." },
        private: { type: "boolean", description: "Whether the repository is private." },
      },
      required: ["name"],
    },
  },
  {
    name: "get_repo",
    description: "Get a repository by owner and name.",
    inputSchema: {
      type: "object",
      properties: {
        owner: { type: "string", description: "Repository owner login." },
        repo: { type: "string", description: "Repository name." },
      },
      required: ["owner", "repo"],
    },
  },
  {
    name: "create_issue",
    description: "Create an issue in a repository.",
    inputSchema: {
      type: "object",
      properties: {
        owner: { type: "string", description: "Repository owner login." },
        repo: { type: "string", description: "Repository name." },
        title: { type: "string", description: "Issue title." },
        body: { type: "string", description: "Issue body." },
      },
      required: ["owner", "repo", "title"],
    },
  },
  {
    name: "list_issues",
    description: "List issues in a repository, optionally filtered by state.",
    inputSchema: {
      type: "object",
      properties: {
        owner: { type: "string", description: "Repository owner login." },
        repo: { type: "string", description: "Repository name." },
        state: { type: "string", description: "Issue state: open, closed or all." },
      },
      required: ["owner", "repo"],
    },
  },
  {
    name: "create_pull_request",
    description: "Open a pull request from a head branch onto a base branch.",
    inputSchema: {
      type: "object",
      properties: {
        owner: { type: "string", description: "Repository owner login." },
        repo: { type: "string", description: "Repository name." },
        title: { type: "string", description: "Pull request title." },
        head: { type: "string", description: "Head branch name." },
        base: { type: "string", description: "Base branch name." },
        body: { type: "string", description: "Pull request body." },
      },
      required: ["owner", "repo", "title", "head", "base"],
    },
  },
  {
    name: "merge_pull_request",
    description: "Squash-merge a pull request onto its base branch.",
    inputSchema: {
      type: "object",
      properties: {
        owner: { type: "string", description: "Repository owner login." },
        repo: { type: "string", description: "Repository name." },
        pull_number: { type: "integer", description: "Pull request number." },
      },
      required: ["owner", "repo", "pull_number"],
    },
  },
  {
    name: "list_repos",
    description: "List repositories owned by the authenticated user.",
    inputSchema: {
      type: "object",
      properties: {
        owner: { type: "string", description: "Owner login (defaults to the authenticated user)." },
      },
      required: [],
    },
  },
  {
    name: "get_issue",
    description: "Get one issue by repository and number.",
    inputSchema: {
      type: "object",
      properties: {
        owner: { type: "string", description: "Repository owner login." },
        repo: { type: "string", description: "Repository name." },
        issue_number: { type: "integer", description: "Issue number." },
      },
      required: ["owner", "repo", "issue_number"],
    },
  },
];

function serverInfo(): { name: string; version: string } {
  return { name: SERVER_NAME, version: SERVER_VERSION };
}

// ---------------------------------------------------------------------------
// Argument picking: accept the documented names and their common synonyms,
// and a full_name ("owner/repo") when owner/repo are absent.
// ---------------------------------------------------------------------------

function pick(args: Record<string, unknown>, ...names: string[]): unknown {
  for (const n of names) {
    const v = args[n];
    if (v !== undefined && v !== null && v !== "") return v;
  }
  return undefined;
}

function pickOwnerRepo(args: Record<string, unknown>): { owner?: string; repo?: string } {
  let owner = pick(args, "owner", "owner_login", "org", "organization", "user", "login") as string | undefined;
  let repo = pick(args, "repo", "repository", "repo_name", "name") as string | undefined;
  const full = pick(args, "full_name", "repo_full_name", "fullName") as string | undefined;
  if ((!owner || !repo) && typeof full === "string" && full.includes("/")) {
    const [o, r] = full.split("/");
    owner = owner ?? o;
    repo = repo ?? r;
  }
  return { owner, repo };
}

// ---------------------------------------------------------------------------
// Tool implementations
// ---------------------------------------------------------------------------

function toolResult(data: unknown, isError = false): Record<string, unknown> {
  const text = typeof data === "string" ? data : JSON.stringify(data);
  return {
    content: [{ type: "text", text }],
    structuredContent: typeof data === "object" && data !== null ? data : { result: data },
    isError,
  };
}

function errResult(message: string): Record<string, unknown> {
  return toolResult({ error: message }, true);
}

function repoFor(args: Record<string, unknown>, store: Store): { repo?: Repo; error?: string } {
  const { owner, repo } = pickOwnerRepo(args);
  if (!owner || !repo) return { error: "owner and repo are required" };
  const found = store.getRepo(owner, repo);
  if (!found) return { error: `Repository not found: ${owner}/${repo}` };
  return { repo: found };
}

function callTool(store: Store, name: string, args: Record<string, unknown>): { result: Record<string, unknown>; rpcError?: { code: number; message: string } } {
  const fail = (message: string) => ({ result: errResult(message) });
  switch (name) {
    case "create_repo": {
      const repoName = pick(args, "name") as string | undefined;
      if (!repoName) return fail("Missing required argument: name");
      if (store.getRepo(VIEWER_LOGIN, repoName)) return fail(`Repository already exists: ${VIEWER_LOGIN}/${repoName}`);
      const repo = store.createRepo(VIEWER_LOGIN, repoName, {
        description: pick(args, "description") as string | undefined,
        private: pick(args, "private") as boolean | undefined,
      });
      return { result: toolResult(repoJson(repo)) };
    }
    case "get_repo": {
      const { repo, error } = repoFor(args, store);
      if (error) return fail(error);
      return { result: toolResult(repoJson(repo!)) };
    }
    case "create_issue": {
      const { repo, error } = repoFor(args, store);
      if (error) return fail(error);
      const title = pick(args, "title") as string | undefined;
      if (!title) return fail("Missing required argument: title");
      const now = nowIso();
      const issue: Issue = {
        number: repo!.nextNumber++,
        title,
        body: (pick(args, "body", "description") as string | undefined) ?? "",
        state: "open",
        labels: [],
        comments: [],
        created_at: now,
        updated_at: now,
        closed_at: null,
        user: { login: VIEWER_LOGIN, id: VIEWER_ID },
        seq: ++repo!.seq,
      };
      repo!.issues.push(issue);
      return { result: toolResult(issueJson(repo!, issue)) };
    }
    case "list_issues": {
      const { repo, error } = repoFor(args, store);
      if (error) return fail(error);
      const state = (pick(args, "state") as string | undefined) ?? "open";
      const list = repo!.issues.filter((i) => state === "all" || i.state === state);
      return { result: toolResult({ issues: list.map((i) => issueJson(repo!, i)) }) };
    }
    case "create_pull_request": {
      const { repo, error } = repoFor(args, store);
      if (error) return fail(error);
      const title = pick(args, "title") as string | undefined;
      const head = pick(args, "head", "head_ref", "head_branch") as string | undefined;
      const base = pick(args, "base", "base_ref", "base_branch") as string | undefined;
      if (!title) return fail("Missing required argument: title");
      if (!head) return fail("Missing required argument: head");
      if (!base) return fail("Missing required argument: base");
      const headSha = branchTip(repo!, head);
      const baseSha = branchTip(repo!, base);
      if (!headSha || !baseSha) return fail(`head or base branch does not exist: ${head} / ${base}`);
      const now = nowIso();
      const pr: PullRequest = {
        number: repo!.nextNumber++,
        title,
        body: (pick(args, "body", "description") as string | undefined) ?? "",
        state: "open",
        merged: false,
        merged_at: null,
        merge_commit_sha: null,
        head: { ref: head, sha: headSha },
        base: { ref: base, sha: baseSha },
        created_at: now,
        updated_at: now,
        seq: ++repo!.seq,
        comments: [],
      };
      repo!.pulls.push(pr);
      return { result: toolResult(prJson(repo!, pr)) };
    }
    case "merge_pull_request": {
      const { repo, error } = repoFor(args, store);
      if (error) return fail(error);
      const num = pick(args, "pull_number", "pullNumber", "number", "pr_number") as number | undefined;
      if (num === undefined) return fail("Missing required argument: pull_number");
      const pr = repo!.pulls.find((p) => p.number === Number(num));
      if (!pr) return fail(`Pull request not found: #${num}`);
      if (pr.state !== "open") return fail("Pull request is already closed");
      const result = squashMerge(repo!, pr);
      if ("conflict" in result) return fail("Merge conflict");
      pr.state = "closed";
      pr.merged = true;
      pr.merged_at = nowIso();
      pr.merge_commit_sha = result.sha;
      pr.updated_at = nowIso();
      return { result: toolResult({ sha: result.sha, merged: true, message: result.message, pull_request: prJson(repo!, pr) }) };
    }
    case "list_repos": {
      const owner = (pick(args, "owner") as string | undefined) ?? VIEWER_LOGIN;
      const repos = store.listReposFor(owner);
      return { result: toolResult({ repositories: repos.map(repoJson) }) };
    }
    case "get_issue": {
      const { repo, error } = repoFor(args, store);
      if (error) return fail(error);
      const num = pick(args, "issue_number", "issueNumber", "number") as number | undefined;
      if (num === undefined) return fail("Missing required argument: issue_number");
      const issue = repo!.issues.find((i) => i.number === Number(num));
      if (!issue) return fail(`Issue not found: #${num}`);
      return { result: toolResult(issueJson(repo!, issue)) };
    }
    default:
      return { result: {}, rpcError: { code: -32602, message: `Unknown tool: ${name}` } };
  }
}

// ---------------------------------------------------------------------------
// JSON-RPC dispatch
// ---------------------------------------------------------------------------

export interface McpResult {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

function rpcResult(id: unknown, result: unknown): Record<string, unknown> {
  return { jsonrpc: "2.0", id: id ?? null, result };
}
function rpcError(id: unknown, code: number, message: string, data?: unknown): Record<string, unknown> {
  const error: Record<string, unknown> = { code, message };
  if (data !== undefined) error.data = data;
  return { jsonrpc: "2.0", id: id ?? null, error };
}

function discoverResult(): Record<string, unknown> {
  return {
    protocolVersion: MCP_CURRENT,
    supportedVersions: [...SUPPORTED_VERSIONS],
    capabilities: { tools: {} },
    serverInfo: serverInfo(),
    _meta: { "io.modelcontextprotocol/serverInfo": serverInfo() },
  };
}

export function handleMcp(
  store: Store,
  rawBody: Buffer,
  headers: Record<string, string | undefined>,
): McpResult {
  const protocolHeader = headers["mcp-protocol-version"];
  const isModern = protocolHeader !== undefined && protocolHeader >= "2026-07-28";
  let parsed: unknown;
  try {
    parsed = JSON.parse(rawBody.toString("utf8"));
  } catch {
    // Non-JSON body: HTTP 400 with -32700 in the modern era, 200 before it.
    const body = rpcError(null, -32700, "Parse error: request body is not valid JSON");
    return { status: isModern ? 400 : 200, body };
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { status: 400, body: rpcError(null, -32600, "Invalid Request") };
  }
  const req = parsed as Record<string, unknown>;
  const id = (req.id ?? null) as unknown;
  const method = req.method as string | undefined;
  const params = (req.params ?? {}) as Record<string, unknown>;

  // Notifications (no id): acknowledge, no JSON-RPC response body.
  if (id === null || id === undefined) {
    return { status: 202, body: "" };
  }

  switch (method) {
    case "server/discover": {
      return { status: 200, body: rpcResult(id, discoverResult()) };
    }
    case "initialize": {
      const offered = (params.protocolVersion as string | undefined) ?? "";
      const chosen = SUPPORTED_VERSIONS.includes(offered) ? offered : "2025-11-25";
      return {
        status: 200,
        body: rpcResult(id, {
          protocolVersion: chosen,
          capabilities: { tools: {} },
          serverInfo: serverInfo(),
        }),
        headers: { "Mcp-Session-Id": "forge-session-1" },
      };
    }
    case "ping": {
      return { status: 200, body: rpcResult(id, {}) };
    }
    case "tools/list": {
      // One frozen list: byte-identical on every call.
      return { status: 200, body: rpcResult(id, { tools: TOOLS }) };
    }
    case "tools/call": {
      const toolName = params.name as string | undefined;
      const args = (params.arguments ?? {}) as Record<string, unknown>;
      if (!toolName) {
        return { status: 200, body: rpcError(id, -32602, "Missing required argument: name") };
      }
      const known = TOOLS.some((t) => t.name === toolName);
      if (!known) {
        return { status: 200, body: rpcError(id, -32602, `Unknown tool: ${toolName}`) };
      }
      const { result, rpcError: rpcErr } = callTool(store, toolName, args);
      if (rpcErr) {
        return { status: 200, body: rpcError(id, rpcErr.code, rpcErr.message) };
      }
      return { status: 200, body: rpcResult(id, result) };
    }
    default: {
      return { status: 200, body: rpcError(id, -32601, `Method not found: ${method}`) };
    }
  }
}
