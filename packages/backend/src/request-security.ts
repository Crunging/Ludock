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
    origin.host.toLowerCase() === externalHost(request)?.toLowerCase()
  );
}

function externalProtocol(request: RequestMetadata): "http:" | "https:" {
  if (
    (request.socket as IncomingMessage["socket"] & { encrypted?: boolean })
      .encrypted
  ) {
    return "https:";
  }

  const forwardedProtocol = firstForwardedValue(
    request.headers["x-forwarded-proto"]
  ).toLowerCase();
  return forwardedProtocol === "https" ? "https:" : "http:";
}

function parseRequestOrigin(request: RequestMetadata): URL | null {
  const value = request.headers.origin;
  if (!value || !externalHost(request)) return null;

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

function externalHost(request: RequestMetadata): string | undefined {
  return (
    firstForwardedValue(request.headers["x-forwarded-host"]) ||
    request.headers.host
  );
}

function firstForwardedValue(
  value: string | string[] | undefined
): string {
  return String(Array.isArray(value) ? value[0] || "" : value || "")
    .split(",")[0]
    .trim();
}
