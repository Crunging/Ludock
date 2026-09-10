import assert from "node:assert/strict";
import { mkdtemp, mkdir, copyFile, readFile, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { it } from "bun:test";

it("drains the old backend before reloading and after repeated shutdown signals", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "ludock-watch-test-"));
  let child;
  try {
    for (const folder of ["scripts", "packages/backend/src", "packages/shared/dist"]) {
      await mkdir(path.join(directory, folder), { recursive: true });
    }
    await copyFile(new URL("../watch-backend.mjs", import.meta.url), path.join(directory, "scripts/watch-backend.mjs"));
    const events = path.join(directory, "events");
    const entry = path.join(directory, "packages/backend/src/index.ts");
    const source = `
      import { appendFileSync, openSync, closeSync, unlinkSync } from "node:fs";
      const events = ${JSON.stringify(events)};
      const lock = events + ".lock";
      const fd = openSync(lock, "wx");
      appendFileSync(events, "started\\n");
      const timer = setInterval(() => {}, 1000);
      let closing = false;
      process.on("SIGTERM", () => {
        if (closing) return;
        closing = true;
        appendFileSync(events, "stopping\\n");
        setTimeout(() => {
          closeSync(fd); unlinkSync(lock);
          appendFileSync(events, "drained\\n");
          clearInterval(timer);
        }, 250);
      });
    `;
    await writeFile(entry, source);
    child = Bun.spawn([process.execPath, "scripts/watch-backend.mjs"], {
      cwd: directory, stdin: "ignore", stdout: "ignore", stderr: "pipe",
    });
    const waitFor = async (expected) => {
      for (let attempt = 0; attempt < 200; attempt++) {
        const actual = await readFile(events, "utf8").catch(() => "");
        if (actual === expected) return;
        if (child.exitCode !== null) throw new Error(`Watcher exited: ${await new Response(child.stderr).text()}`);
        await Bun.sleep(10);
      }
      assert.equal(await readFile(events, "utf8"), expected);
    };
    await waitFor("started\n");
    await writeFile(entry, source + "\n// source changed\n");
    await waitFor("started\nstopping\ndrained\nstarted\n");
    child.kill("SIGTERM");
    await waitFor("started\nstopping\ndrained\nstarted\nstopping\n");
    child.kill("SIGTERM");
    child.kill("SIGINT");
    await Bun.sleep(20);
    assert.equal(child.exitCode, null, "Watcher must remain alive while the backend drains");
    assert.equal(await child.exited, 0);
    assert.equal(await readFile(events, "utf8"), "started\nstopping\ndrained\nstarted\nstopping\ndrained\n");
  } finally {
    if (child?.exitCode === null) {
      child.kill("SIGTERM");
      await child.exited;
    }
    await rm(directory, { recursive: true, force: true });
  }
}, 10_000);
