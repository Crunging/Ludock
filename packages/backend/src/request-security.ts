import type { IncomingMessage } from "node:http";

type RequestMetadata = Pick<IncomingMessage, "headers" | "socket">;

export function isExternalHttpsRequest(request: RequestMetadata): boolean {
  return externalProtocol(request) === "https:";
}

export function isSameOriginRequest(request: RequestMetadata): boolean {
  const origin = parseRequestOrigin(request);
  return (
    origin !== null &&
    origin.protocol === externalProtocol(request) &&
    origin.host.toLowerCase() === request.headers.host?.toLowerCase()
  );
}

function externalProtocol(request: RequestMetadata): "http:" | "https:" {
  if (
    (request.socket as IncomingMessage["socket"] & { encrypted?: boolean })
      .encrypted
  ) {
    return "https:";
  }

  const forwardedProtocol = String(
    request.headers["x-forwarded-proto"] || ""
  )
    .split(",")[0]
    .trim()
    .toLowerCase();
  return forwardedProtocol === "https" ? "https:" : "http:";
}

function parseRequestOrigin(request: RequestMetadata): URL | null {
  const value = request.headers.origin;
  if (!value || !request.headers.host) return null;

  try {
    const origin = new URL(value);
    if (
      !["http:", "https:"].includes(origin.protocol) ||
      origin.username ||
      origin.password ||
      origin.pathname !== "/" ||
      origin.search ||
      origin.hash
    ) {
      return null;
    }
    return origin;
  } catch {
    return null;
  }
}
