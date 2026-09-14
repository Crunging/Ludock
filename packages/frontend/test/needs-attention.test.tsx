import type { AttentionItem, AttentionResponse, AuthUser } from "@ludock/shared";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, mock, spyOn } from "bun:test";
import NeedsAttention from "../src/components/NeedsAttention";
import Dashboard from "../src/pages/Dashboard";
import { useServers } from "../src/hooks/useServers";
import TestProviders from "./TestProviders";
import { serverFixture } from "./fixtures";

const useServersMock = mock<typeof useServers>();
mock.module("../src/hooks/useServers", () => ({ useServers: useServersMock }));

const server = serverFixture();
const admin: AuthUser = { id: "11111111-1111-4111-8111-111111111111", username: "alex", role: "admin" };
const operationId = "22222222-2222-4222-8222-222222222222";
const scheduleId = "77777777-7777-4777-8777-777777777777";
const base = { serverId: server.id, serverName: server.displayName };
const operation: AttentionItem = {
  ...base, kind: "operation", id: `operation:${operationId}`, operationId,
  operationKind: "backup", status: "failed", updatedAt: 0,
};
const items: AttentionItem[] = [
  operation,
  { ...base, kind: "schedule", id: `schedule:${scheduleId}`, scheduleId, action: "restart", reason: "owner_disabled" },
  { ...base, kind: "binding", id: `binding:${server.id}`, bindingStatus: "review_required" },
  { ...base, kind: "availability", id: `availability:${server.id}`, state: "exited", outageStartedAt: 0 },
];
const emptyMessage = "No issues need attention in the servers and schedules you can access.";

function response(current: AttentionItem[] = [], discoveryUnavailable = false) {
  return Response.json({ items: current, discoveryUnavailable } satisfies AttentionResponse);
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function attention(role: AuthUser["role"] = "admin") {
  const navigate = mock();
  const content = (refreshKey: string) => (
    <TestProviders user={{ ...admin, role }} pathname="/" navigate={navigate}>
      <NeedsAttention refreshKey={refreshKey} />
    </TestProviders>
  );
  const view = render(content("first"));
  return { navigate, update: (key: string) => view.rerender(content(key)) };
}

function dashboard() {
  const content = (user: AuthUser) => (
    <TestProviders user={user} pathname="/" navigate={mock()}>
      <Dashboard />
    </TestProviders>
  );
  const view = render(content(admin));
  return { update: (user = admin) => view.rerender(content(user)) };
}

beforeEach(() => {
  useServersMock.mockReturnValue({
    servers: [server], loading: false, error: null,
    refresh: mock().mockResolvedValue(undefined), stale: false, lastUpdated: 1,
    connectionStatus: "connected", connectionError: null, accessDenied: false,
    canRetry: false, retry: mock(),
  });
});

describe("needs attention", () => {
  it("links each issue to its server and the exact operation or schedule to review", async () => {
    const request = spyOn(globalThis, "fetch").mockResolvedValue(response(items));
    const { navigate } = attention();
    const region = within(screen.getByRole("region", { name: "Needs attention" }));
    await region.findByText("Backup failed");
    expect(region.getAllByRole("listitem")).toHaveLength(4);
    for (const [action, path] of [
      ["Review activity", `?tab=activity&operation=${operationId}`],
      ["Review schedule", `?tab=schedules&schedule=${scheduleId}`],
      ["Review server", ""],
      ["Check availability", "?tab=availability"],
    ]) {
      const link = region.getByRole("link", { name: new RegExp(`^${action}: Friends world`) });
      expect(link.getAttribute("href")).toBe(`/servers/${server.id}${path}`);
      await userEvent.click(link);
      expect(navigate).toHaveBeenLastCalledWith(`/servers/${server.id}${path}`);
    }
    expect(region.getByText("The schedule owner’s account is disabled.")).toBeTruthy();
    expect(request.mock.calls[0][0]).toBe("/api/v1/attention");
  });

  it("limits the compact list to ten issues and expands with keyboard access", async () => {
    spyOn(globalThis, "fetch").mockResolvedValue(response(Array.from({ length: 12 }, (_, index) => ({
      ...operation, id: `operation:${index}`, serverName: `World ${index + 1}`,
    }))));
    attention();
    const expand = await screen.findByRole("button", { name: "Show all 12 items" });
    expect(screen.getAllByRole("listitem")).toHaveLength(10);
    expect(expand.getAttribute("aria-expanded")).toBe("false");
    expand.focus();
    await userEvent.keyboard("{Enter}");
    expect(screen.getAllByRole("listitem")).toHaveLength(12);
    expect(screen.getByText("World 12")).toBeTruthy();
    expect(expand.getAttribute("aria-expanded")).toBe("true");
    await userEvent.click(screen.getByRole("button", { name: "Show fewer items" }));
    expect(screen.getAllByRole("listitem")).toHaveLength(10);
  });

  it("keeps loading and failures distinct from a successful empty check and supports retry", async () => {
    const pending = deferred<Response>();
    const request = spyOn(globalThis, "fetch").mockReturnValueOnce(pending.promise);
    attention();
    expect(screen.getByText("Checking for issues…")).toBeTruthy();
    expect(screen.queryByText(emptyMessage)).toBeNull();
    await act(async () => { pending.resolve(Response.json({ error: "Attention unavailable" }, { status: 503 })); });
    expect(screen.getByRole("alert").textContent).toContain("Attention unavailable");
    expect(screen.queryByText(emptyMessage)).toBeNull();
    request.mockResolvedValueOnce(response());
    await userEvent.click(screen.getByRole("button", { name: "Retry attention check" }));
    await screen.findByText(emptyMessage);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("region", { name: "Needs attention" }).getAttribute("aria-busy")).toBe("false");
  });

  it.each(["admin", "viewer"] as const)("explains unavailable Docker state for %s without claiming health", async (role) => {
    spyOn(globalThis, "fetch").mockResolvedValue(response([], true));
    attention(role);
    await screen.findByText("No attention items in the saved state you can access.");
    expect(screen.getByText(/Docker is unavailable\. These items use saved state/)).toBeTruthy();
    expect(screen.queryByText(emptyMessage)).toBeNull();
    const diagnostics = screen.queryByRole("link", { name: "Check diagnostics" });
    if (role === "admin") expect(diagnostics?.getAttribute("href")).toBe("/diagnostics");
    else expect(diagnostics).toBeNull();
  });

  it.each([
    ["admin", 4], ["operator", 3], ["viewer", 2],
  ] as const)("enforces the %s role ceiling even if a response includes extra issues", async (role, count) => {
    spyOn(globalThis, "fetch").mockResolvedValue(response(items));
    attention(role);
    await screen.findByText("Backup failed");
    expect(screen.getAllByRole("listitem")).toHaveLength(count);
    expect(Boolean(screen.queryByRole("link", { name: /^Review server:/ }))).toBe(role === "admin");
    expect(Boolean(screen.queryByRole("link", { name: /^Review schedule:/ }))).toBe(role !== "viewer");
  });

  it("keeps issue controls focused during refresh and hides cached issues after failure", async () => {
    const pending = deferred<Response>();
    spyOn(globalThis, "fetch").mockResolvedValueOnce(response(items)).mockReturnValueOnce(pending.promise);
    const view = attention();
    await screen.findByText("Backup failed");
    const link = screen.getByRole("link", { name: /^Review activity:/ });
    link.focus();
    view.update("refresh");
    expect(screen.getByText("Backup failed")).toBeTruthy();
    expect(document.activeElement).toBe(link);
    expect(screen.getByText("Checking…")).toBeTruthy();
    expect(screen.queryByText(emptyMessage)).toBeNull();
    await act(async () => { pending.reject(new Error("Connection lost")); });
    expect(screen.getByRole("alert").textContent).toContain("Connection lost");
    expect(screen.queryByRole("listitem")).toBeNull();
    expect(screen.queryByText(emptyMessage)).toBeNull();
  });

  it("refreshes outcomes in the background without losing focus on an unchanged issue", async () => {
    const pending = deferred<Response>();
    let poll: () => void = () => {};
    spyOn(window, "setInterval").mockImplementation((callback, delay) => {
      if (delay === 30_000 && typeof callback === "function") poll = callback as () => void;
      return 1;
    });
    const request = spyOn(globalThis, "fetch").mockResolvedValueOnce(response(items)).mockReturnValueOnce(pending.promise);
    attention();
    await screen.findByText("Backup failed");
    const link = screen.getByRole("link", { name: /^Review activity:/ });
    link.focus();
    act(() => { poll(); });
    expect(request).toHaveBeenCalledTimes(2);
    expect(document.activeElement).toBe(link);
    await act(async () => { pending.resolve(response(items)); });
    expect(document.activeElement).toBe(link);
    expect(screen.getByRole("region", { name: "Needs attention" }).getAttribute("aria-busy")).toBe("false");
  });

  it.each(["success", "failure"] as const)("ignores a late %s from a superseded refresh", async (outcome) => {
    const pending = deferred<Response>();
    const request = spyOn(globalThis, "fetch").mockReturnValueOnce(pending.promise).mockResolvedValueOnce(response());
    const view = attention();
    view.update("newer");
    await screen.findByText(emptyMessage);
    expect(request.mock.calls[0][1]?.signal?.aborted).toBe(true);
    await act(async () => {
      if (outcome === "success") pending.resolve(response(items));
      else pending.reject(new Error("Obsolete error"));
    });
    expect(screen.getByText(emptyMessage)).toBeTruthy();
    expect(screen.queryByRole("listitem")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("dashboard attention access and refresh", () => {
  it("refreshes attention with the dashboard while server filters leave issues reachable", async () => {
    const request = spyOn(globalThis, "fetch").mockResolvedValueOnce(response(items)).mockResolvedValueOnce(response());
    dashboard();
    await screen.findByText("Backup failed");
    await userEvent.type(screen.getByRole("searchbox", { name: "Find a server" }), "nothing matches");
    expect(screen.queryByRole("article")).toBeNull();
    expect(screen.getByText("Backup failed")).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Refresh", exact: true }));
    await screen.findByText(emptyMessage);
    expect(useServersMock().refresh).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledTimes(2);
  });

  it.each(["cached", "pending"] as const)("removes %s issues when the signed-in account changes", async (state) => {
    const pending = deferred<Response>();
    const request = spyOn(globalThis, "fetch");
    if (state === "cached") request.mockResolvedValueOnce(response(items));
    else request.mockReturnValueOnce(pending.promise);
    request.mockResolvedValueOnce(response());
    const view = dashboard();
    if (state === "cached") await screen.findByText("Backup failed");
    view.update({ id: "33333333-3333-4333-8333-333333333333", username: "friend", role: "viewer" });
    expect(screen.queryByText("Backup failed")).toBeNull();
    await screen.findByText(emptyMessage);
    expect(request.mock.calls[0][1]?.signal?.aborted).toBe(true);
    await act(async () => { pending.resolve(response(items)); });
    expect(screen.queryByRole("listitem")).toBeNull();
    expect(screen.getByText(emptyMessage)).toBeTruthy();
  });

  it.each(["cached", "pending"] as const)("removes %s issues when the server stream denies access", async (state) => {
    const pending = deferred<Response>();
    const request = spyOn(globalThis, "fetch");
    if (state === "cached") request.mockResolvedValueOnce(response(items));
    else request.mockReturnValueOnce(pending.promise);
    const view = dashboard();
    if (state === "cached") await screen.findByText("Backup failed");
    useServersMock.mockReturnValue({ ...useServersMock(), servers: [], accessDenied: true, stale: true });
    view.update();
    expect(screen.queryByRole("region", { name: "Needs attention" })).toBeNull();
    expect(request.mock.calls[0][1]?.signal?.aborted).toBe(true);
    await act(async () => { pending.resolve(response(items)); });
    expect(screen.queryByText("Backup failed")).toBeNull();
    expect(screen.queryByText(emptyMessage)).toBeNull();
    expect(request).toHaveBeenCalledTimes(1);
  });
});
