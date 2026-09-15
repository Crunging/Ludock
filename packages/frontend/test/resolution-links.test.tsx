import { describe, expect, it, mock, spyOn } from "bun:test";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SERVER_CAPABILITIES, type AuthUser, type Server } from "@ludock/shared";
import { ApiRequestError, apiJson } from "../src/api";
import { NavLink, NavigationProvider } from "../src/navigation";
import ServerDetail from "../src/pages/ServerDetail";
import TestProviders from "./TestProviders";
import { operationFixture, scheduleFixture, serverDetailResponse, serverFixture } from "./fixtures";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({ ...originalApi, apiJson: apiJsonMock }));
const server = serverFixture({ permissions: [...SERVER_CAPABILITIES] });
const base = `/servers/${server.id}`;
const operation = operationFixture(server.id, { kind: "backup", status: "failed", error: "Backup storage is unavailable." });
const schedule = scheduleFixture(server.id, { nextRunAt: null, nextRunUnavailableReason: "owner_disabled" });
const operationSearch = `?tab=activity&operation=${operation.id}`;
const scheduleSearch = `?tab=schedules&schedule=${schedule.id}`;

function detail(search: string, options: {
  role?: AuthUser["role"];
  current?: Server;
  onRequest?: (path: string, init?: RequestInit) => unknown | Promise<unknown>;
} = {}) {
  window.history.replaceState({}, "", `${base}${search}`);
  spyOn(window, "scrollTo").mockImplementation(() => {});
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
    const custom = await options.onRequest?.(path, init);
    if (custom !== undefined) return custom;
    if (path === `/operations/${operation.id}`) return { operation };
    return serverDetailResponse(path, options.current ?? server, { schedules: [schedule] });
  });
  return render(
    <TestProviders user={{ id: "user1", username: "friend", role: options.role ?? "admin" }} pathname={base} navigate={mock()}>
      <NavigationProvider>
        <NavLink to={`${base}${operationSearch}`}>Open failed operation</NavLink>
        <NavLink to={`${base}${scheduleSearch}`}>Open suspended schedule</NavLink>
        <NavLink to={`${base}?tab=availability`}>Open availability</NavLink>
        <ServerDetail serverId={server.id} />
      </NavigationProvider>
    </TestProviders>,
  );
}

describe("server resolution links", () => {
  it("updates in-page tabs without adding history entries while resolution links remain navigable", async () => {
    detail("");
    const historyLength = window.history.length;
    await userEvent.click(await screen.findByRole("tab", { name: "Schedules", exact: true }));
    expect(window.location.search).toBe("?tab=schedules");
    expect(window.history.length).toBe(historyLength);
    await userEvent.click(screen.getByRole("link", { name: "Open failed operation" }));
    await screen.findByText("Backup storage is unavailable.");
    expect(window.history.length).toBe(historyLength + 1);
    await act(async () => { window.history.back(); });
    await screen.findByRole("tabpanel", { name: "Schedules" });
  });

  it("opens a direct operation link beyond recent history and preserves the server controls", async () => {
    detail(operationSearch);
    const region = await screen.findByRole("region", { name: "Operation details" });
    await within(region).findByText("Backup storage is unavailable.");
    expect(document.activeElement).toBe(within(region).getByRole("heading"));
    expect((screen.getByRole("button", { name: "Stop", exact: true }) as HTMLButtonElement).disabled).toBe(false);
    await userEvent.click(within(region).getByRole("button", { name: "Close operation" }));
    expect(window.location.search).toBe("?tab=activity");
    expect(screen.queryByRole("region", { name: "Operation details" })).toBeNull();
  });

  it("tracks same-server links and back/forward while preserving schedule drafts", async () => {
    detail(scheduleSearch);
    await screen.findByRole("tabpanel", { name: "Schedules" });
    await waitFor(() => expect(document.activeElement?.id).toBe(`schedule-${schedule.id}`));
    fireEvent.change(screen.getByRole("combobox", { name: "Time zone" }), { target: { value: "Europe/London" } });
    await userEvent.click(screen.getByRole("link", { name: "Open failed operation" }));
    await screen.findByText("Backup storage is unavailable.");
    expect(window.scrollTo).not.toHaveBeenCalled();
    await act(async () => { window.history.back(); });
    await screen.findByRole("tabpanel", { name: "Schedules" });
    expect((screen.getByRole("combobox", { name: "Time zone" }) as HTMLInputElement).value).toBe("Europe/London");
    await waitFor(() => expect(document.activeElement?.id).toBe(`schedule-${schedule.id}`));
    await act(async () => { window.history.forward(); });
    await screen.findByRole("region", { name: "Operation details" });
    expect(window.location.search).toBe(operationSearch);
  });

  it.each(["schedules", "backups", "update", "unknown"])("falls back safely from an inaccessible %s link", async (tab) => {
    detail(`?tab=${tab}&schedule=${schedule.id}`, { role: "viewer", current: { ...server, permissions: ["server.view"] } });
    await screen.findByRole("tabpanel", { name: "Activity" });
    expect(screen.queryByRole("tab", { name: /Schedules|Backups|Update/ })).toBeNull();
    expect(apiJsonMock.mock.calls.some(([path]) => /\/(schedules|backups|update-capability)$/.test(path))).toBe(false);
  });

  it("focuses a suspended schedule even when editing is unavailable", async () => {
    detail(scheduleSearch, { current: { ...server, bindingStatus: "review_required" } });
    const edit = await screen.findByRole("button", { name: "Edit", exact: true });
    await waitFor(() => expect(document.activeElement?.id).toBe(`schedule-${schedule.id}`));
    expect((edit as HTMLButtonElement).disabled).toBe(true);
    expect(document.activeElement?.getAttribute("aria-current")).toBe("true");
  });

  it("ignores an obsolete operation response after navigating to availability", async () => {
    let finish!: (value: unknown) => void;
    const pending = new Promise((resolve) => { finish = resolve; });
    detail(operationSearch, { onRequest: (path) => path === `/operations/${operation.id}` ? pending : undefined });
    await screen.findByText("Loading operation…");
    await userEvent.click(screen.getByRole("link", { name: "Open availability" }));
    await screen.findByRole("tabpanel", { name: "Availability" });
    await act(async () => { finish({ operation }); });
    expect(screen.queryByText("Backup storage is unavailable.")).toBeNull();
    expect(screen.queryByRole("region", { name: "Operation details" })).toBeNull();
  });

  it("does not show inaccessible operation details", async () => {
    detail(operationSearch, { onRequest: (path) => {
      if (path === `/operations/${operation.id}`) throw new ApiRequestError("Denied", 403);
    } });
    await screen.findByText("This operation is no longer available or you no longer have access.");
    expect(screen.queryByText("Backup storage is unavailable.")).toBeNull();
  });

  it("shows read-only availability to viewers without monitoring controls", async () => {
    detail("?tab=availability", { role: "viewer", current: { ...server, permissions: ["server.view"] } });
    await screen.findByText("Monitoring disabled.");
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(screen.queryByRole("button", { name: "Save monitoring" })).toBeNull();
  });

  it("keeps saved resolutions readable while Docker is unavailable and disables mutations", async () => {
    detail(scheduleSearch, { onRequest: (path) => path === base ? {
      server: { ...server, state: "unknown", status: "Live status unavailable", ports: [], image: "", fileRoots: [], gameConsole: null },
      stats: null,
      discoveryUnavailable: true,
    } : undefined });
    await screen.findByText(/Live server status could not be verified. Saved history and monitoring remain accessible/);
    await waitFor(() => expect(document.activeElement?.id).toBe(`schedule-${schedule.id}`));
    expect((screen.getByRole("button", { name: "Edit", exact: true }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Pause", exact: true }) as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(screen.getByRole("link", { name: "Open failed operation" }));
    await screen.findByText("Backup storage is unavailable.");
    await userEvent.click(screen.getByRole("link", { name: "Open availability" }));
    expect((await screen.findByRole("button", { name: "Save monitoring" }) as HTMLButtonElement).disabled).toBe(true);
    expect(apiJsonMock.mock.calls.some(([, , init]) => init?.method && init.method !== "GET")).toBe(false);
  });

  it("refreshes availability observations without overwriting monitoring drafts", async () => {
    const timers: (() => void)[] = [];
    spyOn(window, "setInterval").mockImplementation(((callback: () => void, delay: number) => {
      if (delay === 10000) timers.push(callback);
      return timers.length;
    }) as typeof window.setInterval);
    spyOn(window, "clearInterval").mockImplementation(() => {});
    let outage = false;
    detail("?tab=availability", { onRequest: (path) => {
      if (path.endsWith("/availability") && outage) return {
        policy: { enabled: true, maintenance: false, graceSeconds: 120 },
        state: { outageStartedAt: Date.now() - 300000, lastState: "exited", notified: true, intentionallyStopped: false, suppressedUntil: 0 },
      };
    } });
    const grace = await screen.findByRole("spinbutton", { name: "Failure grace period (seconds)" });
    fireEvent.change(grace, { target: { value: "240" } });
    outage = true;
    await act(async () => { for (const timer of timers) timer(); });
    await screen.findByText("Availability problem detected.");
    expect((screen.getByRole("spinbutton", { name: "Failure grace period (seconds)" }) as HTMLInputElement).value).toBe("240");
  });
});
