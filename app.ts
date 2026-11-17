// app.ts — HTTP server: routing, auth, body handling.
//
// Surfaces:
//   GET  /healthz                                  (no auth; the boot probe)
//   /{owner}/{repo}.git/info/refs                 git smart-HTTP advertisement
//   POST /{owner}/{repo}.git/git-upload-pack       upload-pack v0/v2 + ls-refs
//   POST /{owner}/{repo}.git/git-receive-pack      receive-pack v0
//   /api/v3/*                                      REST v3 (JSON)
//   POST /api/graphql                             GraphQL v4 (JSON)
//   /mcp                                          MCP JSON-RPC 2.0 (JSON)
//
// Auth: every surface except /healthz requires `Authorization: Bearer
// <token>`. When MAKEFUN_SHOP_TOKEN is set the token must match it;
// otherwise any non-empty Bearer token is accepted (local testing).
// A repo under any owner other than the token owner answers 404, never 403.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { handleGraphQL } from "./graphql.ts";
import {
  handleLsRefs,
  handleReceivePack,
  handleUploadPack,
  infoRefsReceivePack,
  infoRefsUploadPack,
  pktLine,
} from "./git.ts";
import { handleMcp } from "./mcp.ts";
import { handleRest } from "./rest.ts";
import { Store, VIEWER_LOGIN } from "./store.ts";

const MAX_BODY_BYTES = 64 * 1024 * 1024;

function shopToken(): string {
  return process.env.MAKEFUN_SHOP_TOKEN ?? "";
}

function isAuthorized(req: IncomingMessage): boolean {
  const header = req.headers.authorization;
  if (!header || !header.startsWith("Bearer ")) return false;
  const token = header.slice("Bearer ".length).trim();
  if (!token) return false;
  const expected = shopToken();
  return expected === "" || token === expected;
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on("data", (chunk: Buffer) => {
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        reject(new Error("request body too large"));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}

function sendJson(res: ServerResponse, status: number, body: unknown, requestId: string, extra?: Record<string, string>): void {
  const payload = body === null || body === undefined ? "" : JSON.stringify(body);
  res.writeHead(status, {
    "content-type": "application/json",
    "x-github-request-id": requestId,
    ...(extra ?? {}),
  });
  res.end(payload);
}

function sendBuffer(res: ServerResponse, status: number, contentType: string, body: Buffer, requestId: string, extra?: Record<string, string>): void {
  res.writeHead(status, {
    "content-type": contentType,
    "x-github-request-id": requestId,
    ...(extra ?? {}),
  });
  res.end(body);
}

export function createApp(): Server {
  const store = new Store();

  const server = createServer(async (req, res) => {
    const requestId = store.requestId();
    try {
      const url = new URL(req.url ?? "/", "http://127.0.0.1");
      const path = url.pathname;
      const method = req.method ?? "GET";

      // Boot probe: no auth.
      if (path === "/healthz") {
        res.writeHead(200, { "content-type": "text/plain", "x-github-request-id": requestId });
        res.end("ok");
        return;
      }

      if (!isAuthorized(req)) {
        if (path.endsWith(".git/info/refs") || path.includes(".git/")) {
          res.writeHead(401, { "content-type": "text/plain", "x-github-request-id": requestId });
          res.end("Unauthorized");
        } else {
          sendJson(res, 401, { message: "Requires authentication" }, requestId);
        }
        return;
      }

      // ---- git smart HTTP ----
      const gitMatch = /^\/([^/]+)\/([^/]+)\.git\/(.+)$/.exec(path);
      if (gitMatch) {
        const owner = decodeURIComponent(gitMatch[1]!);
        const repoName = decodeURIComponent(gitMatch[2]!);
        const action = gitMatch[3]!;
        const repo = owner === VIEWER_LOGIN ? store.getRepo(owner, repoName) : undefined;
        if (!repo) {
          res.writeHead(404, { "content-type": "text/plain", "x-github-request-id": requestId });
          res.end("Not Found");
          return;
        }
        if (action === "info/refs") {
          const service = url.searchParams.get("service") ?? "";
          if (service === "git-upload-pack") {
            const version2 = (req.headers["git-protocol"] ?? "") === "version=2";
            const body = infoRefsUploadPack(repo, version2);
            sendBuffer(res, 200, `application/x-${service}-advertisement`, body, requestId);
            return;
          }
          if (service === "git-receive-pack") {
            const body = infoRefsReceivePack(repo);
            sendBuffer(res, 200, `application/x-${service}-advertisement`, body, requestId);
            return;
          }
          res.writeHead(400, { "content-type": "text/plain", "x-github-request-id": requestId });
          res.end("Bad Request");
          return;
        }
        if (action === "git-upload-pack" && method === "POST") {
          const body = await readBody(req);
          const version2 = (req.headers["git-protocol"] ?? "") === "version=2";
          let out;
          if (version2 && body.subarray(0, 64).toString("utf8").includes("command=ls-refs")) {
            out = handleLsRefs(repo, body);
          } else {
            out = handleUploadPack(repo, body, version2);
          }
          sendBuffer(res, out.status, out.contentType, out.body, requestId);
          return;
        }
        if (action === "git-receive-pack" && method === "POST") {
          const body = await readBody(req);
          const out = handleReceivePack(repo, body);
          sendBuffer(res, out.status, out.contentType, out.body, requestId);
          return;
        }
        res.writeHead(404, { "content-type": "text/plain", "x-github-request-id": requestId });
        res.end("Not Found");
        return;
      }

      // ---- GraphQL ----
      if (path === "/api/graphql") {
        if (method !== "POST") {
          sendJson(res, 405, { message: "Method Not Allowed" }, requestId);
          return;
        }
        const body = await readBody(req);
        const out = handleGraphQL(store, body);
        sendJson(res, out.status, out.body, requestId);
        return;
      }

      // ---- MCP ----
      if (path === "/mcp") {
        if (method === "DELETE") {
          res.writeHead(204, { "x-github-request-id": requestId });
          res.end();
          return;
        }
        if (method !== "POST") {
          sendJson(res, 405, { message: "Method Not Allowed" }, requestId);
          return;
        }
        const body = await readBody(req);
        const headers: Record<string, string | undefined> = {};
        for (const [k, v] of Object.entries(req.headers)) headers[k] = Array.isArray(v) ? v[0] : v;
        const out = handleMcp(store, body, headers);
        if (typeof out.body === "string") {
          res.writeHead(out.status, { "content-type": "application/json", "x-github-request-id": requestId, ...(out.headers ?? {}) });
          res.end(out.body);
        } else {
          sendJson(res, out.status, out.body, requestId, out.headers);
        }
        return;
      }

      // ---- REST v3 ----
      if (path === "/api/v3" || path.startsWith("/api/v3/")) {
        const body = method === "GET" || method === "DELETE" ? Buffer.alloc(0) : await readBody(req);
        const origin = `http://${req.headers.host ?? "127.0.0.1"}`;
        const out = handleRest(store, method, path, url.searchParams, body, origin);
        if (out.status === 204) {
          res.writeHead(204, { "x-github-request-id": requestId });
          res.end();
          return;
        }
        sendJson(res, out.status, out.body, requestId, out.headers);
        return;
      }

      // Unknown route: JSON 404, never HTML.
      sendJson(res, 404, { message: "Not Found" }, requestId);
    } catch (err) {
      // Never crash the process on a bad request; answer 400/500 and stay up.
      if (!res.headersSent) {
        sendJson(res, 400, { message: err instanceof Error ? err.message : "Bad Request" }, requestId);
      } else {
        res.end();
      }
    }
  });

  server.on("connection", (socket) => {
    socket.setNoDelay(true);
  });
  server.keepAliveTimeout = 5000;
  server.headersTimeout = 10000;
  return server;
}

// Re-exported for the git handlers' pkt-line use in tests.
export { pktLine };
