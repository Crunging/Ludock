import fs from "node:fs/promises";
import { constants as C } from "node:fs";
const sources = /** @type {{source: string, destination: string}[]} */ (
  JSON.parse(process.argv[1])
);
/** @type {import("node:fs/promises").FileHandle[]} */
const retained = [];
(async () => {
  /** @type {Record<string, {dev: string, ino: string}>} */
  const identities = {};
  for (const source of sources) {
    if (
      !source.source.startsWith("/") ||
      source.source.includes("\0") ||
      source.source.split("/").includes("..")
    )
      throw new Error();
    let current = await fs.open(
      "/host",
      C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW,
    );
    for (const part of source.source.split("/").filter(Boolean)) {
      const next = await fs.open(
        "/proc/self/fd/" + current.fd + "/" + part,
        C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW,
      );
      await current.close();
      current = next;
    }
    const info = await current.stat({ bigint: true });
    retained.push(current);
    identities[source.destination] = {
      dev: String(info.dev),
      ino: String(info.ino),
    };
  }
  process.stdout.write(JSON.stringify({ identities }) + "\n");
  setInterval(() => {}, 3600000);
  if (Number(process.argv[2]) > 0)
    setTimeout(() => process.exit(0), Number(process.argv[2]));
})().catch(() => {
  process.stdout.write(JSON.stringify({ error: "unsafe-source" }) + "\n");
  process.exitCode = 1;
});
