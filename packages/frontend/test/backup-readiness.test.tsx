import { describe, expect, it, mock, spyOn } from "bun:test";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type BackupPreflight, type Server } from "@ludock/shared";
import { apiJson } from "../src/api";
import ServerDetail from "../src/pages/ServerDetail";
import TestProviders from "./TestProviders";
import { operationFixture, serverDetailResponse, serverFixture } from "./fixtures";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({ ...originalApi, apiJson: apiJsonMock }));
const ready: BackupPreflight = { ready: true, checkedAt: 1, issues: [] };
const blocked: BackupPreflight = {
  ready: false, checkedAt: 2,
  issues: [{ code: "BACKUP_DESTINATION", message: "Mount a separate backup destination." }],
};

function detail(onRequest?: (path: string, init?: RequestInit) => unknown | Promise<unknown>, overrides: Partial<Server> = {}) {
  const server = serverFixture({
    permissions: ["server.view", "backups.create"],
    latestBackup: { createdAt: Date.UTC(2026, 8, 13, 12), size: 1024 },
    ...overrides,
  });
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
    const response = await onRequest?.(path, init);
    if (response !== undefined) return response;
    if (init?.method === "POST") return { operation: operationFixture(server.id, { kind: "backup", status: "queued" }) };
    return serverDetailResponse(path, server);
  });
  const view = render(<TestProviders user={{ id: "friend", role: "operator", username: "friend" }} pathname={`/servers/${server.id}`} navigate={mock()}>
    <ServerDetail serverId={server.id} />
  </TestProviders>);
  return { ...view, server };
}

describe("backup readiness", () => {
  it("explains failed checks before confirmation and recovers after checking again", async () => {
    let response = blocked;
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    detail((path) => path.endsWith("/preflight") ? { preflight: response } : undefined);
    await userEvent.click(await screen.findByRole("tab", { name: "Backups" }));
    await screen.findByText("Mount a separate backup destination.");
    const create = screen.getByRole("button", { name: "Create backup" }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    expect(screen.getByText(/Latest successful backup:/).closest("p")?.textContent).toContain("1 KiB");
    expect(screen.getByText(/Ask an administrator to resolve/)).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Download" })).toBeNull();
    expect(confirm).not.toHaveBeenCalled();
    response = ready;
    await userEvent.click(screen.getByRole("button", { name: "Check again" }));
    await screen.findByText("Preflight checks passed.");
    expect(create.disabled).toBe(false);
    await userEvent.click(create);
    await waitFor(() => expect(screen.getByRole("tab", { name: "Activity", selected: true })).toBeTruthy());
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(apiJsonMock.mock.calls.filter(([path]) => path.endsWith("/preflight"))).toHaveLength(3);
    expect(apiJsonMock.mock.calls.filter(([, , init]) => init?.method === "POST")).toHaveLength(1);
    confirm.mockRestore();
  });

  it("rechecks on Create and blocks a newly unavailable destination without confirming downtime", async () => {
    let checks = 0;
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    detail((path) => path.endsWith("/preflight") ? { preflight: ++checks === 1 ? ready : blocked } : undefined);
    await userEvent.click(await screen.findByRole("tab", { name: "Backups" }));
    await screen.findByText("Preflight checks passed.");
    await userEvent.click(screen.getByRole("button", { name: "Create backup" }));
    await screen.findByText("Mount a separate backup destination.");
    expect(confirm).not.toHaveBeenCalled();
    expect(apiJsonMock.mock.calls.some(([, , init]) => init?.method === "POST")).toBe(false);
    confirm.mockRestore();
  });

  it("keeps an execution-time rejection visible after a passed check", async () => {
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    detail((_path, init) => { if (init?.method === "POST") throw new Error("The server binding changed. Check its identity."); });
    await userEvent.click(await screen.findByRole("tab", { name: "Backups" }));
    await screen.findByText("Preflight checks passed.");
    await userEvent.click(screen.getByRole("button", { name: "Create backup" }));
    await screen.findByText("The server binding changed. Check its identity.");
    expect(screen.getByRole("tab", { name: "Backups", selected: true })).toBeTruthy();
    expect(screen.queryByText("Backup queued. Follow its progress in Activity.")).toBeNull();
    confirm.mockRestore();
  });

  it("ignores a late passed check from an earlier tab visit", async () => {
    let resolveOld!: (response: unknown) => void;
    const old = new Promise((resolve) => { resolveOld = resolve; });
    let checks = 0;
    detail((path) => path.endsWith("/preflight") ? (++checks === 1 ? old : { preflight: blocked }) : undefined);
    await userEvent.click(await screen.findByRole("tab", { name: "Backups" }));
    await screen.findByText("Checking backup destination and data roots…");
    await userEvent.click(screen.getByRole("tab", { name: "Activity" }));
    await userEvent.click(screen.getByRole("tab", { name: "Backups" }));
    await screen.findByText("Mount a separate backup destination.");
    await act(async () => resolveOld({ preflight: ready }));
    expect(screen.queryByText("Preflight checks passed.")).toBeNull();
    expect((screen.getByRole("button", { name: "Create backup" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("hides prior readiness after a failed read and lets the user retry", async () => {
    let fail = false;
    detail((path) => {
      if (path.endsWith("/preflight") && fail) throw new Error("Readiness is unavailable. Try again.");
    });
    await userEvent.click(await screen.findByRole("tab", { name: "Backups" }));
    await screen.findByText("Preflight checks passed.");
    fail = true;
    await userEvent.click(screen.getByRole("button", { name: "Check again" }));
    await screen.findByRole("alert");
    expect(screen.queryByText("Preflight checks passed.")).toBeNull();
    expect((screen.getByRole("button", { name: "Create backup" }) as HTMLButtonElement).disabled).toBe(true);
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Check again" }));
    await screen.findByText("Preflight checks passed.");
  });

  it("does not fetch readiness for a lifecycle-only grant", async () => {
    detail(undefined, { permissions: ["server.view", "server.start"] });
    await screen.findByRole("heading", { name: "Friends world" });
    expect(screen.queryByRole("tab", { name: "Backups" })).toBeNull();
    expect(apiJsonMock.mock.calls.some(([path]) => path.endsWith("/preflight"))).toBe(false);
  });
});
