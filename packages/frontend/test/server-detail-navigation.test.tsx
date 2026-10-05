import { describe, expect, it, mock } from "bun:test";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SERVER_CAPABILITIES } from "@ludock/shared";
import { apiJson } from "../src/api";
import TestProviders from "./TestProviders";
import { operationFixture, scheduleFixture, serverDetailResponse, serverFixture } from "./fixtures";
import ServerDetail from "../src/pages/ServerDetail";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({
  ...originalApi,
  apiJson: apiJsonMock,
}));

const server = serverFixture({
  permissions: [...SERVER_CAPABILITIES],
});

function detail({
  onRequest,
}: {
  onRequest?: (path: string, init?: RequestInit) => unknown | Promise<unknown>;
} = {}) {
  const navigate = mock();
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
    const response = await onRequest?.(path, init);
    if (response !== undefined) return response;
    return serverDetailResponse(path, server);
  });
  const content = (visible: boolean) => (
    <TestProviders user={{ id: "user1", role: "admin", username: "friend" }} pathname={`/servers/${server.id}`} navigate={navigate}>
      {visible ? <ServerDetail serverId={server.id} /> : <p>Server tools</p>}
    </TestProviders>
  );
  const view = render(content(true));
  return {
    leaveDetail: () => view.rerender(content(false)),
    returnToDetail: () => view.rerender(content(true)),
  };
}

describe("server overview", () => {
  it("opens on an overview with the address, resources, and next schedule", async () => {
    const nextRunAt = Date.now() + 2 * 3_600_000;
    detail({
      onRequest: (path) => {
        if (path === `/servers/${server.id}`) return {
          server: { ...server, connection: { host: "play.example.com", port: 25565 } },
          stats: { cpuPercent: 42.25, memUsageMB: 2048, memLimitMB: 8192 },
        };
        if (path.endsWith("/schedules"))
          return { schedules: [scheduleFixture(server.id, { action: "restart", nextRunAt })] };
      },
    });
    expect((await screen.findByRole("tab", { name: "Overview" })).getAttribute("aria-selected")).toBe("true");
    await screen.findByText("play.example.com:25565");
    expect(screen.getByRole("button", { name: `Copy address for ${server.displayName}` })).toBeTruthy();
    expect(screen.getByText("42.3%")).toBeTruthy();
    expect(screen.getByText("2 GiB")).toBeTruthy();
    expect(screen.getByText("of 8 GiB")).toBeTruthy();
    await screen.findByText("in 2 hours");
    expect(screen.getByRole("heading", { name: "Monitoring" })).toBeTruthy();
  });
});

describe("server detail navigation", () => {
  it("keeps the new visit's selected tab when an old update request completes", async () => {
    let finishUpdate!: () => void;
    const pending = new Promise((resolve) => {
      finishUpdate = () => resolve({ operation: operationFixture(server.id, { status: "queued" }) });
    });
    const view = detail({
      onRequest: (path, init) => {
        if (path.endsWith("/updates") && init?.method === "POST") return pending;
      },
    });
    await userEvent.click(await screen.findByRole("tab", { name: "Update", exact: true }));
    await userEvent.click(screen.getByRole("checkbox", { name: /I understand/ }));
    await userEvent.click(screen.getByRole("button", { name: "Update server", exact: true }));
    await waitFor(() => expect(apiJsonMock.mock.calls.some(
      ([path, , init]) => path.endsWith("/updates") && init?.method === "POST",
    )).toBe(true));

    view.leaveDetail();
    view.returnToDetail();
    await userEvent.click(await screen.findByRole("tab", { name: "Schedules" }));
    const timezone = screen.getByRole("combobox", { name: "Time zone" });
    await userEvent.clear(timezone);
    await userEvent.type(timezone, "Europe/London");
    const requestsBeforeCompletion = apiJsonMock.mock.calls.length;
    await act(async () => { finishUpdate(); });

    expect(screen.getByRole("tabpanel", { name: "Schedules" })).toBeTruthy();
    expect((timezone as HTMLInputElement).value).toBe("Europe/London");
    expect(apiJsonMock.mock.calls.length).toBe(requestsBeforeCompletion);
  });
});
