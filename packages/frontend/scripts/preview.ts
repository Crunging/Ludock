import { resolve } from "node:path";
import { staticFiles } from "../../backend/src/static-files";

const directory = resolve(import.meta.dir, "../dist");
if (!(await Bun.file(resolve(directory, "index.html")).exists())) {
  throw new Error("Build the frontend before starting preview.");
}
const server = Bun.serve({
  hostname: "127.0.0.1",
  port: Number(process.env.LUDOCK_E2E_PORT || 4179),
  fetch: staticFiles(directory),
  development: false,
});
console.log(`Ludock frontend preview: ${server.url}`);
for (const signal of ["SIGINT", "SIGTERM"] as const) process.once(signal, () => void server.stop(true));
