import { spawn, type ChildProcess } from 'node:child_process';
import { chmodSync, copyFileSync, existsSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT_DIR = dirname(fileURLToPath(import.meta.url));
const SOURCE_BIN = join(ROOT_DIR, 'bin', 'forge');

function ensureExecutablePath(): string {
  if (!existsSync(SOURCE_BIN)) {
    throw new Error(`vendored forge binary missing at ${SOURCE_BIN}`);
  }
  return SOURCE_BIN;
}

function runBinary(binPath: string): Promise<number> {
  return new Promise((resolve, reject) => {
    const child: ChildProcess = spawn(binPath, [], {
      stdio: 'inherit',
      env: process.env,
    });
    const forward = (sig: NodeJS.Signals): void => {
      if (!child.killed) {
        child.kill(sig);
      }
    };
    const onTerm = (): void => forward('SIGTERM');
    const onInt = (): void => forward('SIGINT');
    process.on('SIGTERM', onTerm);
    process.on('SIGINT', onInt);

    const cleanup = (): void => {
      process.off('SIGTERM', onTerm);
      process.off('SIGINT', onInt);
    };

    child.once('error', (err) => {
      cleanup();
      reject(err);
    });
    child.once('exit', (code, signal) => {
      cleanup();
      if (signal !== null) {
        resolve(0);
        return;
      }
      resolve(code ?? 0);
    });
  });
}

async function main(): Promise<void> {
  const primary = ensureExecutablePath();
  try {
    const code = await runBinary(primary);
    process.exit(code);
  } catch (err) {
    const code = (err as NodeJS.ErrnoException | undefined)?.code;
    if (code !== 'EACCES' && code !== 'EPERM') {
      throw err;
    }
    const fallback = join(tmpdir(), `makefun-forge-${process.pid}`);
    copyFileSync(primary, fallback);
    chmodSync(fallback, 0o755);
    try {
      const exitCode = await runBinary(fallback);
      process.exit(exitCode);
    } finally {
      try {
        rmSync(fallback, { force: true });
      } catch {
        // Ignore best-effort cleanup errors on exit.
      }
    }
  }
}

main().catch((err: unknown) => {
  const message = err instanceof Error ? err.message : String(err);
  process.stderr.write(`[entry] ${message}\n`);
  process.exit(1);
});
