import { realpath, stat } from "node:fs/promises";
import { extname, resolve, sep } from "node:path";

export function staticFiles(directory: string) {
  let canonicalRoot: string | undefined;
  return async (request: Request): Promise<Response> => {
    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response("Method not allowed", { status: 405, headers: { Allow: "GET, HEAD" } });
    }
    let pathname: string;
    try {
      pathname = decodeURIComponent(new URL(request.url).pathname);
    } catch {
      return new Response("Invalid path", { status: 400 });
    }
    // Missing API endpoints and assets must not receive the SPA document.
    if (/^\/(?:api|ws)(?:\/|$)/i.test(pathname) || pathname.includes("\0") ||
      pathname.split("/").some((part) => part.startsWith(".")))
      return new Response("Not found", { status: 404 });
    try {
      canonicalRoot ||= await realpath(resolve(directory));
    } catch {
      return new Response("Frontend build unavailable", { status: 503 });
    }
    const root = canonicalRoot;
    const requested = resolve(root, `.${pathname}`);
    if (requested !== root && !requested.startsWith(`${root}${sep}`)) {
      return new Response("Not found", { status: 404 });
    }
    let file = requested;
    try {
      const resolved = await realpath(file);
      if (resolved !== root && !resolved.startsWith(`${root}${sep}`)) {
        return new Response("Not found", { status: 404 });
      }
      file = (await stat(resolved)).isFile() ? resolved : resolve(root, "index.html");
    } catch {
      if (extname(requested)) return new Response("Not found", { status: 404 });
      file = resolve(root, "index.html");
    }
    try {
      file = await realpath(file);
      if (!file.startsWith(`${root}${sep}`) || !(await stat(file)).isFile()) {
        return new Response("Not found", { status: 404 });
      }
    } catch {
      return new Response("Frontend build unavailable", { status: 503 });
    }
    const asset = Bun.file(file);
    if (!(await asset.exists())) return new Response("Frontend build unavailable", { status: 503 });
    return new Response(request.method === "HEAD" ? null : asset, {
      headers: { "Content-Type": asset.type, "Cache-Control": "no-cache", "X-Content-Type-Options": "nosniff" },
    });
  };
}
