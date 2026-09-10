import homepage from "../index.html";
import { resolve } from "node:path";
import { createDevelopmentProxy } from "./dev-proxy";
import { serverOptions } from "./server-options";

const options = serverOptions(process.argv.slice(2), 3000);
const proxy = createDevelopmentProxy({
  target: process.env.LUDOCK_DEV_API_ORIGIN || "http://127.0.0.1:3001",
  instance: process.env.LUDOCK_DEV_INSTANCE,
});
const publicDirectory = resolve(import.meta.dir, "../public");
const publicFiles = Object.fromEntries(
  Array.from(new Bun.Glob("**/*").scanSync({ cwd: publicDirectory, onlyFiles: true }))
    .map(file => [`/${file}`, Bun.file(resolve(publicDirectory, file))]),
);
const server = Bun.serve({
  ...options,
  idleTimeout: 0,
  // The backend owns upload limits; the proxy must not reject a valid large stream.
  maxRequestBodySize: Number.MAX_SAFE_INTEGER,
  development: { hmr: true, console: false },
  routes: {
    ...publicFiles,
    "/api": proxy.fetch,
    "/api/*": proxy.fetch,
    "/ws": proxy.fetch,
    "/ws/*": proxy.fetch,
    "/*": homepage,
  },
  websocket: proxy.websocket,
});
console.log(`Ludock frontend development: ${server.url}`);
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.once(signal, () => { proxy.stop(); server.stop(true); });
}
