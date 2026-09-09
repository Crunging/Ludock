import { apiErrorSchema, type ResponseSchema } from "@ludock/shared";

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

  if (response.status === 401 && !init.signal?.aborted) {
    window.dispatchEvent(new Event(AUTH_REQUIRED_EVENT));
  }

  return response;
}

export function authenticatedWebSocketUrl(path: string): string {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return new URL(`${protocol}//${window.location.host}${path}`).toString();
}

export class ApiRequestError extends Error {
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = "ApiRequestError";
    this.status = status;
  }
}

/** Validate success and error bodies before any caller consumes them. */
export async function apiResponse<T>(
  response: Response,
  schema: ResponseSchema<T>,
): Promise<T> {
  const body: unknown = await response.json().catch(() => undefined);
  if (!response.ok) {
    const error = apiErrorSchema.safeParse(body);
    throw new ApiRequestError(
      error.success
        ? error.data.error
        : `Request failed (HTTP ${response.status})`,
      response.status,
    );
  }
  try {
    return schema.parse(body);
  } catch {
    // Schema errors can include response values. Keep unexpected data out of UI
    // errors and logs, especially on authentication and settings endpoints.
    throw new ApiRequestError(
      "The server returned an invalid response. Refresh and try again.",
      response.status,
    );
  }
}

export async function apiJson<T>(
  path: string,
  schema: ResponseSchema<T>,
  init: RequestInit = {},
): Promise<T> {
  return apiResponse(await apiFetch(`/api/v1${path}`, init), schema);
}

export function jsonBody(method: string, value: unknown): RequestInit {
  return {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(value),
  };
}
