export const AUTH_REQUIRED_EVENT = "ludock:auth-required";

export async function apiFetch(
  input: RequestInfo | URL,
  init: RequestInit = {},
): Promise<Response> {
  const headers = new Headers(init.headers);

  const response = await fetch(input, {
    ...init,
    headers,
    credentials: "same-origin",
  });

  if (response.status === 401) {
    window.dispatchEvent(new Event(AUTH_REQUIRED_EVENT));
  }

  return response;
}

export function authenticatedWebSocketUrl(path: string): string {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return new URL(`${protocol}//${window.location.host}${path}`).toString();
}

export async function apiJson<T>(
  path: string,
  init: RequestInit = {},
): Promise<T> {
  const response = await apiFetch(`/api/v1${path}`, init);
  const body = await response.json().catch(() => ({}));
  if (!response.ok)
    throw new Error(body.error || `Request failed (HTTP ${response.status})`);
  return body as T;
}

export function jsonBody(method: string, value: unknown): RequestInit {
  return {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(value),
  };
}
