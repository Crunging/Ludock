import { describe, expect, it, mock } from "bun:test";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SERVER_CAPABILITIES } from "@ludock/shared";
import TestProviders from "./TestProviders";
import { operationFixture, serverDetailResponse, serverFixture } from "./fixtures";
import ServerDetail from "../src/pages/ServerDetail";
import { apiJson } from "../src/api";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({ ...originalApi, apiJson: apiJsonMock }));

function detail({ compose = false, operator = false, backup = true, updatesOnStart = true } = {}) {
  const server = serverFixture({ permissions: operator
    ? ["server.view", "server.restart", ...(backup ? ["backups.create" as const] : [])]
    : [...SERVER_CAPABILITIES],
  });
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
    if (path.endsWith("/update-capability")) return { capability: {
      available: compose, updatesOnStart, manager: compose ? "compose" : "external", actionLabel: "Update server",
      ...(compose ? { projectName: "games", serviceName: "world", image: server.image } : { unavailableReason: "No Compose source" }),
    } };
    if (path.endsWith("/restart") && init?.method === "POST")
      return { operation: operationFixture(server.id, { kind: "restart", status: "running", phase: "backing_up" }) };
    return serverDetailResponse(path, server, { operations: [] });
  });
  const user = { id: "operator", username: "friend", role: operator ? "operator" as const : "admin" as const };
  render(<TestProviders user={user} pathname={`/servers/${server.id}`} navigate={mock()}>
    <ServerDetail serverId={server.id} />
  </TestProviders>);
  return server;
}
const restartBody = () => apiJsonMock.mock.calls
  .find(([path, , init]) => path.endsWith("/restart") && init?.method === "POST")?.[2]?.body;

describe("restart with a backup", () => {
  for (const backup of [false, true])
    it(`sends the dialog's backup choice (${backup ? "backup" : "no backup"})`, async () => {
      apiJsonMock.mockClear();
      detail();
      await userEvent.click(await screen.findByRole("button", { name: "Restart", exact: true }));
      const option = screen.getByRole("checkbox", { name: "Take a backup first" }) as HTMLInputElement;
      expect(option.checked).toBe(false);
      if (backup) await userEvent.click(option);
      await userEvent.click(screen.getByRole("button", { name: "Restart server" }));
      await waitFor(() => expect(apiJsonMock.mock.calls.some(([path]) => path.endsWith("/restart"))).toBe(true));
      expect(restartBody()).toBe(backup ? JSON.stringify({ createBackup: true }) : undefined);
    });
  it("doesn't offer a backup without backup access", async () => {
    apiJsonMock.mockClear();
    detail({ operator: true, backup: false });
    await userEvent.click(await screen.findByRole("button", { name: "Restart", exact: true }));
    expect(screen.queryByRole("checkbox", { name: "Take a backup first" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Restart server" }));
    await waitFor(() => expect(apiJsonMock.mock.calls.some(([path]) => path.endsWith("/restart"))).toBe(true));
    expect(restartBody()).toBeUndefined();
  });
});

describe("startup updates on the Update tab", () => {
  for (const compose of [false, true])
    it(`explains that Restart updates the game ${compose ? "alongside Compose updates" : "without Compose access"}`, async () => {
      detail({ compose });
      await userEvent.click(await screen.findByRole("tab", { name: "Update", exact: true }));
      expect(await screen.findByRole("heading", { name: "Game updates" })).toBeTruthy();
      expect(screen.getByText(/so Restart updates the game\.$/)).toBeTruthy();
      expect(screen.getByText(/To back up first, choose Take a backup first when you restart\./)).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Restart to update" })).toBeNull();
      expect(Boolean(screen.queryByRole("button", { name: "Update server", exact: true }))).toBe(compose);
      if (!compose) expect(screen.getByText("No Compose source")).toBeTruthy();
    });
  it("tells restart-only operators how to get a newer image", async () => {
    detail({ operator: true, backup: false });
    await userEvent.click(await screen.findByRole("tab", { name: "Update", exact: true }));
    await screen.findByRole("heading", { name: "Game updates" });
    expect(screen.queryByText(/To back up first/)).toBeNull();
    expect(screen.queryByText("Update container image")).toBeNull();
    expect(screen.getByText("Pull a newer image through the tool that created this container.")).toBeTruthy();
  });
  it("keeps Compose updates for images without a confirmed startup default", async () => {
    detail({ compose: true, updatesOnStart: false });
    await userEvent.click(await screen.findByRole("tab", { name: "Update", exact: true }));
    await screen.findByRole("button", { name: "Update server", exact: true });
    expect(screen.queryByRole("heading", { name: "Game updates" })).toBeNull();
  });
});
