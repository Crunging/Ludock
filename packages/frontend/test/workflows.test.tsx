import { describe, expect, it, mock } from "bun:test";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  SERVER_CAPABILITIES,
  type Backup,
} from "@ludock/shared";
import TestProviders from "./TestProviders";
import { operationFixture, serverDetailResponse, serverFixture, type ServerDetailData } from "./fixtures";
import ServerDetail from "../src/pages/ServerDetail";
import { apiJson } from "../src/api";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({
  ...originalApi,
  apiJson: apiJsonMock,
}));
const server = serverFixture({
  permissions: ["server.view", "server.start", "server.stop"],
});
const completedUpdate = operationFixture(server.id);
const backup: Backup = {
  id: "60c15d4e-b46a-4e57-b52d-fc08ee134b75",
  serverId: server.id,
  createdAt: 0,
  roots: [{ id: "root-0", path: "/data" }],
  size: 4096,
  checksum: "a".repeat(64),
  state: "complete",
};
interface DetailOptions extends ServerDetailData {
  onRequest?: (path: string, init?: RequestInit) => unknown | Promise<unknown>;
}
function detail(options: DetailOptions) {
  const currentServer = {
    ...server,
    permissions: [...SERVER_CAPABILITIES],
  };
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
    const response = await options.onRequest?.(path, init);
    if (response !== undefined) return response;
    if (init?.method && init.method !== "GET")
      return { operation: { ...completedUpdate, status: "queued" }, ok: true };
    return serverDetailResponse(path, currentServer, {
      ...options,
      operations: options.operations ?? [completedUpdate],
    });
  });
  return render(
    <TestProviders
      user={{ id: "user1", role: "admin", username: "admin" }}
      pathname={`/servers/${server.id}`}
      navigate={mock()}
    >
      <ServerDetail serverId={server.id} />
    </TestProviders>,
  );
}

describe("server detail request ownership", () => {
  it("ignores an older refresh after a newer binding snapshot arrives", async () => {
    let completeOlder!: (value: unknown) => void;
    const older = new Promise((resolve) => { completeOlder = resolve; });
    let reads = 0;
    detail({ onRequest: (path) => {
      if (path !== `/servers/${server.id}`) return;
      reads += 1;
      if (reads === 2) return older;
      if (reads >= 3) return { server: { ...server, bindingStatus: "review_required", permissions: ["server.view"] } };
    } });
    await screen.findByRole("button", { name: "Refresh" });
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByText(/This server’s binding is review required/);
    await act(async () => completeOlder({ server: { ...server, state: "exited", permissions: [...SERVER_CAPABILITIES] } }));
    expect(screen.getByText(/This server’s binding is review required/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Start", exact: true })).toBeNull();
  });

  it("blocks an already-open restore when an operation starts", async () => {
    let active = false;
    detail({ backups: [backup], onRequest: (path) => {
      if (path.endsWith("/operations") && active) return { operations: [{ ...completedUpdate, status: "running" }] };
    } });
    await userEvent.click(await screen.findByRole("tab", { name: "Backups" }));
    await userEvent.click(screen.getByRole("button", { name: "Restore…" }));
    fireEvent.change(screen.getByRole("textbox", { name: /to confirm/ }), { target: { value: server.displayName } });
    await userEvent.click(screen.getByRole("tab", { name: "Activity" }));
    active = true;
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await userEvent.click(screen.getByRole("tab", { name: "Backups" }));
    const submit = screen.getByRole("button", { name: "Restore game data" });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.submit(submit.closest("form")!);
    expect(apiJsonMock.mock.calls.some(([path, , init]) => path.endsWith("/restores") && init?.method === "POST")).toBe(false);
  });

  it("consumes update confirmation when the action succeeds but its refresh fails", async () => {
    let queued = false;
    detail({ onRequest: (path, init) => {
      if (path.endsWith("/updates") && init?.method === "POST") {
        queued = true;
        return { operation: { ...completedUpdate, status: "queued" } };
      }
      if (path === `/servers/${server.id}` && queued) throw new Error("Unable to read updated server");
    } });
    await userEvent.click(await screen.findByRole("tab", { name: "Update", exact: true }));
    await userEvent.click(screen.getByRole("checkbox", { name: /I understand/ }));
    await userEvent.click(screen.getByRole("button", { name: "Update server", exact: true }));
    await screen.findByText("Update queued.");
    await screen.findByText("Unable to read updated server");
    await waitFor(() => expect(screen.getByRole("tab", { name: "Activity", selected: true })).toBeTruthy());
    await userEvent.click(screen.getByRole("tab", { name: "Update", exact: true }));
    expect((screen.getByRole("checkbox", { name: /I understand/ }) as HTMLInputElement).checked).toBe(false);
    expect((screen.getByRole("button", { name: "Update server", exact: true }) as HTMLButtonElement).disabled).toBe(true);
  });
});
