import { describe, expect, it, mock } from "bun:test";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SERVER_CAPABILITIES, type Operation } from "@ludock/shared";
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
  let operations: Operation[] = [];
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
    if (path.endsWith("/update-capability")) return { capability: {
      available: compose, updatesOnStart, manager: compose ? "compose" : "external", actionLabel: "Update server",
      ...(compose ? { projectName: "games", serviceName: "world", image: server.image } : { unavailableReason: "No Compose source" }),
    } };
    if (path.endsWith("/restart-update") && init?.method === "POST") {
      operations = [operationFixture(server.id, { kind: "restart_to_update", status: "running", phase: "backing_up" })];
      return { operation: operations[0] };
    }
    return serverDetailResponse(path, server, { operations });
  });
  const user = { id: "operator", username: "friend", role: operator ? "operator" as const : "admin" as const };
  const content = (visible: boolean) => <TestProviders user={user} pathname={`/servers/${server.id}`} navigate={mock()}>
    {visible ? <ServerDetail serverId={server.id} /> : <p>Away</p>}
  </TestProviders>;
  const view = render(content(true));
  return { server, reload: () => { view.rerender(content(false)); view.rerender(content(true)); } };
}

describe("restart updates on the Update tab", () => {
  for (const compose of [false, true])
    it(`offers a tracked restart with backup ${compose ? "alongside Compose updates" : "without Compose access"}`, async () => {
      const { reload } = detail({ compose });
      await userEvent.click(await screen.findByRole("tab", { name: "Update", exact: true }));
      const button = await screen.findByRole("button", { name: "Restart to update" });
      const form = within(button.closest("form")!);
      expect((button as HTMLButtonElement).disabled).toBe(true);
      expect((form.getByRole("checkbox", { name: "Take a backup first" }) as HTMLInputElement).checked).toBe(true);
      expect(screen.getByText("This image downloads the latest game version when it starts.")).toBeTruthy();
      expect(Boolean(screen.queryByRole("button", { name: "Update server", exact: true }))).toBe(compose);
      if (!compose) expect(screen.getByText("No Compose source")).toBeTruthy();
      await userEvent.click(form.getByRole("checkbox", { name: /I understand/ }));
      await userEvent.click(button);
      await screen.findByRole("tabpanel", { name: "Activity" });
      expect(apiJsonMock.mock.calls.find(([path, , init]) => path.endsWith("/restart-update") && init?.method === "POST")?.[2]?.body)
        .toBe(JSON.stringify({ createBackup: true }));
      expect(apiJsonMock.mock.calls.some(([path, , init]) => path.endsWith("/updates") && init?.method === "POST")).toBe(false);
      reload();
      await screen.findByRole("tabpanel", { name: "Activity" });
      await screen.findByRole("link", { name: "restart to update" });
      await userEvent.click(screen.getByRole("tab", { name: "Update", exact: true }));
      expect((screen.getByRole("button", { name: "Restart to update" }) as HTMLButtonElement).disabled).toBe(true);
    });
  it("lets restart-only operators skip backup with the same explicit name confirmation", async () => {
    const { server } = detail({ operator: true, backup: false });
    await userEvent.click(await screen.findByRole("tab", { name: "Update", exact: true }));
    expect(screen.queryByRole("checkbox", { name: "Take a backup first" })).toBeNull();
    expect(screen.queryByText("Update container image")).toBeNull();
    const button = await screen.findByRole("button", { name: "Restart to update" });
    await userEvent.click(screen.getByRole("checkbox", { name: /I understand/ }));
    expect((button as HTMLButtonElement).disabled).toBe(true);
    await userEvent.type(screen.getByRole("textbox", { name: /to skip the backup/ }), server.displayName);
    await userEvent.click(button);
    await waitFor(() => expect(apiJsonMock.mock.calls.some(([path, , init]) =>
      path.endsWith("/restart-update") && init?.body === JSON.stringify({ createBackup: false, skipBackupConfirmation: server.displayName }),
    )).toBe(true));
  });
  it("keeps Compose updates for images without a confirmed startup default", async () => {
    detail({ compose: true, updatesOnStart: false });
    await userEvent.click(await screen.findByRole("tab", { name: "Update", exact: true }));
    await screen.findByRole("button", { name: "Update server", exact: true });
    expect(screen.queryByRole("button", { name: "Restart to update" })).toBeNull();
  });
});
