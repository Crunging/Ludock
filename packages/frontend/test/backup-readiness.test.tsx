import { describe, expect, it, mock, spyOn } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type BackupPreflight } from "@ludock/shared";
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

function detail(onRequest: (path: string, init?: RequestInit) => unknown | Promise<unknown>) {
  const server = serverFixture({
    permissions: ["server.view", "backups.create"],
  });
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
    const response = await onRequest?.(path, init);
    if (response !== undefined) return response;
    if (init?.method === "POST") return { operation: operationFixture(server.id, { kind: "backup", status: "queued" }) };
    return serverDetailResponse(path, server);
  });
  render(<TestProviders user={{ id: "friend", role: "operator", username: "friend" }} pathname={`/servers/${server.id}`} navigate={mock()}>
    <ServerDetail serverId={server.id} />
  </TestProviders>);
}

describe("backup readiness", () => {
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
});
