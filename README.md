# makefun-github-forge (Go)

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone GitHub-compatible git forge in Go, using only the Go standard
library: git smart HTTP (upload-pack v0/v2, receive-pack), REST API v3,
GraphQL v4 and MCP, all sharing one deterministic in-memory store. The Go
program is the statically linked binary `forge`, committed beside the
sources; the entrypoint is `main.ts` (Node 22), which spawns the binary,
registers its port with the trial env, and waits for the env to say done.

This is a speed port of the Node/TypeScript forge in
`makefun-github-forge`: same behavior, same wire formats, with Go's true
parallelism for the concurrent clone workload and a single-flight pack
cache so 50 concurrent clones of one tip build the pack once.

## Layout

- `main.ts` — Node 22 entrypoint: spawns `forge`, registers its port,
  polls the env's `/done`, then kills the child and exits 0. Robust to
  the binary arriving mode 0644: it stages a copy in the temp dir and
  chmods it 0755.
- `forge` — the statically linked Linux amd64 Go binary
  (`CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`).
- `main.go` — Go server entrypoint: binds the port (`--port`, else
  `FORGE_PORT`, else ephemeral) and serves.
- `store.go` — in-memory data model (repos, issues, PRs, git objects).
- `git.go` — git smart HTTP: pkt-line codec, pack read/write, object
  graph, upload-pack v0/v2, receive-pack v0.
- `rest.go` — REST API v3.
- `graphql.go` — GraphQL v4 (parser + executor).
- `mcp.go` — MCP JSON-RPC 2.0 (Streamable HTTP).
- `app.go` — HTTP server: routing, auth, body handling.

## Build it

```sh
export GOCACHE=$PWD/.gocache   # if the default cache is not writable
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
```

## Run it

```sh
MAKEFUN_ENV_URL=http://127.0.0.1:8080 node main.ts
```

Or run the binary directly for local testing (any Bearer token is
accepted when `MAKEFUN_SHOP_TOKEN` is unset):

```sh
FORGE_PORT=18123 ./forge
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=pgdcswan&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
