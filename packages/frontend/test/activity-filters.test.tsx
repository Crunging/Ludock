import { describe, expect, it, mock } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SERVER_CAPABILITIES, type Operation } from "@ludock/shared";
import { apiJson } from "../src/api";
import ServerDetail from "../src/pages/ServerDetail";
import TestProviders from "./TestProviders";
import { operationFixture, serverDetailResponse, serverFixture } from "./fixtures";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({ ...originalApi, apiJson: apiJsonMock }));
const server = serverFixture({ permissions: [...SERVER_CAPABILITIES] });
const path = `/servers/${server.id}`;
const running = operationFixture(server.id, { id: "11111111-1111-4111-8111-111111111111", kind: "backup", status: "running", phase: "Copying game data" });
const failed = operationFixture(server.id, { id: "22222222-2222-4222-8222-222222222222", kind: "restart", status: "failed", error: "Restart failed" });
const completed = operationFixture(server.id, { id: "33333333-3333-4333-8333-333333333333", kind: "stop", status: "succeeded" });
const interrupted = operationFixture(server.id, { id: "44444444-4444-4444-8444-444444444444", kind: "restart", status: "interrupted" });

function detail(initial: Operation[] = [running, failed, completed, interrupted]) {
  let operations = initial;
  apiJsonMock.mockImplementation(async (requestPath) => serverDetailResponse(requestPath, server, { operations }));
  render(
    <TestProviders user={{ id: "user1", username: "friend", role: "admin" }} pathname={path} navigate={mock()}>
      <ServerDetail serverId={server.id} />
    </TestProviders>,
  );
  return { replace: (next: Operation[]) => { operations = next; } };
}

const table = () => within(screen.getByRole("table", { name: "Recent operations" }));
const statusFilter = () => screen.getByRole("combobox", { name: "Status", exact: true });
const kindFilter = () => screen.getByRole("combobox", { name: "Operation", exact: true });

describe("activity filters", () => {
  it("combines status and operation filters without hiding the existence of active work", async () => {
    detail();
    await screen.findByText("Showing 4 of 4 recent operations.");
    await userEvent.selectOptions(kindFilter(), "restart");
    expect(table().getAllByRole("row")).toHaveLength(3);
    await userEvent.selectOptions(statusFilter(), "failed");
    expect(table().getAllByRole("row")).toHaveLength(2);
    expect(table().getByText("Restart failed")).toBeTruthy();
    expect(screen.getByText("Showing 1 of 4 recent operations.")).toBeTruthy();
    expect(screen.getByText(/An active operation is hidden/)).toBeTruthy();
    expect((screen.getByRole("button", { name: "Stop", exact: true }) as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Show active work" }));
    expect(table().getByText("Copying game data")).toBeTruthy();
    expect((statusFilter() as HTMLSelectElement).value).toBe("all");
    expect((kindFilter() as HTMLSelectElement).value).toBe("all");
  });

  it("retains selections when refresh removes the last matching operation and offers a clear empty state", async () => {
    const view = detail([failed, completed]);
    await screen.findByText("Showing 2 of 2 recent operations.");
    await userEvent.selectOptions(kindFilter(), "restart");
    await userEvent.selectOptions(statusFilter(), "failed");
    view.replace([completed]);
    await userEvent.click(screen.getByRole("button", { name: "Refresh", exact: true }));
    await screen.findByText("No recent operations match these filters.");
    expect((kindFilter() as HTMLSelectElement).value).toBe("restart");
    expect((statusFilter() as HTMLSelectElement).value).toBe("failed");
    expect(screen.getByText("Showing 0 of 1 recent operations.")).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(table().getByText("Completed")).toBeTruthy();
    expect(screen.queryByRole("option", { name: "restart", exact: true })).toBeNull();
    expect(apiJsonMock.mock.calls.every(([, , init]) => !init?.method || init.method === "GET")).toBe(true);
  });

  it("preserves filters across tabs while View progress explicitly reveals running work", async () => {
    detail();
    await screen.findByText("Showing 4 of 4 recent operations.");
    await userEvent.selectOptions(kindFilter(), "restart");
    await userEvent.selectOptions(statusFilter(), "failed");
    await userEvent.click(screen.getByRole("tab", { name: "Backups", exact: true }));
    await userEvent.click(screen.getByRole("tab", { name: "Activity", exact: true }));
    expect((statusFilter() as HTMLSelectElement).value).toBe("failed");
    expect((kindFilter() as HTMLSelectElement).value).toBe("restart");
    await userEvent.click(screen.getByRole("tab", { name: "Backups", exact: true }));
    await userEvent.click(screen.getByRole("button", { name: "View progress", exact: true }));
    expect((statusFilter() as HTMLSelectElement).value).toBe("all");
    expect(table().getByText("Copying game data")).toBeTruthy();
  });

  it("keeps the empty history distinct from a filtered list", async () => {
    detail([]);
    await screen.findByText("No operations yet.");
    await userEvent.selectOptions(statusFilter(), "queued");
    expect(table().getByText("No operations yet.")).toBeTruthy();
    expect(screen.queryByText("No recent operations match these filters.")).toBeNull();
  });
});
