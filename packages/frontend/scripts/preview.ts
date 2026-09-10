import { resolve } from "node:path";
import { serverOptions } from "./server-options";
import { staticFiles } from "./static-files";

const options = serverOptions(process.argv.slice(2), Number(process.env.LUDOCK_E2E_PORT || 4179));
const directory = resolve(import.meta.dir, "../dist");
if (!(await Bun.file(resolve(directory, "index.html")).exists())) {
  throw new Error("Build the frontend before starting preview.");
}
const server = Bun.serve({ ...options, fetch: staticFiles(directory), development: false });
console.log(`Ludock frontend preview: ${server.url}`);
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.once(signal, () => { server.stop(true); });
}
