import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, mock, spyOn } from "bun:test";
import type { ReactNode } from "react";
import type { AuditEntry, AuthUser, Operation } from "@ludock/shared";
import { AUTH_REQUIRED_EVENT } from "../src/api";
import { NavigationProvider } from "../src/navigation";
import Audit from "../src/pages/Audit";
import OperationDetail from "../src/pages/OperationDetail";
import Operations from "../src/pages/Operations";
import TestProviders from "./TestProviders";
import { operationFixture, serverFixture } from "./fixtures";

const server = serverFixture();
const removedServerId = "33333333-3333-4333-8333-333333333333";
const operation = operationFixture(removedServerId, { kind: "restart", status: "succeeded" });

function auditEntry(action: string): AuditEntry {
  return {
    id: 1, username: null, action, targetType: "server", targetId: removedServerId,
    operationId: operation.id, status: "succeeded", details: null, ipAddress: null, createdAt: 0,
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((yes) => { resolve = yes; });
  return { promise, resolve };
}

function historyView(children: ReactNode, path: string, role: AuthUser["role"] = "admin") {
  window.history.replaceState({}, "", path);
  spyOn(window, "scrollTo").mockImplementation(() => {});
  return render(
    <TestProviders user={{ id: "user1", username: "friend", role }} pathname={path} navigate={mock()}>
      <NavigationProvider>{children}</NavigationProvider>
    </TestProviders>,
  );
}

function field(name: string) {
  return screen.getByLabelText(name, { exact: true }) as HTMLInputElement | HTMLSelectElement;
}

function requestUrl(input: RequestInfo | URL) {
  return new URL(String(input), window.location.origin);
}

describe("searchable history", () => {
  it("hydrates shared audit links and preserves removed server identifiers", async () => {
    const from = Date.UTC(2026, 8, 1, 12, 30, 15, 123);
    const to = Date.UTC(2026, 8, 14, 18, 45, 30);
    const filters = new URLSearchParams({
      serverId: removedServerId, actor: "Alex", action: "server.restart", status: "succeeded",
      from: String(from), to: String(to), operationId: operation.id,
    });
    const request = spyOn(globalThis, "fetch").mockImplementation(async (input) =>
      requestUrl(input).pathname.endsWith("/servers")
        ? Response.json({ servers: [server] })
        : Response.json({ entries: [auditEntry("server.restart.succeeded")] }),
    );
    historyView(<Audit />, `/audit?${filters}`);
    await screen.findByText("server.restart.succeeded");

    for (const name of ["Server", "Actor", "Action", "Status", "Operation ID"]) {
      const key = { Server: "serverId", Actor: "actor", Action: "action", Status: "status", "Operation ID": "operationId" }[name]!;
      expect(field(name).value).toBe(filters.get(key)!);
    }
    expect(new Date(field("From").value).getTime()).toBe(from);
    expect(new Date(field("To").value).getTime()).toBe(to);
    const requested = request.mock.calls.find(([input]) => requestUrl(input).pathname.endsWith("/audit"))!;
    expect(Object.fromEntries(requestUrl(requested[0]).searchParams)).toEqual({ limit: "50", ...Object.fromEntries(filters) });
    expect(screen.getByRole("link", { name: `server: ${removedServerId}` }).getAttribute("href"))
      .toBe(`/operations?serverId=${removedServerId}`);
    expect(screen.getByRole("link", { name: "Operation details" }).getAttribute("href"))
      .toBe(`/operations/${operation.id}`);
    expect(screen.getByText("Not recorded")).toBeTruthy();
  });

  it("submits local dates and keeps unsent drafts through pagination and refresh", async () => {
    const request = spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const url = requestUrl(input);
      if (url.pathname.endsWith("/servers")) return Response.json({ servers: [server] });
      return Response.json({
        operations: [operationFixture(server.id, { kind: url.searchParams.has("cursor") ? "stop" : "restart" })],
        nextCursor: url.searchParams.has("cursor") ? null : "older-page",
      });
    });
    historyView(<Operations />, "/operations");
    await screen.findByRole("link", { name: "restart" });
    const historyRequests = () => request.mock.calls.filter(([input]) => requestUrl(input).pathname.endsWith("/operations"));
    await userEvent.selectOptions(field("Server"), server.id);
    fireEvent.change(field("Actor"), { target: { value: "Alex" } });
    fireEvent.change(field("Action"), { target: { value: "restart" } });
    await userEvent.selectOptions(field("Status"), "succeeded");
    const from = "2026-09-01T09:15";
    const to = "2026-09-14T18:30";
    fireEvent.change(field("From"), { target: { value: from } });
    fireEvent.change(field("To"), { target: { value: to } });
    expect(historyRequests()).toHaveLength(1);
    await userEvent.click(screen.getByRole("button", { name: "Search", exact: true }));
    await screen.findByRole("link", { name: "restart" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Search", exact: true }));
    expect(Object.fromEntries(requestUrl(historyRequests().at(-1)![0]).searchParams)).toEqual({
      limit: "50", serverId: server.id, actor: "Alex", action: "restart", status: "succeeded",
      from: String(new Date(from).getTime()), to: String(new Date(to).getTime()),
    });
    fireEvent.change(field("Actor"), { target: { value: "Unsubmitted name" } });
    await userEvent.click(screen.getByRole("button", { name: "Older", exact: true }));
    await screen.findByRole("link", { name: "stop" });
    expect(field("Actor").value).toBe("Unsubmitted name");
    expect(requestUrl(historyRequests().at(-1)![0]).searchParams.get("actor")).toBe("Alex");
    expect(requestUrl(historyRequests().at(-1)![0]).searchParams.get("cursor")).toBe("older-page");
    await userEvent.click(screen.getByRole("button", { name: "Refresh", exact: true }));
    await screen.findByRole("link", { name: "stop" });
    expect(field("Actor").value).toBe("Unsubmitted name");
    expect(requestUrl(historyRequests().at(-1)![0]).searchParams.get("actor")).toBe("Alex");
    await userEvent.click(screen.getByRole("button", { name: "Newer", exact: true }));
    await screen.findByRole("link", { name: "restart" });
    expect(requestUrl(historyRequests().at(-1)![0]).searchParams.has("cursor")).toBe(false);
    expect(field("Actor").value).toBe("Unsubmitted name");
  });

  it("preserves unchanged URL timestamps during a repeated daylight-saving hour", async () => {
    const previousTimezone = process.env.TZ;
    process.env.TZ = "America/New_York";
    try {
      const from = Date.UTC(2024, 10, 3, 6, 30, 0, 123);
      const to = Date.UTC(2024, 10, 3, 6, 45);
      const request = spyOn(globalThis, "fetch").mockImplementation(async (input) =>
        requestUrl(input).pathname.endsWith("/servers")
          ? Response.json({ servers: [] })
          : Response.json({ operations: [] }),
      );
      const view = historyView(<Operations />, `/operations?from=${from}&to=${to}`);
      try {
        await screen.findByText("No operations match these filters.");
        expect(field("From").value).toBe("2024-11-03T01:30:00.123");
        expect(new Date(field("From").value).getTime()).toBe(from - 3_600_000);
        fireEvent.change(field("Actor"), { target: { value: "Alex" } });
        await userEvent.click(screen.getByRole("button", { name: "Search", exact: true }));
        await screen.findByText("No operations match these filters.");
        const url = requestUrl(request.mock.calls.filter(([input]) => requestUrl(input).pathname.endsWith("/operations")).at(-1)![0]);
        expect(url.searchParams.get("from")).toBe(String(from));
        expect(url.searchParams.get("to")).toBe(String(to));
      } finally {
        view.unmount();
      }
    } finally {
      if (previousTimezone === undefined) delete process.env.TZ;
      else process.env.TZ = previousTimezone;
    }
  });

  it.each(["audit", "operations"] as const)("aborts obsolete %s searches and ignores their late authentication failures", async (kind) => {
    const pending = deferred<Response>();
    const events = spyOn(window, "dispatchEvent");
    const request = spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const url = requestUrl(input);
      if (url.pathname.endsWith("/servers")) return Response.json({ servers: [] });
      if (!url.searchParams.has("actor")) return pending.promise;
      return Response.json(kind === "audit"
        ? { entries: [auditEntry("Fresh search result")] }
        : { operations: [operationFixture(removedServerId, { kind: "Fresh search result" })] });
    });
    historyView(kind === "audit" ? <Audit /> : <Operations />, `/${kind}`);
    const oldSignal = request.mock.calls.find(([input]) => requestUrl(input).pathname.endsWith(`/${kind}`))![1]?.signal;
    fireEvent.change(field("Actor"), { target: { value: "Alex" } });
    await userEvent.click(screen.getByRole("button", { name: "Search", exact: true }));
    await screen.findByText("Fresh search result");
    expect(oldSignal?.aborted).toBe(true);
    await act(async () => { pending.resolve(Response.json({ error: "Obsolete access rejection" }, { status: 401 })); });
    expect(events.mock.calls.some(([event]) => event.type === AUTH_REQUIRED_EVENT)).toBe(false);
    expect(screen.getByText("Fresh search result")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it.each(["audit", "operations"] as const)("clears the visible %s page when access is revoked", async (kind) => {
    let denied = false;
    spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      if (requestUrl(input).pathname.endsWith("/servers")) return Response.json({ servers: [] });
      if (denied) return Response.json({ error: "History access denied" }, { status: 403 });
      return Response.json(kind === "audit"
        ? { entries: [auditEntry("Previously visible action")], nextCursor: "private-page" }
        : { operations: [operationFixture(removedServerId, { kind: "Previously visible action" })], nextCursor: "private-page" });
    });
    historyView(kind === "audit" ? <Audit /> : <Operations />, `/${kind}`);
    await screen.findByText("Previously visible action");
    denied = true;
    await userEvent.click(screen.getByRole("button", { name: "Refresh", exact: true }));
    expect((await screen.findByRole("alert")).textContent).toContain("History access denied");
    expect(screen.queryByText("Previously visible action")).toBeNull();
    expect((screen.getByRole("button", { name: "Older", exact: true }) as HTMLButtonElement).disabled).toBe(true);
  });

  it.each([
    { role: "admin", actor: { id: "removed-user", name: null }, label: "User removed-user (name unavailable)" },
    { role: "viewer", actor: undefined, label: "Not recorded" },
  ] satisfies { role: AuthUser["role"]; actor: Operation["actor"]; label: string }[])(
    "shows direct operation details with actor fallback and role-appropriate links for $role",
    async ({ role, actor, label }) => {
      spyOn(globalThis, "fetch").mockImplementation(async (input) =>
        requestUrl(input).pathname.endsWith("/servers")
          ? Response.json({ servers: [] })
          : Response.json({ operation: { ...operation, actor } }),
      );
      historyView(<OperationDetail operationId={operation.id} />, `/operations/${operation.id}`, role);
      await screen.findByText(label);
      expect(screen.getByRole("link", { name: removedServerId }).getAttribute("href")).toBe(`/servers/${removedServerId}`);
      expect(screen.getByRole("link", { name: "Server operation history" }).getAttribute("href"))
        .toBe(`/operations?serverId=${removedServerId}`);
      const auditLink = screen.queryByRole("link", { name: "Related audit events" });
      if (role === "admin") expect(auditLink?.getAttribute("href")).toBe(`/audit?operationId=${operation.id}`);
      else expect(auditLink).toBeNull();
    },
  );

  it.each([401, 403, 404])("clears operation details after an HTTP %s access rejection", async (status) => {
    let denied = false;
    spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      if (requestUrl(input).pathname.endsWith("/servers")) return Response.json({ servers: [] });
      return denied
        ? Response.json({ error: "Details denied" }, { status })
        : Response.json({ operation: { ...operation, actor: { id: "private-user", name: "Private actor" } } });
    });
    historyView(<OperationDetail operationId={operation.id} />, `/operations/${operation.id}`);
    await screen.findByText("Private actor");
    denied = true;
    await userEvent.click(screen.getByRole("button", { name: "Refresh", exact: true }));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("no longer available or you no longer have access"));
    expect(screen.queryByText("Private actor")).toBeNull();
    expect(screen.queryByText(operation.id)).toBeNull();
    expect(screen.queryByRole("link", { name: "Related audit events" })).toBeNull();
    expect(screen.getByRole("link", { name: "Operation history" })).toBeTruthy();
  });
});
