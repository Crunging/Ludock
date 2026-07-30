import type { IncomingMessage } from "node:http";

type RequestMetadata = Pick<IncomingMessage, "headers" | "socket">;

export function isExternalHttpsRequest(request: RequestMetadata): boolean {
  return externalProtocols(request).includes("https:");
}

export function isSameOriginRequest(request: RequestMetadata): boolean {
  const origin = parseRequestOrigin(request);
  if (!origin) return false;

  const fetchSite = headerValue(request.headers["sec-fetch-site"])
    .trim()
    .toLowerCase();
  if (fetchSite === "same-origin") return true;
  if (fetchSite === "same-site" || fetchSite === "cross-site") return false;

  return (
    externalProtocols(request).includes(origin.protocol) &&
    externalHosts(request).includes(origin.host.toLowerCase())
  );
}

export function requestOriginDiagnostic(request: RequestMetadata): {
  originHost: string;
  originProtocol: string;
  host: string;
  forwardedHost: string;
  forwardedProtocol: string;
  fetchSite: string;
  resolvedHosts: string;
  resolvedProtocols: string;
} {
  const origin = parseRequestOrigin(request);
  return {
    originHost: origin?.host || "missing-or-invalid",
    originProtocol: origin?.protocol || "missing-or-invalid",
    host: request.headers.host || "missing",
    forwardedHost:
      headerValue(request.headers["x-forwarded-host"]) || "missing",
    forwardedProtocol:
      headerValue(request.headers["x-forwarded-proto"]) || "missing",
    fetchSite: headerValue(request.headers["sec-fetch-site"]) || "missing",
    resolvedHosts: externalHosts(request).join(",") || "none",
    resolvedProtocols: externalProtocols(request).join(",") || "none",
  };
}

function externalProtocols(request: RequestMetadata): string[] {
  if (
    (request.socket as IncomingMessage["socket"] & { encrypted?: boolean })
      .encrypted
  ) {
    return ["https:"];
  }

  const forwarded = forwardedValues(request.headers["x-forwarded-proto"])
    .map((value) => value.toLowerCase())
    .flatMap((value) => {
      if (value === "https" || value === "wss") return ["https:"];
      if (value === "http" || value === "ws") return ["http:"];
      return [];
    });
  return [...new Set(forwarded.length > 0 ? forwarded : ["http:"])];
}

function parseRequestOrigin(request: RequestMetadata): URL | null {
  const value = request.headers.origin;
  if (!value) return null;

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

function externalHosts(request: RequestMetadata): string[] {
  const protocols = externalProtocols(request);
  const forwardedHosts = forwardedValues(
    request.headers["x-forwarded-host"]
  );
  const forwardedPorts = forwardedValues(
    request.headers["x-forwarded-port"]
  ).filter(isPort);
  const rawHosts = [
    request.headers.host,
    ...forwardedHosts,
    ...forwardedHosts.flatMap((host) =>
      hostHasPort(host)
        ? []
        : forwardedPorts.map((port) => `${host}:${port}`)
    ),
  ].filter((value): value is string => Boolean(value));

  const hosts = rawHosts.flatMap((host) => [
    host.toLowerCase(),
    ...protocols.flatMap((protocol) => {
      try {
        return [new URL(`${protocol}//${host}`).host.toLowerCase()];
      } catch {
        return [];
      }
    }),
  ]);
  return [...new Set(hosts)];
}

function forwardedValues(value: string | string[] | undefined): string[] {
  return headerValue(value)
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}

function headerValue(value: string | string[] | undefined): string {
  return Array.isArray(value) ? value.join(",") : value || "";
}

function hostHasPort(host: string): boolean {
  try {
    return new URL(`http://${host}`).port !== "";
  } catch {
    return false;
  }
}

function isPort(value: string): boolean {
  const port = Number(value);
  return Number.isInteger(port) && port >= 1 && port <= 65_535;
}
