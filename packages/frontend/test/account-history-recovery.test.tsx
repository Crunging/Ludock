import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, spyOn } from "bun:test";
import Account from "../src/pages/Account";
import Audit from "../src/pages/Audit";
import ApplicationLogs from "../src/pages/ApplicationLogs";
import Diagnostics from "../src/pages/Diagnostics";
import { AUTH_REQUIRED_EVENT } from "../src/api";
import { NavigationProvider } from "../src/navigation";

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

const generation = "31a2fb01-b8d7-4bc4-b741-cfefc4e72cc1";
function logs(message: string, id = 1) {
  return Response.json({
    generation,
    entries: [{ id, timestamp: 0, level: "info", component: "app", message }],
  });
}

describe("account and history recovery", () => {
  it("preserves password success and newer edits when the following sessions read fails", async () => {
    const saved = deferred<Response>();
    let sessionReads = 0;
    const request = spyOn(globalThis, "fetch").mockImplementation(async (_url, init) => {
      if (init?.method === "POST") return saved.promise;
      return ++sessionReads === 1
        ? Response.json({ sessions: [] })
        : Response.json({ error: "Sessions unavailable" }, { status: 503 });
    });
    render(<Account />);
    await screen.findByText("No active sessions.");
    const current = screen.getByLabelText("Current password") as HTMLInputElement;
    const next = screen.getByLabelText("New password") as HTMLInputElement;
    const confirmation = screen.getByLabelText("Confirm new password") as HTMLInputElement;
    fireEvent.change(current, { target: { value: "submitted-current" } });
    for (const field of [next, confirmation])
      fireEvent.change(field, { target: { value: "submitted-password-123" } });
    await userEvent.click(screen.getByRole("button", { name: "Change password" }));
    fireEvent.change(current, { target: { value: "newer-current-draft" } });
    for (const field of [next, confirmation])
      fireEvent.change(field, { target: { value: "newer-password-draft" } });
    await act(async () => { saved.resolve(Response.json({ ok: true })); });

    expect((await screen.findByRole("alert")).textContent).toContain("Unable to load sessions: Sessions unavailable");
    expect(screen.getByRole("status").textContent).toContain("Password changed. Other sessions have been signed out.");
    expect(current.value).toBe("newer-current-draft");
    expect(next.value).toBe("newer-password-draft");
    expect(confirmation.value).toBe("newer-password-draft");
    expect((screen.getByRole("button", { name: "Change password" }) as HTMLButtonElement).disabled).toBe(false);
    const writes = request.mock.calls.filter(([, init]) => init?.method === "POST");
    expect(writes).toHaveLength(1);
    expect(JSON.parse(String(writes[0][1]?.body))).toEqual({
      currentPassword: "submitted-current", newPassword: "submitted-password-123",
    });
  });

  it("shows audit loading and failure without claiming there is no activity", async () => {
    const pending = deferred<Response>();
    let failed = true;
    spyOn(globalThis, "fetch").mockImplementation(async (url) => String(url).endsWith("/servers")
      ? Response.json({ servers: [] })
      : failed ? pending.promise : Response.json({ entries: [] }));
    render(<NavigationProvider><Audit /></NavigationProvider>);
    expect(screen.getByRole("status").textContent).toContain("Loading audit log");
    expect(screen.queryByText("No audit activity yet")).toBeNull();
    await act(async () => { pending.reject(new Error("Audit unavailable")); });
    expect((await screen.findByRole("alert")).textContent).toContain("Audit unavailable");
    expect(screen.queryByText("No audit activity yet")).toBeNull();
    failed = false;
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByText("No audit activity yet");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("hides stale diagnostics and its no-issues message after a failed refresh", async () => {
    let unavailable = false;
    spyOn(globalThis, "fetch").mockImplementation(async (url) => {
      if (String(url).endsWith("/integrations")) return Response.json({ integrations: [] });
      return unavailable
        ? Response.json({ error: "Diagnostics unavailable" }, { status: 503 })
        : Response.json({ diagnostics: [], dockerConnected: true, composeAvailable: true });
    });
    render(<NavigationProvider><Diagnostics /></NavigationProvider>);
    await screen.findByText("No discovery issues reported.");
    expect(screen.getByText("Connected")).toBeTruthy();
    unavailable = true;
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect((await screen.findByRole("alert")).textContent).toContain("Diagnostics unavailable");
    expect(screen.queryByText("No discovery issues reported.")).toBeNull();
    expect(screen.queryByText("Connected")).toBeNull();
    expect(screen.queryByText("Unavailable")).toBeNull();
  });

  it("shows diagnostics only after both system and integration reads succeed", async () => {
    const integrations = deferred<Response>();
    const request = spyOn(globalThis, "fetch").mockImplementation(async (url) =>
      String(url).endsWith("/integrations")
        ? integrations.promise
        : Response.json({ diagnostics: [], dockerConnected: true, composeAvailable: true }),
    );
    render(<NavigationProvider><Diagnostics /></NavigationProvider>);
    await act(async () => {});
    expect(screen.getByRole("status").textContent).toContain("Loading diagnostics");
    expect(screen.queryByText("Connected")).toBeNull();
    expect(screen.queryByText("No discovery issues reported.")).toBeNull();

    await act(async () => { integrations.reject(new Error("Integrations unavailable")); });
    expect((await screen.findByRole("alert")).textContent).toContain("Integrations unavailable");
    expect(screen.queryByText("Connected")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Game capabilities" })).toBeNull();

    request.mockImplementation(async (url) =>
      String(url).endsWith("/integrations")
        ? Response.json({ integrations: [] })
        : Response.json({ diagnostics: [], dockerConnected: true, composeAvailable: true }),
    );
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByText("Connected");
    expect(screen.getByRole("heading", { name: "Game capabilities" })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("cancels remaining diagnostics reads after failure and ignores their late authorization response", async () => {
    const system = deferred<Response>();
    const events = spyOn(window, "dispatchEvent");
    const request = spyOn(globalThis, "fetch").mockImplementation(async (url) =>
      String(url).endsWith("/integrations")
        ? Response.json({ error: "Integrations unavailable" }, { status: 503 })
        : system.promise,
    );
    render(<NavigationProvider><Diagnostics /></NavigationProvider>);
    expect((await screen.findByRole("alert")).textContent).toContain("Integrations unavailable");
    const systemSignal = request.mock.calls.find(([url]) => String(url).endsWith("/diagnostics"))?.[1]?.signal;
    expect(systemSignal?.aborted).toBe(true);
    await act(async () => {
      system.resolve(Response.json({ error: "Obsolete authentication rejection" }, { status: 401 }));
    });
    expect(events.mock.calls.some(([event]) => event.type === AUTH_REQUIRED_EVENT)).toBe(false);
    expect(screen.getByRole("alert").textContent).toContain("Integrations unavailable");
    expect(screen.queryByRole("heading", { name: "Game capabilities" })).toBeNull();
  });

  it("offers connection recovery without claiming discovery succeeded while Docker is unavailable", async () => {
    let connected = false;
    spyOn(globalThis, "fetch").mockImplementation(async (url) =>
      String(url).endsWith("/integrations")
        ? Response.json({ integrations: [] })
        : Response.json({ diagnostics: [], dockerConnected: connected, composeAvailable: false }),
    );
    render(<NavigationProvider><Diagnostics /></NavigationProvider>);
    await screen.findByRole("heading", { name: "Connect Ludock to Docker" });
    expect(screen.getByText("Discovery cannot be checked until Docker is connected.")).toBeTruthy();
    expect(screen.queryByText("No discovery issues reported.")).toBeNull();
    expect(screen.getByRole("link", { name: "View Ludock logs" }).getAttribute("href")).toBe("/logs");
    expect(screen.getByRole("link", { name: "Open update settings" }).getAttribute("href")).toBe("/settings");

    connected = true;
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByText("No discovery issues reported.");
    expect(screen.queryByRole("heading", { name: "Connect Ludock to Docker" })).toBeNull();
    expect(screen.getByRole("heading", { name: "Enable Compose updates when you need them" })).toBeTruthy();
  });

  it("aborts a paused log read and ignores its late response after resuming", async () => {
    const older = deferred<Response>();
    const request = spyOn(globalThis, "fetch")
      .mockReturnValueOnce(older.promise)
      .mockResolvedValueOnce(logs("Resumed data", 12));
    render(<ApplicationLogs />);
    const signal = request.mock.calls[0][1]?.signal;
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    expect(signal?.aborted).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Resume" }));
    await screen.findByText("Resumed data");
    await act(async () => { older.resolve(logs("Stale paused data", 1)); });
    expect(screen.getByText("Resumed data")).toBeTruthy();
    expect(screen.queryByText("Stale paused data")).toBeNull();
    expect(request).toHaveBeenCalledTimes(2);
  });

  it.each([401, 403, 200])("clears cached logs after access or schema rejection (HTTP %s)", async (status) => {
    const denied = status === 200
      ? Response.json({ entries: [], secret: "invalid-response-private-value" })
      : Response.json({ error: "Log access denied" }, { status });
    const request = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(logs("Previously visible log", 12))
      .mockResolvedValueOnce(denied)
      .mockResolvedValueOnce(logs("Recovered log", 20));
    render(<ApplicationLogs />);
    await screen.findByText("Previously visible log");
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    const error = await screen.findByRole("alert");
    expect(error.textContent).toContain(status === 200 ? "invalid response" : "Log access denied");
    expect(error.textContent).not.toContain("invalid-response-private-value");
    expect(screen.queryByText("Previously visible log")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByText("Recovered log");
    expect(request.mock.calls[2][0]).toBe("/api/v1/application-logs?limit=250&after=0");
  });

  it("aborts a pending manual refresh when unmounted while logs are paused", async () => {
    const pending = deferred<Response>();
    const request = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(logs("Initial log"))
      .mockReturnValueOnce(pending.promise);
    const view = render(<ApplicationLogs />);
    await screen.findByText("Initial log");
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    const signal = request.mock.calls[1][1]?.signal;
    expect(signal?.aborted).toBe(false);
    view.unmount();
    expect(signal?.aborted).toBe(true);
    await act(async () => { pending.resolve(logs("Late manual refresh", 2)); });
    expect(request).toHaveBeenCalledTimes(2);
  });
});
