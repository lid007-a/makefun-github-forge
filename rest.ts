// rest.ts — GitHub REST API v3 surface under /api/v3.
//
// Every reply is JSON with a non-empty X-GitHub-Request-Id. Errors are
// JSON {message}. A repo under any owner other than the token owner is a
// 404, never a 403. Numbers for issues and pull requests come from one
// shared per-repo counter, like real GitHub.

import {
  encodeCommit,
  encodeTree,
  identLine,
  nowIso,
  parseCommit,
  parseTree,
  sha1Of,
  storeObject,
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  type Issue,
  type Label,
  type PullRequest,
  type Repo,
  type Store,
} from "./store.ts";

export interface RestResult {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

const ok = (status: number, body: unknown, headers?: Record<string, string>): RestResult => ({
  status,
  body,
  headers,
});
const err = (status: number, message: string): RestResult => ({ status, body: { message } });

// ---------------------------------------------------------------------------
// JSON shapes
// ---------------------------------------------------------------------------

function userJson() {
  return { login: VIEWER_LOGIN, id: VIEWER_ID, name: VIEWER_NAME, email: VIEWER_EMAIL, type: "User" };
}

export function repoJson(r: Repo) {
  return {
    id: r.id,
    name: r.name,
    full_name: `${r.owner}/${r.name}`,
    owner: { login: r.owner, id: VIEWER_ID, type: "User" },
    private: r.private,
    description: r.description,
    default_branch: r.default_branch,
    created_at: r.created_at,
    updated_at: r.updated_at,
    html_url: `https://github.com/${r.owner}/${r.name}`,
    clone_url: `https://github.com/${r.owner}/${r.name}.git`,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
  };
}

function labelJson(l: Label) {
  return { name: l.name, color: l.color, description: l.description ?? "" };
}

export function issueJson(r: Repo, i: Issue) {
  return {
    id: r.id * 1000000 + i.number,
    number: i.number,
    title: i.title,
    body: i.body,
    state: i.state,
    labels: i.labels.map(labelJson),
    user: i.user,
    created_at: i.created_at,
    updated_at: i.updated_at,
    closed_at: i.closed_at,
    comments: i.comments.length,
    html_url: `https://github.com/${r.owner}/${r.name}/issues/${i.number}`,
  };
}

export function prJson(r: Repo, p: PullRequest) {
  return {
    id: r.id * 1000000 + p.number,
    number: p.number,
    title: p.title,
    body: p.body,
    state: p.state,
    merged: p.merged,
    merged_at: p.merged_at,
    merge_commit_sha: p.merge_commit_sha,
    head: { ref: p.head.ref, sha: p.head.sha },
    base: { ref: p.base.ref, sha: p.base.sha },
    user: { login: VIEWER_LOGIN, id: VIEWER_ID },
    created_at: p.created_at,
    updated_at: p.updated_at,
    draft: false,
    html_url: `https://github.com/${r.owner}/${r.name}/pull/${p.number}`,
  };
}

// ---------------------------------------------------------------------------
// Tree / commit helpers (shared with the GraphQL and MCP layers)
// ---------------------------------------------------------------------------

export interface FlatFile {
  mode: string;
  sha: string;
}

/** Flatten a tree object into path -> {mode, sha} (blobs only). */
export function flattenTree(repo: Repo, treeSha: string, prefix = "", out = new Map<string, FlatFile>()): Map<string, FlatFile> {
  const obj = repo.objects.get(treeSha);
  if (!obj || obj.type !== "tree") return out;
  for (const entry of parseTree(obj.data)) {
    const path = prefix ? `${prefix}/${entry.name}` : entry.name;
    if (entry.mode === "40000") flattenTree(repo, entry.sha, path, out);
    else out.set(path, { mode: entry.mode, sha: entry.sha });
  }
  return out;
}

/** Build nested tree objects from a flat file map; returns the root tree sha. */
export function buildTree(repo: Repo, files: Map<string, FlatFile>): string {
  const build = (prefix: string): string => {
    const entries: { mode: string; name: string; sha: string }[] = [];
    const subdirs = new Set<string>();
    for (const [path, file] of files) {
      if (prefix !== "" && !path.startsWith(`${prefix}/`)) continue;
      const rest = prefix === "" ? path : path.slice(prefix.length + 1);
      const slash = rest.indexOf("/");
      if (slash < 0) entries.push({ mode: file.mode, name: rest, sha: file.sha });
      else subdirs.add(rest.slice(0, slash));
    }
    for (const dir of subdirs) {
      const sha = build(prefix ? `${prefix}/${dir}` : dir);
      entries.push({ mode: "40000", name: dir, sha });
    }
    return storeObject(repo.objects, "tree", encodeTree(entries));
  };
  return build("");
}

export function commitTree(repo: Repo, treeSha: string, parents: string[], message: string): string {
  const ident = identLine();
  const data = encodeCommit({ tree: treeSha, parents, author: ident, committer: ident, message });
  return storeObject(repo.objects, "commit", data);
}

/** Tip commit sha of a branch (or undefined when the branch does not exist). */
export function branchTip(repo: Repo, branch: string): string | undefined {
  return repo.refs.get(`refs/heads/${branch}`);
}

function resolveRefToCommit(repo: Repo, ref: string): string | undefined {
  const tip = branchTip(repo, ref);
  if (tip) return tip;
  if (/^[0-9a-f]{40}$/.test(ref)) {
    const obj = repo.objects.get(ref);
    if (obj?.type === "commit") return ref;
  }
  return undefined;
}

export interface ContentCommit {
  commitSha: string;
  blobSha: string;
  treeSha: string;
}

/**
 * Create (or update) a file on a branch, making the branch if needed.
 * Returns the new commit sha. `expectedSha` is enforced on update: a
 * mismatch is a 409-style stale-sha failure (returned as {stale: true}).
 */
export function putContentCommit(
  repo: Repo,
  branch: string,
  path: string,
  content: Buffer,
  message: string,
  expectedSha?: string,
): ContentCommit | { stale: true } {
  const tip = branchTip(repo, branch);
  const files = new Map<string, FlatFile>();
  if (tip) {
    const commit = parseCommit(repo.objects.get(tip)!.data);
    flattenTree(repo, commit.tree, "", files);
  }
  const existing = files.get(path);
  if (existing) {
    if (expectedSha !== undefined && expectedSha !== existing.sha) return { stale: true };
  } else if (expectedSha !== undefined) {
    return { stale: true };
  }
  const blobSha = storeObject(repo.objects, "blob", content);
  files.set(path, { mode: "100644", sha: blobSha });
  const treeSha = buildTree(repo, files);
  const commitSha = commitTree(repo, treeSha, tip ? [tip] : [], message);
  repo.refs.set(`refs/heads/${branch}`, commitSha);
  repo.updated_at = nowIso();
  return { commitSha, blobSha, treeSha };
}

export function deleteContentCommit(
  repo: Repo,
  branch: string,
  path: string,
  message: string,
  expectedSha?: string,
): ContentCommit | { stale: true } | { missing: true } {
  const tip = branchTip(repo, branch);
  if (!tip) return { missing: true };
  const commit = parseCommit(repo.objects.get(tip)!.data);
  const files = flattenTree(repo, commit.tree);
  const existing = files.get(path);
  if (!existing) return { missing: true };
  if (expectedSha !== undefined && expectedSha !== existing.sha) return { stale: true };
  files.delete(path);
  const treeSha = buildTree(repo, files);
  const commitSha = commitTree(repo, treeSha, [tip], message);
  repo.refs.set(`refs/heads/${branch}`, commitSha);
  repo.updated_at = nowIso();
  return { commitSha, blobSha: existing.sha, treeSha };
}

/** First common ancestor of two commits (or null). */
export function mergeBase(repo: Repo, a: string, b: string): string | null {
  const ancestorsA = new Set<string>();
  const stackA = [a];
  while (stackA.length) {
    const sha = stackA.pop()!;
    if (ancestorsA.has(sha)) continue;
    const obj = repo.objects.get(sha);
    if (!obj || obj.type !== "commit") continue;
    ancestorsA.add(sha);
    for (const p of parseCommit(obj.data).parents) stackA.push(p);
  }
  const stackB = [b];
  const seenB = new Set<string>();
  while (stackB.length) {
    const sha = stackB.pop()!;
    if (ancestorsA.has(sha)) return sha;
    if (seenB.has(sha)) continue;
    seenB.add(sha);
    const obj = repo.objects.get(sha);
    if (!obj || obj.type !== "commit") continue;
    for (const p of parseCommit(obj.data).parents) stackB.push(p);
  }
  return null;
}

export interface FileChange {
  filename: string;
  status: "added" | "modified" | "changed";
  additions: number;
  deletions: number;
  changes: number;
}

function countLines(buf: Buffer): string[] {
  const text = buf.toString("utf8");
  if (text === "") return [];
  const lines = text.split("\n");
  if (lines[lines.length - 1] === "") lines.pop();
  return lines;
}

function lineDiff(oldBuf: Buffer | undefined, newBuf: Buffer | undefined): { additions: number; deletions: number } {
  const oldLines = oldBuf ? countLines(oldBuf) : [];
  const newLines = newBuf ? countLines(newBuf) : [];
  const oldCount = new Map<string, number>();
  for (const l of oldLines) oldCount.set(l, (oldCount.get(l) ?? 0) + 1);
  let common = 0;
  for (const l of newLines) {
    const n = oldCount.get(l) ?? 0;
    if (n > 0) {
      oldCount.set(l, n - 1);
      common += 1;
    }
  }
  return { additions: newLines.length - common, deletions: oldLines.length - common };
}

/** Diff two trees; deletions are reported as "modified" (the suite's file
 *  status enum is added/modified/changed). */
export function diffTrees(repo: Repo, baseTree: string, headTree: string): FileChange[] {
  const base = flattenTree(repo, baseTree);
  const head = flattenTree(repo, headTree);
  const out: FileChange[] = [];
  const paths = new Set([...base.keys(), ...head.keys()]);
  for (const path of paths) {
    const b = base.get(path);
    const h = head.get(path);
    if (b && !h) {
      const oldData = repo.objects.get(b.sha)?.data;
      const d = lineDiff(oldData, undefined);
      out.push({ filename: path, status: "modified", additions: 0, deletions: d.deletions, changes: d.deletions });
    } else if (!b && h) {
      const newData = repo.objects.get(h.sha)?.data;
      const d = lineDiff(undefined, newData);
      out.push({ filename: path, status: "added", additions: d.additions, deletions: 0, changes: d.additions });
    } else if (b && h && b.sha !== h.sha) {
      const oldData = repo.objects.get(b.sha)?.data;
      const newData = repo.objects.get(h.sha)?.data;
      const d = lineDiff(oldData, newData);
      out.push({ filename: path, status: "modified", additions: d.additions, deletions: d.deletions, changes: d.additions + d.deletions });
    }
  }
  out.sort((x, y) => (x.filename < y.filename ? -1 : 1));
  return out;
}

/** Squash-merge a pull request onto its base branch. */
export function squashMerge(
  repo: Repo,
  pr: PullRequest,
): { sha: string; message: string } | { conflict: true } {
  const baseTip = branchTip(repo, pr.base.ref);
  const headTip = branchTip(repo, pr.head.ref);
  if (!baseTip || !headTip) return { conflict: true };
  const baseCommit = parseCommit(repo.objects.get(baseTip)!.data);
  const headCommit = parseCommit(repo.objects.get(headTip)!.data);
  const base = mergeBase(repo, baseTip, headTip);
  if (base) {
    const baseTree = parseCommit(repo.objects.get(base)!.data).tree;
    const headChanges = diffTrees(repo, baseTree, headCommit.tree);
    const baseChanges = diffTrees(repo, baseTree, baseCommit.tree);
    const baseChanged = new Map(baseChanges.map((c) => [c.filename, c]));
    for (const change of headChanges) {
      const other = baseChanged.get(change.filename);
      if (!other) continue;
      // Both sides changed the same file: conflict unless the resulting
      // blobs are identical.
      const headFile = flattenTree(repo, headCommit.tree).get(change.filename);
      const baseFile = flattenTree(repo, baseCommit.tree).get(change.filename);
      if (!headFile || !baseFile || headFile.sha !== baseFile.sha) return { conflict: true };
    }
  }
  const message = pr.title;
  const sha = commitTree(repo, headCommit.tree, [baseTip], message);
  repo.refs.set(`refs/heads/${pr.base.ref}`, sha);
  repo.updated_at = nowIso();
  return { sha, message };
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

type Body = Record<string, unknown>;

function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

function parseBody(raw: Buffer): { body: Body | null; malformed: boolean } {
  if (raw.length === 0) return { body: {}, malformed: false };
  try {
    const parsed: unknown = JSON.parse(raw.toString("utf8"));
    if (parsed !== null && typeof parsed === "object" && !Array.isArray(parsed)) {
      return { body: parsed as Body, malformed: false };
    }
    return { body: null, malformed: true };
  } catch {
    return { body: null, malformed: true };
  }
}

function paginate<T>(items: T[], query: URLSearchParams, basePath: string, baseUrl: string): { page: T[]; link: string } {
  const perPage = Math.max(1, Math.min(100, parseInt(query.get("per_page") ?? "30", 10) || 30));
  const page = Math.max(1, parseInt(query.get("page") ?? "1", 10) || 1);
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  let link = "";
  if (start + perPage < items.length) {
    const next = `${baseUrl}${basePath}?page=${page + 1}&per_page=${perPage}`;
    link = `<${next}>; rel="next"`;
    const lastPage = Math.ceil(items.length / perPage);
    link += `, <${baseUrl}${basePath}?page=${lastPage}&per_page=${perPage}>; rel="last"`;
  }
  return { page: slice, link };
}

export function handleRest(
  store: Store,
  method: string,
  path: string,
  query: URLSearchParams,
  rawBody: Buffer,
  baseUrl = "",
): RestResult {
  const segments = path.split("/").filter((s) => s.length > 0);
  // /api/v3[/...]
  if (segments.length < 2 || segments[0] !== "api" || segments[1] !== "v3") {
    return err(404, "Not Found");
  }
  const rest = segments.slice(2);

  // GET /api/v3/ — root
  if (rest.length === 0) {
    if (method !== "GET") return err(404, "Not Found");
    return ok(200, {
      current_user_url: "/api/v3/user",
      repository_url: "/api/v3/repos/{owner}/{repo}",
      user_repositories_url: "/api/v3/user/repos",
    }, { "X-OAuth-Scopes": "repo, user", "X-Accepted-OAuth-Scopes": "repo, user" });
  }

  // GET /api/v3/meta
  if (rest.length === 1 && rest[0] === "meta") {
    if (method !== "GET") return err(404, "Not Found");
    return ok(200, { verifiable: true, installed_version: "1.0.0", version: "1.0.0" });
  }

  // GET /api/v3/user and /api/v3/user/repos
  if (rest[0] === "user") {
    if (rest.length === 1) {
      if (method !== "GET") return err(404, "Not Found");
      return ok(200, userJson());
    }
    if (rest.length === 2 && rest[1] === "repos") {
      if (method === "GET") {
        const repos = store.listReposFor(VIEWER_LOGIN);
        const { page, link } = paginate(repos, query, "/api/v3/user/repos", baseUrl);
        return ok(200, page.map(repoJson), link ? { Link: link } : undefined);
      }
      if (method === "POST") {
        const { body, malformed } = parseBody(rawBody);
        if (malformed || !body) return err(400, "Problems parsing JSON");
        const name = str(body.name);
        if (!name) return err(422, "name is required");
        if (store.getRepo(VIEWER_LOGIN, name)) return err(422, "Repository already exists");
        const repo = store.createRepo(VIEWER_LOGIN, name, {
          description: str(body.description),
          private: body.private === true,
          default_branch: str(body.default_branch),
        });
        return ok(201, repoJson(repo));
      }
      return err(404, "Not Found");
    }
    return err(404, "Not Found");
  }

  // /api/v3/repos/{owner}/{repo}[/...]
  if (rest[0] === "repos") {
    if (rest.length < 3) return err(404, "Not Found");
    const owner = decodeURIComponent(rest[1]);
    const name = decodeURIComponent(rest[2]);
    if (owner !== VIEWER_LOGIN) return err(404, "Not Found");
    const repo = store.getRepo(owner, name);
    const tail = rest.slice(3);

    if (tail.length === 0) {
      if (!repo) return err(404, "Not Found");
      if (method === "GET") return ok(200, repoJson(repo));
      if (method === "DELETE") {
        store.deleteRepo(owner, name);
        return { status: 204, body: null };
      }
      if (method === "PATCH") {
        const { body, malformed } = parseBody(rawBody);
        if (malformed || !body) return err(400, "Problems parsing JSON");
        const desc = str(body.description);
        if (desc !== undefined) repo.description = desc;
        repo.updated_at = nowIso();
        return ok(200, repoJson(repo));
      }
      return err(404, "Not Found");
    }

    if (!repo) return err(404, "Not Found");
    return handleRepoSub(store, repo, tail, method, query, rawBody, baseUrl);
  }

  return err(404, "Not Found");
}

function handleRepoSub(
  store: Store,
  repo: Repo,
  tail: string[],
  method: string,
  query: URLSearchParams,
  rawBody: Buffer,
  baseUrl = "",
): RestResult {
  const [section, ...rest] = tail;

  // ---- git/refs ----
  if (section === "git" && rest[0] === "refs") {
    if (rest.length === 1) {
      if (method === "POST") {
        const { body, malformed } = parseBody(rawBody);
        if (malformed || !body) return err(400, "Problems parsing JSON");
        const ref = str(body.ref);
        const sha = str(body.sha);
        if (!ref || !sha) return err(422, "ref and sha are required");
        if (repo.refs.has(ref)) return err(422, "Reference already exists");
        if (!repo.objects.has(sha)) return err(422, "Object does not exist");
        repo.refs.set(ref, sha);
        return ok(201, { ref, object: { sha } });
      }
      if (method === "GET") {
        const refs = [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1));
        return ok(200, refs.map(([ref, sha]) => ({ ref, object: { sha } })));
      }
      return err(404, "Not Found");
    }
    // GET /git/refs/heads/{branch} (branch may contain slashes)
    if (method === "GET" && rest.length >= 2) {
      const ref = `refs/${rest.join("/")}`;
      const sha = repo.refs.get(ref);
      if (!sha) return err(404, "Not Found");
      return ok(200, { ref, object: { sha } });
    }
    return err(404, "Not Found");
  }

  // ---- branches ----
  if (section === "branches") {
    const branches = [...repo.refs.keys()]
      .filter((r) => r.startsWith("refs/heads/"))
      .map((r) => r.slice("refs/heads/".length))
      .sort();
    if (rest.length === 0) {
      if (method !== "GET") return err(404, "Not Found");
      return ok(200, branches.map((b) => ({ name: b, commit: { sha: repo.refs.get(`refs/heads/${b}`)! } })));
    }
    const branch = decodeURIComponent(rest.join("/"));
    const sha = repo.refs.get(`refs/heads/${branch}`);
    if (!sha) return err(404, "Not Found");
    if (method !== "GET") return err(404, "Not Found");
    return ok(200, { name: branch, commit: { sha } });
  }

  // ---- commits ----
  if (section === "commits") {
    if (method !== "GET") return err(404, "Not Found");
    const ref = query.get("sha") ?? repo.default_branch;
    const tip = resolveRefToCommit(repo, ref);
    if (!tip) return ok(200, []);
    const out: { sha: string; commit: { message: string } }[] = [];
    const seen = new Set<string>();
    const stack = [tip];
    while (stack.length) {
      const sha = stack.pop()!;
      if (seen.has(sha)) continue;
      const obj = repo.objects.get(sha);
      if (!obj || obj.type !== "commit") continue;
      seen.add(sha);
      const info = parseCommit(obj.data);
      out.push({ sha, commit: { message: info.message } });
      for (const p of info.parents) stack.push(p);
    }
    return ok(200, out);
  }

  // ---- contents ----
  if (section === "contents") {
    const filePath = rest.map((s) => decodeURIComponent(s)).join("/");
    if (method === "GET") {
      const ref = query.get("ref") ?? repo.default_branch;
      const tip = resolveRefToCommit(repo, ref);
      if (!tip) return err(404, "Not Found");
      const commit = parseCommit(repo.objects.get(tip)!.data);
      const files = flattenTree(repo, commit.tree);
      if (!filePath) {
        const entries = [...files.entries()].map(([p, f]) => ({ type: "file" as const, name: p, path: p, sha: f.sha }));
        return ok(200, entries);
      }
      const file = files.get(filePath);
      if (file) {
        const blob = repo.objects.get(file.sha)!;
        return ok(200, {
          type: "file",
          encoding: "base64",
          content: blob.data.toString("base64"),
          sha: file.sha,
          size: blob.data.length,
          name: filePath.split("/").pop(),
          path: filePath,
        });
      }
      // directory listing
      const prefix = `${filePath}/`;
      const dirs = new Set<string>();
      const entries: { type: string; name: string; path: string; sha: string }[] = [];
      for (const [p, f] of files) {
        if (!p.startsWith(prefix)) continue;
        const restPath = p.slice(prefix.length);
        const slash = restPath.indexOf("/");
        if (slash < 0) entries.push({ type: "file", name: restPath, path: p, sha: f.sha });
        else dirs.add(restPath.slice(0, slash));
      }
      if (entries.length === 0 && dirs.size === 0) return err(404, "Not Found");
      for (const d of [...dirs].sort()) {
        // directory sha: the tree object for this subdir
        const subFiles = new Map<string, FlatFile>();
        for (const [p, f] of files) if (p.startsWith(`${filePath}/${d}/`) || p === `${filePath}/${d}`) subFiles.set(p.slice(prefix.length), f);
        const treeSha = buildTree(repo, subFiles);
        entries.push({ type: "dir", name: d, path: `${filePath}/${d}`, sha: treeSha });
      }
      entries.sort((a, b) => (a.name < b.name ? -1 : 1));
      return ok(200, entries);
    }
    if (method === "PUT") {
      const { body, malformed } = parseBody(rawBody);
      if (malformed || !body) return err(400, "Problems parsing JSON");
      const contentB64 = str(body.content);
      if (contentB64 === undefined) return err(422, "content is required");
      const branch = str(body.branch) ?? repo.default_branch;
      const message = str(body.message) ?? `Update ${filePath}`;
      const content = Buffer.from(contentB64, "base64");
      // 201 when the file is created, 200 when it already existed.
      const tipBefore = branchTip(repo, branch);
      let existed = false;
      if (tipBefore) {
        existed = flattenTree(repo, parseCommit(repo.objects.get(tipBefore)!.data).tree).has(filePath);
      }
      const result = putContentCommit(repo, branch, filePath, content, message, str(body.sha));
      if ("stale" in result) return err(409, "sha does not match the current file");
      return ok(existed ? 200 : 201, {
        content: {
          type: "file",
          encoding: "base64",
          content: contentB64,
          sha: result.blobSha,
          size: content.length,
          name: filePath.split("/").pop(),
          path: filePath,
        },
        commit: { sha: result.commitSha, message },
      });
    }
    if (method === "DELETE") {
      const { body, malformed } = parseBody(rawBody);
      if (malformed || !body) return err(400, "Problems parsing JSON");
      const branch = str(body.branch) ?? repo.default_branch;
      const message = str(body.message) ?? `Delete ${filePath}`;
      const result = deleteContentCommit(repo, branch, filePath, message, str(body.sha));
      if ("missing" in result) return err(404, "Not Found");
      if ("stale" in result) return err(409, "sha does not match the current file");
      return ok(200, { commit: { sha: result.commitSha, message } });
    }
    return err(404, "Not Found");
  }

  // ---- labels ----
  if (section === "labels") {
    if (method === "GET" && rest.length === 0) {
      return ok(200, [...repo.labels.values()].map(labelJson));
    }
    if (method === "POST" && rest.length === 0) {
      const { body, malformed } = parseBody(rawBody);
      if (malformed || !body) return err(400, "Problems parsing JSON");
      const name = str(body.name);
      if (!name) return err(422, "name is required");
      if (repo.labels.has(name)) return err(422, "Label already exists");
      const label: Label = { name, color: str(body.color) ?? "ededed", description: str(body.description) };
      repo.labels.set(name, label);
      return ok(201, labelJson(label));
    }
    return err(404, "Not Found");
  }

  // ---- issues ----
  if (section === "issues") {
    if (rest.length === 0) {
      if (method === "GET") {
        const state = query.get("state") ?? "open";
        const sort = query.get("sort") ?? "created";
        const direction = query.get("direction") ?? "desc";
        let list = repo.issues.filter((i) => state === "all" || i.state === state);
        if (sort === "created") {
          list = [...list].sort((a, b) => (direction === "asc" ? a.seq - b.seq : b.seq - a.seq));
        }
        const { page, link } = paginate(list, query, `/api/v3/repos/${repo.owner}/${repo.name}/issues`, baseUrl);
        return ok(200, page.map((i) => issueJson(repo, i)), link ? { Link: link } : undefined);
      }
      if (method === "POST") {
        const { body, malformed } = parseBody(rawBody);
        if (malformed || !body) return err(400, "Problems parsing JSON");
        const title = str(body.title);
        if (!title) return err(422, "title is required");
        const labelNames: string[] = Array.isArray(body.labels)
          ? (body.labels as unknown[]).map((l) => (typeof l === "string" ? l : str((l as Body).name))).filter((x): x is string => !!x)
          : [];
        const labels: Label[] = [];
        for (const n of labelNames) {
          const l = repo.labels.get(n);
          if (!l) return err(422, `Label '${n}' does not exist`);
          labels.push(l);
        }
        const now = nowIso();
        const issue: Issue = {
          number: repo.nextNumber++,
          title,
          body: str(body.body) ?? "",
          state: "open",
          labels,
          comments: [],
          created_at: now,
          updated_at: now,
          closed_at: null,
          user: { login: VIEWER_LOGIN, id: VIEWER_ID },
          seq: ++repo.seq,
        };
        repo.issues.push(issue);
        return ok(201, issueJson(repo, issue));
      }
      return err(404, "Not Found");
    }
    const number = parseInt(rest[0], 10);
    if (Number.isNaN(number)) return err(404, "Not Found");
    const issue = repo.issues.find((i) => i.number === number);
    if (!issue) return err(404, "Not Found");
    if (rest.length === 1) {
      if (method === "GET") return ok(200, issueJson(repo, issue));
      if (method === "PATCH") {
        const { body, malformed } = parseBody(rawBody);
        if (malformed || !body) return err(400, "Problems parsing JSON");
        const title = str(body.title);
        if (title !== undefined) issue.title = title;
        const b = str(body.body);
        if (b !== undefined) issue.body = b;
        const state = str(body.state);
        if (state === "closed" || state === "open") {
          issue.state = state;
          issue.closed_at = state === "closed" ? nowIso() : null;
        }
        issue.updated_at = nowIso();
        return ok(200, issueJson(repo, issue));
      }
      return err(404, "Not Found");
    }
    if (rest[1] === "comments") {
      if (method === "GET") {
        return ok(200, issue.comments.map((c) => ({
          id: c.id, body: c.body, user: c.user, created_at: c.created_at, updated_at: c.updated_at,
        })));
      }
      if (method === "POST") {
        const { body, malformed } = parseBody(rawBody);
        if (malformed || !body) return err(400, "Problems parsing JSON");
        const text = str(body.body);
        if (!text) return err(422, "body is required");
        const now = nowIso();
        const comment = {
          id: ++repo.seq,
          body: text,
          created_at: now,
          updated_at: now,
          user: { login: VIEWER_LOGIN, id: VIEWER_ID },
        };
        issue.comments.push(comment);
        return ok(201, comment);
      }
      return err(404, "Not Found");
    }
    return err(404, "Not Found");
  }

  // ---- pulls ----
  if (section === "pulls") {
    if (rest.length === 0) {
      if (method === "GET") {
        const state = query.get("state") ?? "open";
        const list = repo.pulls.filter((p) => state === "all" || p.state === state);
        const { page, link } = paginate(list, query, `/api/v3/repos/${repo.owner}/${repo.name}/pulls`, baseUrl);
        return ok(200, page.map((p) => prJson(repo, p)), link ? { Link: link } : undefined);
      }
      if (method === "POST") {
        const { body, malformed } = parseBody(rawBody);
        if (malformed || !body) return err(400, "Problems parsing JSON");
        const title = str(body.title);
        const head = str(body.head);
        const base = str(body.base);
        if (!title || !head || !base) return err(422, "title, head and base are required");
        const headSha = branchTip(repo, head);
        const baseSha = branchTip(repo, base);
        if (!headSha || !baseSha) return err(422, "head or base branch does not exist");
        const now = nowIso();
        const pr: PullRequest = {
          number: repo.nextNumber++,
          title,
          body: str(body.body) ?? "",
          state: "open",
          merged: false,
          merged_at: null,
          merge_commit_sha: null,
          head: { ref: head, sha: headSha },
          base: { ref: base, sha: baseSha },
          created_at: now,
          updated_at: now,
          seq: ++repo.seq,
          comments: [],
        };
        repo.pulls.push(pr);
        return ok(201, prJson(repo, pr));
      }
      return err(404, "Not Found");
    }
    const number = parseInt(rest[0], 10);
    if (Number.isNaN(number)) return err(404, "Not Found");
    const pr = repo.pulls.find((p) => p.number === number);
    if (!pr) return err(404, "Not Found");
    if (rest.length === 1) {
      if (method === "GET") return ok(200, prJson(repo, pr));
      return err(404, "Not Found");
    }
    if (rest[1] === "files") {
      if (method !== "GET") return err(404, "Not Found");
      const baseTip = branchTip(repo, pr.base.ref);
      const headTip = branchTip(repo, pr.head.ref);
      if (!baseTip || !headTip) return err(404, "Not Found");
      const base = mergeBase(repo, baseTip, headTip);
      const baseTree = base ? parseCommit(repo.objects.get(base)!.data).tree : buildTree(repo, new Map());
      const headTree = parseCommit(repo.objects.get(headTip)!.data).tree;
      return ok(200, diffTrees(repo, baseTree, headTree));
    }
    if (rest[1] === "merge") {
      if (method !== "PUT") return err(404, "Not Found");
      if (pr.state !== "open") return err(405, "Pull request is already closed");
      const result = squashMerge(repo, pr);
      if ("conflict" in result) return err(405, "Merge conflict");
      pr.state = "closed";
      pr.merged = true;
      pr.merged_at = nowIso();
      pr.merge_commit_sha = result.sha;
      pr.updated_at = nowIso();
      return ok(200, { sha: result.sha, merged: true, message: result.message });
    }
    return err(404, "Not Found");
  }

  return err(404, "Not Found");
}

// ---------------------------------------------------------------------------
// Small helpers the other surfaces reuse
// ---------------------------------------------------------------------------

export function findIssue(repo: Repo, number: number): Issue | undefined {
  return repo.issues.find((i) => i.number === number);
}

export function findPull(repo: Repo, number: number): PullRequest | undefined {
  return repo.pulls.find((p) => p.number === number);
}

export function shaShape(s: string): boolean {
  return /^[0-9a-f]{40}$/.test(s);
}

export { sha1Of };
