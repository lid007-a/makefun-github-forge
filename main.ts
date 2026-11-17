// main.ts — the entry <-> env lifecycle, as env/src/wire.ts settles it:
// bind an ephemeral port, tell the env the port it bound, then poll until
// the env says the trial is over and exit 0.
//
// This entrypoint is a thin launcher: the forge itself is the statically
// linked Go binary `forge` committed beside this file. main.ts spawns it,
// reads the port it bound from its stdout, and owns the register / done
// lifecycle exactly as the Node forge's main.ts did.
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial
//      container). Without it there is nothing to talk to, so the program
//      exits 1 at once.
//   2. Spawn ./forge (see spawnForge below). It binds an ephemeral port and
//      prints FORGE_PORT=<n> as its first stdout line. Never bind 8080 —
//      the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the
//      address the request came from plus N as your base URL and dials it
//      from then on. The first register wins; a 409 means another entry
//      already registered, and this process keeps waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env
//      answers at once ({"done": false} while the suite runs), so this is
//      a poll, not a long-held request. A failed or slow poll is retried
//      with a short exponential backoff; the process never exits before the
//      env says done.
//   5. Kill the Go child, then exit 0. The harness then stops the env,
//      which writes the export the scorer reads.
//
// main.ts imports Node built-ins only: no package, no model call.

import { spawn, type ChildProcess } from 'node:child_process';
import { access, chmod, copyFile, constants } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

/** How long one register or done request may take before it is abandoned and retried. */
const REQUEST_TIMEOUT_MS = 30_000;
/** The first wait between retries, and the longest. */
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** The wait after `attempt` failed requests: exponential from RETRY_INITIAL_MS, capped. */
function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

/**
 * Spawn the Go forge binary and resolve with the port it bound.
 *
 * The trial runs /bot read-only, and the committed binary may arrive with
 * mode 0644 (not executable). If it is not directly executable, copy it to
 * the temp dir, chmod 0755, and spawn the copy. The binary prints
 * `FORGE_PORT=<n>` as its first stdout line; that line is parsed here.
 * Its stderr is forwarded to this process's stderr for debugging.
 */
async function spawnForge(): Promise<{ child: ChildProcess; port: number }> {
  const here = dirname(fileURLToPath(import.meta.url));
  const binary = join(here, 'forge');
  let execPath = binary;
  try {
    await access(binary, constants.X_OK);
  } catch {
    // Not executable (e.g. mode 0644 in a read-only /bot): stage a copy.
    const staged = join(tmpdir(), `github-forge-go-${process.pid}`);
    await copyFile(binary, staged);
    await chmod(staged, 0o755);
    execPath = staged;
    process.stderr.write(`[entry] staged forge binary at ${staged}\n`);
  }

  const child = spawn(execPath, [], { stdio: ['ignore', 'pipe', 'pipe'] });
  child.stderr?.on('data', (chunk: Buffer) => process.stderr.write(chunk));

  const port = await new Promise<number>((resolve, reject) => {
    let buf = '';
    let settled = false;
    const done = (fn: () => void) => {
      if (!settled) {
        settled = true;
        fn();
      }
    };
    child.stdout?.on('data', (chunk: Buffer) => {
      buf += chunk.toString('utf8');
      const nl = buf.indexOf('\n');
      if (nl >= 0) {
        const line = buf.slice(0, nl).trim();
        const m = /^FORGE_PORT=(\d+)$/.exec(line);
        if (m) done(() => resolve(parseInt(m[1]!, 10)));
        // Any other first stdout line is ignored; keep waiting for the port.
        buf = buf.slice(nl + 1);
      }
    });
    child.once('error', (err) => done(() => reject(err)));
    child.once('exit', (code, signal) => {
      done(() => reject(new Error(`forge exited before printing its port (code ${code}, signal ${signal})`)));
    });
    setTimeout(() => done(() => reject(new Error('timed out waiting for the forge to print its port'))), 30_000).unref();
  });
  return { child, port };
}

/**
 * POST the bound port until the env accepts it. 200 is the normal answer; 409 means a first register
 * already won, which is not this process's to fix. 400 and 405 are contract errors and end the program;
 * a network error or another status is retried, since the env may still be starting.
 */
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

/** Poll GET /done until it answers {"done":true}; never return before that. */
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

/** Bound the port, register it, then wait for the env: the whole life of the process. */
async function main(): Promise<void> {
  const { child, port } = await spawnForge();
  process.stdout.write(`[entry] listening on port ${port}\n`);
  process.stdout.write('[entry] github-forge-go 1.0.0 (git smart HTTP, REST v3, GraphQL v4, MCP)\n');

  // If the forge dies before the trial ends, the trial cannot proceed.
  const childGone = new Promise<never>((_, reject) => {
    child.once('exit', (code, signal) => {
      reject(new Error(`forge exited early (code ${code}, signal ${signal})`));
    });
  });

  await register(port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await Promise.race([waitForDone(), childGone]);
  child.kill('SIGTERM');
  // Give it a moment to exit, then force it.
  await Promise.race([
    new Promise<void>((resolve) => child.once('exit', () => resolve())),
    sleep(2_000).then(() => {
      try {
        child.kill('SIGKILL');
      } catch {
        // Already gone.
      }
    }),
  ]);
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
