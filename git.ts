// git.ts — git smart HTTP: pkt-line codec, pack read/write, object graph
// walking, upload-pack (v0 + v2) and receive-pack (v0).
//
// Wire notes (verified against the suite's own git-client.ts):
// - v2 fetch responses name their sections with literal pkt-lines
//   ("acknowledgments\n", "shallow-info\n", "packfile\n"), each section
//   ends with a flush, and there is NO response-end packet.
// - A v2 fetch with nothing to send still carries an empty 32-byte pack
//   as one sideband packet (the client errors if the packfile section
//   has zero packets).
// - v0 wants carry capabilities space-separated on the first want line.
// - Push reports are pkt-lined inside sideband band 1, then a flush.

import { createHash } from "node:crypto";
import { deflateSync, inflateSync } from "node:zlib";
import {
  parseCommit,
  parseTag,
  parseTree,
  sha1Of,
  storeObject,
  type GitObject,
  type ObjType,
  type Repo,
} from "./store.ts";

export const ZERO_OID = "0000000000000000000000000000000000000000";
const AGENT = "forge/1.0";

const UPLOAD_V0_CAPS =
  "side-band-64k thin-pack ofs-delta multi_ack_detailed shallow symref=HEAD:refs/heads/main agent=forge/1.0";
const RECEIVE_V0_CAPS = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0";

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

export function pktLine(payload: string | Buffer): Buffer {
  const data = typeof payload === "string" ? Buffer.from(payload, "utf8") : payload;
  const len = data.length + 4;
  if (len > 65520) throw new Error("pkt-line too long");
  const head = Buffer.alloc(4);
  head.write(len.toString(16).padStart(4, "0"), "ascii");
  return Buffer.concat([head, data]);
}

export const pktFlush = (): Buffer => Buffer.from("0000", "ascii");
export const pktDelim = (): Buffer => Buffer.from("0001", "ascii");

export interface Pkt {
  kind: "data" | "flush" | "delim" | "response-end";
  payload?: Buffer;
}

/** Decode a whole buffer of pkt-lines. Stops at the first non-pkt-line byte (raw pack data). */
export function decodePktLines(buf: Buffer): { packets: Pkt[]; rest: Buffer } {
  const packets: Pkt[] = [];
  let i = 0;
  while (i + 4 <= buf.length) {
    const header = buf.toString("ascii", i, i + 4);
    if (!/^[0-9a-f]{4}$/.test(header)) break;
    const len = parseInt(header, 16);
    if (len === 0) {
      packets.push({ kind: "flush" });
      i += 4;
      continue;
    }
    if (len === 1) {
      packets.push({ kind: "delim" });
      i += 4;
      continue;
    }
    if (len === 2) {
      packets.push({ kind: "response-end" });
      i += 4;
      continue;
    }
    if (len < 4 || i + len > buf.length) break;
    packets.push({ kind: "data", payload: buf.subarray(i + 4, i + len) });
    i += len;
  }
  return { packets, rest: buf.subarray(i) };
}

/** Split sideband pack bytes into pkt-lines with a band byte (chunk <= 32768 data bytes). */
export function sidebandPackets(data: Buffer, band = 1): Buffer[] {
  const out: Buffer[] = [];
  const CHUNK = 32768;
  if (data.length === 0) {
    out.push(pktLine(Buffer.concat([Buffer.from([band]), Buffer.alloc(0)])));
    return out;
  }
  for (let i = 0; i < data.length; i += CHUNK) {
    const slice = data.subarray(i, Math.min(i + CHUNK, data.length));
    out.push(pktLine(Buffer.concat([Buffer.from([band]), slice])));
  }
  return out;
}

// ---------------------------------------------------------------------------
// Pack reading (with delta resolution, including thin packs)
// ---------------------------------------------------------------------------

interface PackEntry {
  sha: string;
  type: ObjType;
  data: Buffer;
}

function readVarint(buf: Buffer, off: number): { value: number; next: number } {
  let value = buf[off] & 0x7f;
  let shift = 7;
  let i = off + 1;
  while (buf[i - 1] & 0x80) {
    value |= (buf[i] & 0x7f) << shift;
    shift += 7;
    i += 1;
  }
  return { value, next: i };
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readDeltaVarint = (): number => {
    let v = 0;
    let shift = 0;
    while (true) {
      const b = delta[pos++];
      v |= (b & 0x7f) << shift;
      shift += 7;
      if (!(b & 0x80)) break;
    }
    return v;
  };
  const srcSize = readDeltaVarint();
  const tgtSize = readDeltaVarint();
  if (srcSize !== base.length) throw new Error("delta base size mismatch");
  const out: Buffer[] = [];
  let written = 0;
  while (pos < delta.length) {
    const op = delta[pos++];
    if (op & 0x80) {
      let offset = 0;
      let size = 0;
      if (op & 0x01) offset = delta[pos++];
      if (op & 0x02) offset |= delta[pos++] << 8;
      if (op & 0x04) offset |= delta[pos++] << 16;
      if (op & 0x08) offset |= delta[pos++] << 24;
      if (op & 0x10) size = delta[pos++];
      if (op & 0x20) size |= delta[pos++] << 8;
      if (op & 0x40) size |= delta[pos++] << 16;
      if (size === 0) size = 0x10000;
      out.push(base.subarray(offset, offset + size));
      written += size;
    } else if (op !== 0) {
      out.push(delta.subarray(pos, pos + op));
      pos += op;
      written += op;
    } else {
      throw new Error("delta opcode 0");
    }
  }
  const result = Buffer.concat(out);
  if (written !== tgtSize || result.length !== tgtSize) throw new Error("delta target size mismatch");
  return result;
}

export function readPack(
  buf: Buffer,
  resolveBase?: (sha: string) => GitObject | undefined,
): PackEntry[] {
  if (buf.length < 12 || buf.subarray(0, 4).toString("ascii") !== "PACK") {
    throw new Error("not a packfile");
  }
  const version = buf.readUInt32BE(4);
  if (version !== 2) throw new Error(`unsupported pack version ${version}`);
  const count = buf.readUInt32BE(8);
  let off = 12;
  const entries: PackEntry[] = [];
  const byOffset = new Map<number, PackEntry>();
  const bySha = new Map<string, PackEntry>();
  for (let n = 0; n < count; n += 1) {
    const start = off;
    const first = buf[off++];
    const typeNum = (first >> 4) & 0x07;
    let size = first & 0x0f;
    let shift = 4;
    while (first & 0x80 || buf[off - 1] & 0x80) {
      const b = buf[off++];
      size |= (b & 0x7f) << shift;
      shift += 7;
      if (!(b & 0x80)) break;
    }
    let baseSha: string | undefined;
    let baseOffset: number | undefined;
    if (typeNum === 6) {
      // OFS_DELTA: negative offset varint
      let b = buf[off++];
      let offset = b & 0x7f;
      while (b & 0x80) {
        b = buf[off++];
        offset = ((offset + 1) << 7) | (b & 0x7f);
      }
      baseOffset = start - offset;
    } else if (typeNum === 7) {
      baseSha = buf.subarray(off, off + 20).toString("hex");
      off += 20;
    }
    const inflated = inflateEntry(buf, off);
    off = inflated.next;
    const raw = inflated.data;
    if (typeNum >= 1 && typeNum <= 4) {
      const type = (["", "commit", "tree", "blob", "tag"] as const)[typeNum] as ObjType;
      const entry: PackEntry = { sha: sha1Of(type, raw), type, data: raw };
      entries.push(entry);
      byOffset.set(start, entry);
      bySha.set(entry.sha, entry);
    } else if (typeNum === 6 || typeNum === 7) {
      let base: PackEntry | undefined;
      if (baseOffset !== undefined) base = byOffset.get(baseOffset);
      else if (baseSha !== undefined) {
        base = bySha.get(baseSha);
        if (!base && resolveBase) {
          const obj = resolveBase(baseSha);
          if (obj) base = { sha: baseSha, type: obj.type, data: obj.data };
        }
      }
      if (!base) throw new Error("delta base not found");
      const data = applyDelta(base.data, raw);
      const entry: PackEntry = { sha: sha1Of(base.type, data), type: base.type, data };
      entries.push(entry);
      byOffset.set(start, entry);
      bySha.set(entry.sha, entry);
    } else {
      throw new Error(`unknown pack object type ${typeNum}`);
    }
  }
  return entries;
}

// Inflate one zlib stream starting at off and report the offset just past
// its end. inflateSync ignores trailing bytes but throws on truncation, so
// the predicate "this prefix inflates cleanly" is monotonic in length and
// the stream end is found by binary search.
function inflateEntry(buf: Buffer, off: number): { data: Buffer; next: number } {
  const data = inflateSync(buf.subarray(off));
  let lo = 1;
  let hi = buf.length - off;
  while (lo < hi) {
    const mid = Math.floor((lo + hi) / 2);
    try {
      inflateSync(buf.subarray(off, off + mid));
      hi = mid;
    } catch {
      lo = mid + 1;
    }
  }
  return { data, next: off + lo };
}

// ---------------------------------------------------------------------------
// Pack writing (plain objects only — no deltas)
// ---------------------------------------------------------------------------

const TYPE_NUM: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };

export function writePack(objects: Array<{ type: ObjType; data: Buffer }>): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write("PACK", "ascii");
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const obj of objects) {
    const typeNum = TYPE_NUM[obj.type];
    const size = obj.data.length;
    const hb: number[] = [];
    let b = (typeNum << 4) | (size & 0x0f);
    let rest = size >> 4;
    if (rest > 0) b |= 0x80;
    hb.push(b);
    while (rest > 0) {
      b = rest & 0x7f;
      rest >>= 7;
      if (rest > 0) b |= 0x80;
      hb.push(b);
    }
    parts.push(Buffer.from(hb));
    parts.push(deflateSync(obj.data));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash("sha1").update(body).digest();
  return Buffer.concat([body, trailer]);
}

export function emptyPack(): Buffer {
  return writePack([]);
}

// ---------------------------------------------------------------------------
// Object graph: reachability with depth limits and have-exclusion
// ---------------------------------------------------------------------------

export interface CollectResult {
  /** objects to send, in commit-first order */
  objects: Array<{ type: ObjType; data: Buffer }>;
  shallow: string[];
  unshallow: string[];
}

/** Resolve a tag object to its ultimate target (commit or blob/tree). */
function peelToCommit(repo: Repo, sha: string): string | undefined {
  let cur = sha;
  const seen = new Set<string>();
  while (true) {
    if (seen.has(cur)) return undefined;
    seen.add(cur);
    const obj = repo.objects.get(cur);
    if (!obj) return undefined;
    if (obj.type === "commit") return cur;
    if (obj.type === "tag") {
      cur = parseTag(obj.data).object;
      continue;
    }
    return undefined;
  }
}

function collectTreeObjects(repo: Repo, treeSha: string, out: Map<string, { type: ObjType; data: Buffer }>, exclude?: Set<string>): void {
  const obj = repo.objects.get(treeSha);
  if (!obj || obj.type !== "tree" || out.has(treeSha)) return;
  if (exclude?.has(treeSha)) return;
  out.set(treeSha, obj);
  for (const entry of parseTree(obj.data)) {
    if (exclude?.has(entry.sha)) continue;
    const child = repo.objects.get(entry.sha);
    if (!child) continue;
    if (entry.mode === "40000") collectTreeObjects(repo, entry.sha, out, exclude);
    else if (!out.has(entry.sha)) out.set(entry.sha, child);
  }
}

function reachableCommits(repo: Repo, tips: string[], stopAt?: Set<string>): Set<string> {
  const seen = new Set<string>();
  const stack = [...tips];
  while (stack.length) {
    const sha = stack.pop()!;
    if (seen.has(sha)) continue;
    const obj = repo.objects.get(sha);
    if (!obj || obj.type !== "commit") continue;
    seen.add(sha);
    // A shallow boundary is a commit the client holds but whose parents it
    // does not: the walk stops there, it does not cross it.
    if (stopAt?.has(sha)) continue;
    for (const p of parseCommit(obj.data).parents) stack.push(p);
  }
  return seen;
}

/** Every object (commit/tree/blob) reachable from the given commits. */
function reachableObjects(repo: Repo, tips: string[], stopAt?: Set<string>): Set<string> {
  const out = new Set<string>();
  const commits = reachableCommits(repo, tips, stopAt);
  for (const sha of commits) {
    out.add(sha);
    const obj = repo.objects.get(sha);
    if (!obj || obj.type !== "commit") continue;
    const found = new Map<string, { type: ObjType; data: Buffer }>();
    collectTreeObjects(repo, parseCommit(obj.data).tree, found);
    for (const key of found.keys()) out.add(key);
  }
  return out;
}

export function collectObjects(
  repo: Repo,
  wants: string[],
  opts: { depth?: number; haves?: string[]; clientShallow?: string[] } = {},
): CollectResult {
  // Objects the client already holds: everything reachable from its haves,
  // stopping at its shallow boundaries (it holds the boundary commit, not
  // the boundary's parents).
  const stopAt = new Set(opts.clientShallow ?? []);
  const haveReachable = opts.haves?.length ? reachableCommits(repo, opts.haves, stopAt) : new Set<string>();
  const haveObjects = opts.haves?.length ? reachableObjects(repo, opts.haves, stopAt) : new Set<string>();
  // Traversal set: the depth-limited walk from the wants, ignoring haves.
  // Haves prune the PACK, never the walk — a deepen fetch must still reach
  // the parent of a have that is a shallow boundary.
  const sent = new Set<string>();
  const queue: Array<{ sha: string; depth: number }> = [];
  const tagObjects: string[] = [];
  for (const want of wants) {
    const obj = repo.objects.get(want);
    if (!obj) continue;
    if (obj.type === "tag") {
      tagObjects.push(want);
      const target = peelToCommit(repo, want);
      if (target) queue.push({ sha: target, depth: 0 });
      continue;
    }
    if (obj.type === "commit") queue.push({ sha: want, depth: 0 });
  }
  const maxDepth = opts.depth;
  while (queue.length) {
    const { sha, depth } = queue.shift()!;
    if (sent.has(sha)) continue;
    const obj = repo.objects.get(sha);
    if (!obj || obj.type !== "commit") continue;
    sent.add(sha);
    if (maxDepth === undefined || depth + 1 < maxDepth) {
      for (const p of parseCommit(obj.data).parents) queue.push({ sha: p, depth: depth + 1 });
    }
  }
  // New shallow boundaries: sent commits with an unsent parent that exists
  // and that the client does not already hold.
  const shallow: string[] = [];
  for (const sha of sent) {
    const obj = repo.objects.get(sha)!;
    for (const p of parseCommit(obj.data).parents) {
      if (!sent.has(p) && repo.objects.has(p) && !haveReachable.has(p)) {
        shallow.push(sha);
        break;
      }
    }
  }
  // Unshallow: client boundaries whose parents are now all sent or held.
  const unshallow: string[] = [];
  for (const b of opts.clientShallow ?? []) {
    if (!sent.has(b)) continue;
    const obj = repo.objects.get(b);
    if (!obj || obj.type !== "commit") continue;
    const parents = parseCommit(obj.data).parents;
    if (parents.every((p) => sent.has(p) || haveReachable.has(p))) unshallow.push(b);
  }
  const out = new Map<string, { type: ObjType; data: Buffer }>();
  for (const tagSha of tagObjects) {
    const obj = repo.objects.get(tagSha);
    if (obj && !haveObjects.has(tagSha)) out.set(tagSha, obj);
  }
  for (const sha of sent) {
    if (haveReachable.has(sha)) continue;
    const obj = repo.objects.get(sha)!;
    out.set(sha, obj);
    collectTreeObjects(repo, parseCommit(obj.data).tree, out, haveObjects);
  }
  return { objects: [...out.values()], shallow, unshallow };
}

// ---------------------------------------------------------------------------
// Advertisements
// ---------------------------------------------------------------------------

function sortedRefs(repo: Repo): Array<{ name: string; sha: string }> {
  const names = [...repo.refs.keys()].sort();
  return names.map((name) => ({ name, sha: repo.refs.get(name)! }));
}

function headTarget(repo: Repo): string {
  return `refs/heads/${repo.default_branch}`;
}

function refLinesV0(repo: Repo, caps: string): Buffer[] {
  const lines: Buffer[] = [];
  const refs = sortedRefs(repo);
  const head = headTarget(repo);
  const headSha = repo.refs.get(head);
  if (refs.length === 0) {
    lines.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
    return lines;
  }
  const headLine = headSha
    ? `${headSha} HEAD\0${caps}\n`
    : `${ZERO_OID} HEAD\0${caps}\n`;
  lines.push(pktLine(headLine));
  for (const ref of refs) {
    lines.push(pktLine(`${ref.sha} ${ref.name}\n`));
    const obj = repo.objects.get(ref.sha);
    if (obj?.type === "tag") {
      const target = parseTag(obj.data).object;
      const peeled = peelToCommit(repo, target) ?? target;
      lines.push(pktLine(`${peeled} ${ref.name}^{}\n`));
    }
  }
  return lines;
}

export function infoRefsUploadPack(repo: Repo, version2: boolean): Buffer {
  const preamble = pktLine(`# service=git-upload-pack\n`);
  if (version2) {
    return Buffer.concat([
      preamble,
      pktFlush(),
      pktLine("version 2\n"),
      pktLine("agent=forge/1.0\n"),
      pktLine("ls-refs=unborn\n"),
      pktLine("fetch=shallow filter\n"),
      pktLine("object-format=sha1\n"),
      pktFlush(),
    ]);
  }
  return Buffer.concat([preamble, pktFlush(), ...refLinesV0(repo, UPLOAD_V0_CAPS), pktFlush()]);
}

export function infoRefsReceivePack(repo: Repo): Buffer {
  const preamble = pktLine(`# service=git-receive-pack\n`);
  return Buffer.concat([preamble, pktFlush(), ...refLinesV0(repo, RECEIVE_V0_CAPS), pktFlush()]);
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth?: number;
  filter?: string;
  /** The client sent `done`: no further negotiation rounds. Real git then
   *  expects NO acknowledgments section — the response goes straight to
   *  shallow-info (if any) and packfile. Without `done` the server must
   *  answer the acknowledgments section and no packfile. */
  done: boolean;
}

function parseFetchArgsV2(body: Buffer): FetchArgs {
  const { packets } = decodePktLines(body);
  const args: FetchArgs = { wants: [], haves: [], shallow: [], done: false };
  let seenDelim = false;
  for (const p of packets) {
    if (p.kind === "flush") break;
    if (p.kind === "delim") {
      seenDelim = true;
      continue;
    }
    if (p.kind !== "data" || !p.payload) continue;
    const line = p.payload.toString("utf8").replace(/\n$/, "");
    if (!seenDelim) continue; // command= / agent= / object-format= lines
    if (line.startsWith("want ")) args.wants.push(line.slice(5).trim());
    else if (line.startsWith("have ")) args.haves.push(line.slice(5).trim());
    else if (line.startsWith("shallow ")) args.shallow.push(line.slice(8).trim());
    else if (line.startsWith("deepen ")) args.depth = parseInt(line.slice(7), 10) || undefined;
    else if (line.startsWith("filter ")) args.filter = line.slice(7).trim();
    else if (line === "done") args.done = true;
  }
  return args;
}

function parseFetchArgsV0(body: Buffer): FetchArgs {
  const { packets } = decodePktLines(body);
  const args: FetchArgs = { wants: [], haves: [], shallow: [], done: false };
  let section = 0; // 0 = wants, 1 = haves
  for (const p of packets) {
    if (p.kind === "flush") {
      section += 1;
      if (section > 1) break;
      continue;
    }
    if (p.kind !== "data" || !p.payload) continue;
    const line = p.payload.toString("utf8").replace(/\n$/, "");
    if (section === 0) {
      if (line.startsWith("want ")) {
        // first want line may carry space-separated capabilities
        const rest = line.slice(5);
        args.wants.push(rest.split(" ")[0].trim());
      } else if (line.startsWith("shallow ")) args.shallow.push(line.slice(8).trim());
      else if (line.startsWith("deepen ")) args.depth = parseInt(line.slice(7), 10) || undefined;
      else if (line.startsWith("filter ")) args.filter = line.slice(7).trim();
    } else {
      if (line.startsWith("have ")) args.haves.push(line.slice(5).trim());
      else if (line === "done") args.done = true;
    }
  }
  return args;
}

function buildFetchResponseV2(repo: Repo, args: FetchArgs): Buffer {
  // Negotiation round (no `done`): acknowledgments only, no packfile.
  if (!args.done) {
    return Buffer.concat([pktLine("acknowledgments\n"), pktLine("NAK\n"), pktFlush()]);
  }
  const collected = collectObjects(repo, args.wants, {
    depth: args.depth,
    haves: args.haves,
    clientShallow: args.shallow,
  });
  const parts: Buffer[] = [];
  // shallow-info only when there are lines to send (real git omits the
  // section otherwise); it ends with a delim, not a flush.
  if (collected.shallow.length || collected.unshallow.length) {
    parts.push(pktLine("shallow-info\n"));
    for (const s of collected.shallow) parts.push(pktLine(`shallow ${s}\n`));
    for (const s of collected.unshallow) parts.push(pktLine(`unshallow ${s}\n`));
    parts.push(pktDelim());
  }
  parts.push(pktLine("packfile\n"));
  const pack = writePack(collected.objects);
  parts.push(...sidebandPackets(pack, 1));
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

function buildFetchResponseV0(repo: Repo, args: FetchArgs): Buffer {
  const collected = collectObjects(repo, args.wants, {
    depth: args.depth,
    haves: args.haves,
    clientShallow: args.shallow,
  });
  const parts: Buffer[] = [];
  parts.push(pktLine("NAK\n"));
  for (const s of collected.shallow) parts.push(pktLine(`shallow ${s}\n`));
  for (const s of collected.unshallow) parts.push(pktLine(`unshallow ${s}\n`));
  const pack = writePack(collected.objects);
  parts.push(...sidebandPackets(pack, 1));
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

export function handleUploadPack(
  repo: Repo,
  body: Buffer,
  version2: boolean,
): { status: number; contentType: string; body: Buffer } {
  try {
    const args = version2 ? parseFetchArgsV2(body) : parseFetchArgsV0(body);
    if (args.wants.length === 0) {
      // No wants: answer an empty pack so the client still sees a packfile.
      // (No acknowledgments section: a fetch with no wants carries `done`.)
      const parts = version2
        ? [
            pktLine("packfile\n"),
            ...sidebandPackets(emptyPack(), 1),
            pktFlush(),
          ]
        : [pktLine("NAK\n"), ...sidebandPackets(emptyPack(), 1), pktFlush()];
      return { status: 200, contentType: "application/x-git-upload-pack-result", body: Buffer.concat(parts) };
    }
    const out = version2 ? buildFetchResponseV2(repo, args) : buildFetchResponseV0(repo, args);
    return { status: 200, contentType: "application/x-git-upload-pack-result", body: out };
  } catch {
    return { status: 400, contentType: "application/x-git-upload-pack-result", body: Buffer.alloc(0) };
  }
}

export function handleLsRefs(repo: Repo, body: Buffer): { status: number; contentType: string; body: Buffer } {
  try {
    const { packets } = decodePktLines(body);
    let peel = false;
    let symrefs = false;
    let unborn = false;
    const prefixes: string[] = [];
    let seenDelim = false;
    for (const p of packets) {
      if (p.kind === "flush") break;
      if (p.kind === "delim") {
        seenDelim = true;
        continue;
      }
      if (p.kind !== "data" || !p.payload || !seenDelim) continue;
      const line = p.payload.toString("utf8").replace(/\n$/, "");
      if (line === "peel") peel = true;
      else if (line === "symrefs") symrefs = true;
      else if (line === "unborn") unborn = true;
      else if (line.startsWith("ref-prefix ")) prefixes.push(line.slice(11));
    }
    const keep = (name: string): boolean =>
      prefixes.length === 0 || prefixes.some((pre) => name.startsWith(pre));
    const parts: Buffer[] = [];
    const head = headTarget(repo);
    const headSha = repo.refs.get(head);
    if (keep("HEAD")) {
      if (headSha) {
        const extra = symrefs ? ` symref-target:${head}` : "";
        parts.push(pktLine(`${headSha} HEAD${extra}\n`));
      } else if (unborn) {
        parts.push(pktLine(`unborn HEAD symref-target:${head}\n`));
      }
    }
    for (const ref of sortedRefs(repo)) {
      if (!keep(ref.name)) continue;
      let line = `${ref.sha} ${ref.name}`;
      if (peel) {
        const obj = repo.objects.get(ref.sha);
        if (obj?.type === "tag") {
          const peeled = peelToCommit(repo, parseTag(obj.data).object);
          if (peeled) line += ` peeled:${peeled}`;
        }
      }
      parts.push(pktLine(`${line}\n`));
    }
    parts.push(pktFlush());
    return { status: 200, contentType: "application/x-git-upload-pack-result", body: Buffer.concat(parts) };
  } catch {
    return { status: 400, contentType: "application/x-git-upload-pack-result", body: Buffer.alloc(0) };
  }
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

function isDescendant(repo: Repo, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const stack = [descendant];
  while (stack.length) {
    const sha = stack.pop()!;
    if (sha === ancestor) return true;
    if (seen.has(sha)) continue;
    seen.add(sha);
    const obj = repo.objects.get(sha);
    if (!obj || obj.type !== "commit") continue;
    for (const p of parseCommit(obj.data).parents) stack.push(p);
  }
  return false;
}

export function handleReceivePack(
  repo: Repo,
  body: Buffer,
): { status: number; contentType: string; body: Buffer } {
  const reportLines: string[] = [];
  const fail = (reason: string): { status: number; contentType: string; body: Buffer } => {
    reportLines.push(`unpack ${reason}`);
    // The report stream ends with a flush packet INSIDE band 1; a raw
    // flush then terminates the sideband stream.
    const report = Buffer.concat([...reportLines.map((l) => pktLine(`${l}\n`)), pktFlush()]);
    const out = Buffer.concat([...sidebandPackets(report, 1), pktFlush()]);
    return { status: 200, contentType: "application/x-git-receive-pack-result", body: out };
  };
  try {
    const { packets, rest } = decodePktLines(body);
    const commands: PushCommand[] = [];
    for (const p of packets) {
      if (p.kind === "flush") break;
      if (p.kind !== "data" || !p.payload) continue;
      let line = p.payload.toString("utf8").replace(/\n$/, "");
      const nul = line.indexOf("\0");
      if (nul >= 0) line = line.slice(0, nul);
      const parts = line.split(" ");
      if (parts.length < 3) continue;
      commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts[2] });
    }
    // Unpack the pack (if any) before applying ref updates.
    const hasPack = commands.some((c) => c.newOid !== ZERO_OID);
    if (hasPack && rest.length > 0) {
      try {
        const entries = readPack(rest, (sha) => repo.objects.get(sha));
        for (const e of entries) storeObject(repo.objects, e.type, e.data);
      } catch (err) {
        return fail(`unpack error: ${(err as Error).message}`);
      }
    }
    reportLines.push("unpack ok");
    for (const cmd of commands) {
      const current = repo.refs.get(cmd.ref);
      if (cmd.newOid === ZERO_OID) {
        // delete
        if (!current) {
          reportLines.push(`ng ${cmd.ref} remote ref does not exist`);
        } else {
          repo.refs.delete(cmd.ref);
          reportLines.push(`ok ${cmd.ref}`);
        }
        continue;
      }
      if (cmd.oldOid === ZERO_OID) {
        // create
        if (current) {
          reportLines.push(`ng ${cmd.ref} already exists`);
          continue;
        }
        if (!repo.objects.has(cmd.newOid)) {
          reportLines.push(`ng ${cmd.ref} object not found`);
          continue;
        }
        repo.refs.set(cmd.ref, cmd.newOid);
        reportLines.push(`ok ${cmd.ref}`);
        continue;
      }
      // update: fast-forward only
      if (current !== cmd.oldOid) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward`);
        continue;
      }
      if (!repo.objects.has(cmd.newOid)) {
        reportLines.push(`ng ${cmd.ref} object not found`);
        continue;
      }
      if (!isDescendant(repo, cmd.oldOid, cmd.newOid)) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward`);
        continue;
      }
      repo.refs.set(cmd.ref, cmd.newOid);
      reportLines.push(`ok ${cmd.ref}`);
    }
    const report = Buffer.concat([...reportLines.map((l) => pktLine(`${l}\n`)), pktFlush()]);
    const out = Buffer.concat([...sidebandPackets(report, 1), pktFlush()]);
    return { status: 200, contentType: "application/x-git-receive-pack-result", body: out };
  } catch (err) {
    return fail(`unpack error: ${(err as Error).message}`);
  }
}
