import type { IncomingMessage } from "node:http";
import proxyaddr from "proxy-addr";

type RequestMetadata = Pick<IncomingMessage, "headers" | "socket">;

let compiledFrom: string | null = null;
let isTrustedPeer: ((address: string, hop: number) => boolean) | null = null;

/**
 * Returns whether `X-Forwarded-Proto` may be believed for this connection.
 *
 * When TRUSTED_PROXIES is configured we only honour the header if the immediate
 * peer is one of those proxies. When it is not configured we stay permissive:
 * an operator terminating TLS at a proxy without declaring it should not
 * silently lose the `Secure` cookie flag, and a spoofed header only affects the
 * spoofing client's own request, never a victim's.
 */
function forwardedHeadersAreTrusted(request: RequestMetadata): boolean {
  const configured = process.env.TRUSTED_PROXIES?.trim() || "";
  if (!configured) return true;

  if (compiledFrom !== configured) {
    compiledFrom = configured;
    const list = configured
      .split(",")
      .map((value) => value.trim())
      .filter(Boolean);
    try {
      isTrustedPeer = proxyaddr.compile(list);
    } catch {
      isTrustedPeer = null;
    }
  }

  const peer = request.socket?.remoteAddress;
  if (!isTrustedPeer || !peer) return false;
  return isTrustedPeer(peer, 0);
}

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

  if (!forwardedHeadersAreTrusted(request)) return "http:";

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
