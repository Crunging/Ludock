#!/usr/bin/env bun
import { watch } from "node:fs";
import path from "node:path";

const repository = path.resolve(import.meta.dir, "..");

// Bun's --watch invokes signal handlers but can restart before asynchronous
// cleanup finishes. Operations and file helpers must drain before a new backend
// opens the same database, so this watcher explicitly waits for each Bun child.
export async function watchBackend(directory = repository, env = process.env) {
  let child;
  let restarting;
  let requested = false;
  let stopping = false;
  let timer;
  let finish;
  const done = new Promise((resolve) => { finish = resolve; });
  const watchers = [];
  const retiring = new Set();

  const stop = async () => {
    if (stopping) return done;
    stopping = true;
    clearTimeout(timer);
    watchers.forEach((watcher) => watcher.close());
    if (child) {
      child.kill("SIGTERM");
      await child.exited;
    }
    await restarting;
    finish();
    return done;
  };
  const fail = (error) => {
    console.error(`Backend watcher: ${error.message}`);
    process.exitCode = 1;
    void stop();
  };
  const restart = () => {
    requested = true;
    if (restarting || stopping) return;
    restarting = (async () => {
      while (requested && !stopping) {
        requested = false;
        if (child) {
          retiring.add(child);
          child.kill("SIGTERM");
          await child.exited;
          child = undefined;
        }
        if (stopping) return;
        const launched = Bun.spawn([process.execPath, "src/index.ts"], {
          cwd: path.join(directory, "packages/backend"),
          env,
          stdin: "ignore",
          stdout: "inherit",
          stderr: "inherit",
          detached: process.platform !== "win32",
        });
        child = launched;
        void launched.exited.then((code) => {
          if (retiring.delete(launched) || stopping) return;
          fail(new Error(`process stopped (${launched.signalCode || code}).`));
        }, fail);
      }
    })().catch(fail).finally(() => { restarting = undefined; });
  };
  const changed = (_event, name) => {
    // Declaration and source-map output does not change running code.
    if (name && (/\.d\.ts$|\.map$/.test(String(name)))) return;
    clearTimeout(timer);
    timer = setTimeout(restart, 100);
  };
  const onSignal = () => { void stop(); };
  // Keep handling repeated interrupts until the child has finished cleanup.
  process.on("SIGINT", onSignal);
  process.on("SIGTERM", onSignal);
  try {
    for (const folder of ["packages/backend/src", "packages/shared/src"]) {
      const watcher = watch(path.join(directory, folder), { recursive: true }, changed);
      watcher.on("error", fail);
      watchers.push(watcher);
    }
    restart();
    await done;
  } finally {
    await stop();
    process.removeListener("SIGINT", onSignal);
    process.removeListener("SIGTERM", onSignal);
  }
}

if (import.meta.main) {
  try {
    await watchBackend();
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
