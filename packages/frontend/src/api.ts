export const AUTH_REQUIRED_EVENT = "game-panel:auth-required";

export async function apiFetch(
  input: RequestInfo | URL,
  init: RequestInit = {}
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
