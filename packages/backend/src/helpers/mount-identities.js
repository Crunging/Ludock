import fs from "node:fs/promises";
import { constants as C } from "node:fs";
(async () => {
  for (const [name, expected] of Object.entries(
    /** @type {Record<string, {dev: string, ino: string}>} */ (
      JSON.parse(process.argv[1])
    ),
  )) {
    let file = await fs.open("/", C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW);
    for (const part of name.split("/").filter(Boolean)) {
      const next = await fs.open(
        "/proc/self/fd/" + file.fd + "/" + part,
        C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW,
      );
      await file.close();
      file = next;
    }
    const info = await file.stat({ bigint: true });
    await file.close();
    if (String(info.dev) !== expected.dev || String(info.ino) !== expected.ino)
      throw new Error();
  }
})().catch(() => {
  process.exitCode = 1;
});
