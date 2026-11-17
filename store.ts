// store.ts — in-memory data model for the forge.
//
// One Store holds every repository. All state lives here; nothing is read
// from disk at request time, which keeps the hot paths (2000 sequential
// issue reads, 200 sequential issue writes) cheap.
//
// Determinism: nothing in this file touches Date.now(), Math.random() or
// any wall clock. Creation timestamps come from the caller-supplied
// "now" string (MAKEFUN_TRIAL_NOW or a fixed fallback), so a live run and
// its replay leave byte-identical state.

import { createHash } from "node:crypto";

export const VIEWER_LOGIN = "adaiversen";
export const VIEWER_ID = 1000;
export const VIEWER_NAME = "ada iversen";
export const VIEWER_EMAIL = "adaiversen@example.invalid";
export const DEFAULT_BRANCH = "main";
export const FIXED_NOW = "2025-01-01T00:00:00Z";
// Epoch seconds used for merge commits when no trial time is supplied.
export const FIXED_EPOCH = 1700000000;

export function nowIso(): string {
  const t = process.env.MAKEFUN_TRIAL_NOW;
  return t && t.length > 0 ? t : FIXED_NOW;
}

export function nowEpoch(): number {
  const t = process.env.MAKEFUN_TRIAL_NOW;
  if (t && t.length > 0) {
    const ms = Date.parse(t);
    if (!Number.isNaN(ms)) return Math.floor(ms / 1000);
  }
  return FIXED_EPOCH;
}

// ---------------------------------------------------------------------------
// Git objects
// ---------------------------------------------------------------------------

export type ObjType = "blob" | "tree" | "commit" | "tag";

export interface GitObject {
  type: ObjType;
  /** Raw payload, without the "<type> <len>\0" header. */
  data: Buffer;
}

export function sha1Of(type: ObjType, data: Buffer): string {
  const head = Buffer.from(`${type} ${data.length}\0`, "utf8");
  return createHash("sha1").update(head).update(data).digest("hex");
}

export function storeObject(objects: Map<string, GitObject>, type: ObjType, data: Buffer): string {
  const sha = sha1Of(type, data);
  if (!objects.has(sha)) objects.set(sha, { type, data });
  return sha;
}

export interface TreeEntry {
  mode: string; // "100644" | "100755" | "120000" | "40000"
  name: string;
  sha: string;
}

export function encodeTree(entries: TreeEntry[]): Buffer {
  const parts: Buffer[] = [];
  const sorted = [...entries].sort((a, b) => {
    const an = a.mode === "40000" ? a.name + "/" : a.name;
    const bn = b.mode === "40000" ? b.name + "/" : b.name;
    return an < bn ? -1 : an > bn ? 1 : 0;
  });
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, "utf8"));
    parts.push(Buffer.from(e.sha, "hex"));
  }
  return Buffer.concat(parts);
}

export function parseTree(data: Buffer): TreeEntry[] {
  const out: TreeEntry[] = [];
  let i = 0;
  while (i < data.length) {
    const sp = data.indexOf(0x20, i);
    const nul = data.indexOf(0x00, sp);
    const mode = data.toString("utf8", i, sp);
    const name = data.toString("utf8", sp + 1, nul);
    const sha = data.subarray(nul + 1, nul + 21).toString("hex");
    out.push({ mode, name, sha });
    i = nul + 21;
  }
  return out;
}

export interface CommitInfo {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function encodeCommit(c: CommitInfo): Buffer {
  const lines: string[] = [`tree ${c.tree}`];
  for (const p of c.parents) lines.push(`parent ${p}`);
  lines.push(`author ${c.author}`);
  lines.push(`committer ${c.committer}`);
  lines.push("");
  lines.push(c.message);
  return Buffer.from(lines.join("\n"), "utf8");
}

export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString("utf8");
  const headEnd = text.indexOf("\n\n");
  const head = headEnd >= 0 ? text.slice(0, headEnd) : text;
  const message = headEnd >= 0 ? text.slice(headEnd + 2) : "";
  const parents: string[] = [];
  let tree = "";
  let author = "";
  let committer = "";
  for (const line of head.split("\n")) {
    if (line.startsWith("tree ")) tree = line.slice(5).trim();
    else if (line.startsWith("parent ")) parents.push(line.slice(7).trim());
    else if (line.startsWith("author ")) author = line.slice(7);
    else if (line.startsWith("committer ")) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message };
}

export function parseTag(data: Buffer): { object: string; type: string; tag: string; tagger: string; message: string } {
  const text = data.toString("utf8");
  const headEnd = text.indexOf("\n\n");
  const head = headEnd >= 0 ? text.slice(0, headEnd) : text;
  const message = headEnd >= 0 ? text.slice(headEnd + 2) : "";
  let object = "", type = "", tag = "", tagger = "";
  for (const line of head.split("\n")) {
    if (line.startsWith("object ")) object = line.slice(7).trim();
    else if (line.startsWith("type ")) type = line.slice(5).trim();
    else if (line.startsWith("tag ")) tag = line.slice(4).trim();
    else if (line.startsWith("tagger ")) tagger = line.slice(7);
  }
  return { object, type, tag, tagger, message };
}

/** Identity line used for commits we author: "name <email> epoch +0000". */
export function identLine(): string {
  return `${VIEWER_NAME} <${VIEWER_EMAIL}> ${nowEpoch()} +0000`;
}

// ---------------------------------------------------------------------------
// Repositories, issues, pull requests
// ---------------------------------------------------------------------------

export interface Label { name: string; color: string; description?: string }

export interface Comment {
  id: number;
  body: string;
  created_at: string;
  updated_at: string;
  user: { login: string; id: number };
}

export interface Issue {
  number: number;
  title: string;
  body: string;
  state: "open" | "closed";
  labels: Label[];
  comments: Comment[];
  created_at: string;
  updated_at: string;
  closed_at: string | null;
  user: { login: string; id: number };
  seq: number; // monotonic creation order within the repo
}

export interface PullRequest {
  number: number;
  title: string;
  body: string;
  state: "open" | "closed";
  merged: boolean;
  merged_at: string | null;
  merge_commit_sha: string | null;
  head: { ref: string; sha: string };
  base: { ref: string; sha: string };
  created_at: string;
  updated_at: string;
  seq: number;
  comments: Comment[];
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  description: string;
  private: boolean;
  default_branch: string;
  created_at: string;
  updated_at: string;
  /** git object store: sha -> object */
  objects: Map<string, GitObject>;
  /** ref name -> commit/tag sha (refs/heads/* and refs/tags/*) */
  refs: Map<string, string>;
  /** annotated tag name -> tag object sha is in refs; this maps tag name -> target */
  labels: Map<string, Label>;
  issues: Issue[];
  pulls: PullRequest[];
  /** one shared counter: issues and PRs draw numbers from the same sequence */
  nextNumber: number;
  seq: number; // creation-order counter for issues/PRs/comments
}

export class Store {
  repos = new Map<string, Repo>(); // key: owner + "/" + name
  nextRepoId = 1;
  private requestSeq = 0;

  requestId(): string {
    this.requestSeq += 1;
    return `req-${this.requestSeq}`;
  }

  key(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  createRepo(owner: string, name: string, opts: { description?: string; private?: boolean; default_branch?: string } = {}): Repo {
    const now = nowIso();
    const repo: Repo = {
      id: this.nextRepoId++,
      name,
      owner,
      description: opts.description ?? "",
      private: opts.private ?? false,
      default_branch: opts.default_branch ?? DEFAULT_BRANCH,
      created_at: now,
      updated_at: now,
      objects: new Map(),
      refs: new Map(),
      labels: new Map(),
      issues: [],
      pulls: [],
      nextNumber: 1,
      seq: 0,
    };
    this.repos.set(this.key(owner, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.key(owner, name));
  }

  listReposFor(owner: string): Repo[] {
    const out: Repo[] = [];
    for (const r of this.repos.values()) if (r.owner === owner) out.push(r);
    out.sort((a, b) => a.id - b.id);
    return out;
  }
}

// ---------------------------------------------------------------------------
// Pack helpers (used by git.ts)
// ---------------------------------------------------------------------------

/** Encode a pack object header: type + size, variable length. */
export function packObjectHeader(typeNum: number, size: number): Buffer {
  const bytes: number[] = [];
  let b = (typeNum << 4) | (size & 0x0f);
  size >>= 4;
  if (size > 0) b |= 0x80;
  bytes.push(b);
  while (size > 0) {
    b = size & 0x7f;
    size >>= 7;
    if (size > 0) b |= 0x80;
    bytes.push(b);
  }
  return Buffer.from(bytes);
}

export const PACK_TYPE_NUM: Record<ObjType, number> = {
  commit: 1,
  tree: 2,
  blob: 3,
  tag: 4,
};
