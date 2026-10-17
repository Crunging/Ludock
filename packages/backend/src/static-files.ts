import { realpath, stat } from "node:fs/promises";
import { extname, relative, resolve, sep } from "node:path";

// The frontend build emits content-hashed bundles under assets/ and keeps
// stable public files and the SPA document at the root.
const IMMUTABLE_PREFIX = `assets${sep}`;
// Precompressed siblings written by the frontend build, in preference order.
const ENCODINGS = [
  { name: "zstd", suffix: ".zst" },
  { name: "gzip", suffix: ".gz" },
] as const;

function acceptedEncodings(header: string | null): Set<string> {
  const accepted = new Set<string>();
  for (const part of (header ?? "").toLowerCase().split(",")) {
    const [name, ...parameters] = part.split(";").map((value) => value.trim());
    const quality = parameters.find((parameter) => parameter.startsWith("q="));
    if (name && !(quality && Number(quality.slice(2)) === 0)) accepted.add(name);
  }
  return accepted;
}

/** A sibling of a validated file stays inside the root unless it is a link. */
async function regularFile(file: string): Promise<boolean> {
  try {
    return await realpath(file) === file && (await stat(file)).isFile();
  } catch {
    return false;
  }
}

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
    const inside = (file: string) => file.startsWith(`${root}${sep}`);
    const requested = resolve(root, `.${pathname}`);
    if (requested !== root && !inside(requested)) {
      return new Response("Not found", { status: 404 });
    }
    let file: string;
    try {
      const resolved = await realpath(requested);
      if (resolved !== root && !inside(resolved)) {
        return new Response("Not found", { status: 404 });
      }
      file = (await stat(resolved)).isFile() ? resolved : resolve(root, "index.html");
    } catch {
      if (extname(requested)) return new Response("Not found", { status: 404 });
      file = resolve(root, "index.html");
    }
    try {
      file = await realpath(file);
      if (!inside(file) || !(await stat(file)).isFile()) {
        return new Response("Not found", { status: 404 });
      }
    } catch {
      return new Response("Frontend build unavailable", { status: 503 });
    }
    const asset = Bun.file(file);
    const headers = new Headers({
      "Content-Type": asset.type,
      "Cache-Control": relative(root, file).startsWith(IMMUTABLE_PREFIX)
        ? "public, max-age=31536000, immutable"
        : "no-cache",
      "X-Content-Type-Options": "nosniff",
    });
    let body = asset;
    const accepted = acceptedEncodings(request.headers.get("accept-encoding"));
    for (const encoding of ENCODINGS) {
      const candidate = `${file}${encoding.suffix}`;
      if (!(await regularFile(candidate))) continue;
      // The response varies whenever a compressed form exists, even when
      // this client did not accept it.
      headers.set("Vary", "Accept-Encoding");
      if (!accepted.has(encoding.name)) continue;
      body = Bun.file(candidate);
      headers.set("Content-Encoding", encoding.name);
      break;
    }
    return new Response(request.method === "HEAD" ? null : body, { headers });
  };
}
