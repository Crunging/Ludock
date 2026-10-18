import { describe, expect, it, mock } from "bun:test";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SERVER_CAPABILITIES, nextScheduleRun, type Schedule, type ServerCapability } from "@ludock/shared";
import { ApiRequestError, apiJson } from "../src/api";
import ServerDetail from "../src/pages/ServerDetail";
import { NavigationProvider } from "../src/navigation";
import TestProviders from "./TestProviders";
import { operationFixture, scheduleFixture, serverDetailResponse, serverFixture } from "./fixtures";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({ ...originalApi, apiJson: apiJsonMock }));
const server = serverFixture({ permissions: [...SERVER_CAPABILITIES] });
const base = `/servers/${server.id}`;
const itemPath = `${base}/schedules/77777777-7777-4777-8777-777777777777`;
const broadcastConsole = {
  id: "minecraft-rcon", name: "Minecraft RCON", commandPlaceholder: "say Hello", commands: [], supportsBroadcast: true,
};

function detail({
  onRequest,
  saved,
  without = [],
  broadcast = false,
}: {
  onRequest?: (path: string, init?: RequestInit) => unknown | Promise<unknown>;
  saved?: Partial<Schedule>;
  without?: ServerCapability[];
  broadcast?: boolean;
} = {}) {
  window.history.replaceState({}, "", base);
  let schedule = scheduleFixture(server.id, saved);
  const visibleServer = {
    ...server,
    gameConsole: broadcast ? broadcastConsole : server.gameConsole,
    permissions: server.permissions.filter((capability) => !without.includes(capability)),
  };
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
    const custom = await onRequest?.(path, init);
    if (custom !== undefined) return custom;
    if (path === `${base}/schedules` && init?.method === "POST") {
      schedule = { ...schedule, ...JSON.parse(init.body as string) };
      return { schedule };
    }
    if (path === itemPath && init?.method === "PUT") {
      const input = JSON.parse(init.body as string);
      schedule = { ...schedule, ...input, revision: schedule.revision + 1 };
      schedule.nextRunAt = nextScheduleRun(schedule, Date.now(), schedule.lastSlot);
      return { schedule };
    }
    return serverDetailResponse(path, visibleServer, {
      schedules: [schedule],
      operations: [],
    });
  });
  const content = (visible: boolean) => (
    <TestProviders user={{ id: schedule.ownerId, username: "friend", role: "admin" }} pathname={base} navigate={mock()}>
      <NavigationProvider>
        {visible ? <ServerDetail serverId={server.id} /> : <p>Other page</p>}
      </NavigationProvider>
    </TestProviders>
  );
  const view = render(content(true));
  return {
    get schedule() { return schedule; },
    replace: (updated: Schedule) => { schedule = updated; },
    leave: () => view.rerender(content(false)),
    return: () => view.rerender(content(true)),
  };
}

async function openSchedules() {
  await userEvent.click(await screen.findByRole("tab", { name: "Schedules", exact: true }));
}

function writes(method?: string) {
  return apiJsonMock.mock.calls.filter(([, , init]) =>
    ["POST", "PUT", "PATCH", "DELETE"].includes(init?.method ?? "") && (!method || init?.method === method));
}

function draftTime(value: string) {
  fireEvent.change(screen.getByLabelText("Time", { exact: true }), { target: { value } });
}

function draftTimezone(value: string) {
  fireEvent.change(screen.getByLabelText("Time zone"), { target: { value } });
}

const save = () => screen.getByRole("button", { name: "Save changes" }) as HTMLButtonElement;

const completedOnce: Partial<Schedule> = {
  kind: "once",
  date: "2026-09-11",
  lastSlot: "2026-09-11T09:00:UTC",
  nextRunAt: null,
  completedAt: Date.UTC(2026, 8, 11, 9),
};

describe("schedules", () => {
  it("creates a restart schedule with an optional backup and clears the option when changing action", async () => {
    detail();
    await openSchedules();
    expect(screen.queryByRole("checkbox", { name: "Create a backup before restarting" })).toBeNull();
    const action = screen.getByRole("combobox", { name: "Action", exact: true });
    await userEvent.selectOptions(action, "restart");
    const backup = () => screen.getByRole("checkbox", { name: "Create a backup before restarting" }) as HTMLInputElement;
    expect(backup().checked).toBe(false);
    await userEvent.click(backup());
    await userEvent.selectOptions(action, "stop");
    expect(screen.queryByRole("checkbox", { name: "Create a backup before restarting" })).toBeNull();
    await userEvent.selectOptions(action, "restart");
    expect(backup().checked).toBe(false);
    await userEvent.click(backup());
    await userEvent.click(screen.getByRole("button", { name: "Add schedule", exact: true }));
    await screen.findByText("Schedule created.");
    expect(JSON.parse(writes("POST")[0][2]!.body as string)).toMatchObject({ action: "restart", createBackup: true });
  });

  it("shows a saved backup restart and preserves the option when editing other settings", async () => {
    const view = detail({ saved: { createBackup: true } });
    await openSchedules();
    expect(screen.getByRole("cell", { name: "restart Backup before restart" })).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    expect((screen.getByRole("checkbox", { name: "Create a backup before restarting" }) as HTMLInputElement).checked).toBe(true);
    draftTime("14:45");
    await userEvent.click(save());
    await screen.findByText("Schedule saved.");
    expect(JSON.parse(writes("PUT")[0][2]!.body as string)).toMatchObject({ createBackup: true, time: "14:45", revision: 1 });
    expect(view.schedule.createBackup).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Create a backup before restarting" }));
    await userEvent.click(save());
    await screen.findByText("Schedule saved.");
    expect(JSON.parse(writes("PUT")[1][2]!.body as string)).toMatchObject({ createBackup: false, revision: 2 });
  });

  it("offers the option only with backup access but allows removing it after access is revoked", async () => {
    detail({ without: ["backups.create"], saved: { createBackup: true, enabled: false, nextRunAt: null } });
    await openSchedules();
    expect((screen.getByRole("button", { name: "Resume", exact: true }) as HTMLButtonElement).disabled).toBe(true);
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Action", exact: true }), "restart");
    const backup = () => screen.queryByRole("checkbox", { name: "Create a backup before restarting" }) as HTMLInputElement | null;
    expect(backup()).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    expect(backup()?.checked).toBe(true);
    expect(save().disabled).toBe(true);
    await userEvent.click(backup()!);
    expect(backup()).toBeNull();
    expect(save().disabled).toBe(false);
    await userEvent.click(save());
    await screen.findByText("Schedule saved.");
    expect(JSON.parse(writes("PUT")[0][2]!.body as string)).toMatchObject({ createBackup: false, enabled: false });
  });

  it("edits and saves countdown settings, rejecting invalid leads and clearing warnings for other actions", async () => {
    detail({ broadcast: true });
    await openSchedules();
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    await userEvent.click(screen.getByLabelText("Announce countdown to players"));
    const leads = screen.getByLabelText(/Warning lead times/) as HTMLInputElement;
    expect(leads.value).toBe("10, 5, 1");
    fireEvent.change(leads, { target: { value: "10, 10" } });
    expect(save().disabled).toBe(true);
    expect(screen.getByRole("alert").textContent).toContain("unique whole numbers");
    fireEvent.change(leads, { target: { value: "8, " } });
    expect(leads.value).toBe("8, ");
    fireEvent.change(leads, { target: { value: "8, 3, 1" } });
    expect(save().disabled).toBe(false);
    await userEvent.click(save());
    await screen.findByText("Schedule saved.");
    expect(JSON.parse(writes("PUT")[0][2]!.body as string)).toMatchObject({ announcements: { leadMinutes: [8, 3, 1] } });
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    expect((screen.getByLabelText(/Warning lead times/) as HTMLInputElement).value).toBe("8, 3, 1");
    await userEvent.selectOptions(screen.getByLabelText("Action"), "start");
    expect(screen.queryByLabelText("Announce countdown to players")).toBeNull();
    await userEvent.click(save());
    await waitFor(() => expect(writes("PUT")).toHaveLength(2));
    expect(JSON.parse(writes("PUT")[1][2]!.body as string).announcements).toBeUndefined();
  });

  it("requires console access to resume or save saved warnings, but allows turning them off", async () => {
    detail({ broadcast: true, without: ["console.execute"], saved: { announcements: { leadMinutes: [5] }, enabled: false, nextRunAt: null } });
    await openSchedules();
    expect((screen.getByRole("button", { name: "Resume", exact: true }) as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    expect(save().disabled).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Turn off saved warnings" }));
    expect(save().disabled).toBe(false);
    await userEvent.click(save());
    await screen.findByText("Schedule saved.");
    expect(JSON.parse(writes("PUT")[0][2]!.body as string).announcements).toBeUndefined();
  });

  it("hides the countdown control without a supported broadcast console", async () => {
    detail();
    await openSchedules();
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    expect(screen.queryByLabelText("Announce countdown to players")).toBeNull();
    expect(screen.getByText(/Player warnings require a supported broadcast console/)).toBeTruthy();
  });

  it("requires review after a revision conflict and preserves an intervening pause when retaining the edit draft", async () => {
    let reject = true;
    const view = detail({ onRequest: (path, init) => {
      if (path === itemPath && init?.method === "PUT" && reject) {
        reject = false;
        view.replace({ ...view.schedule, time: "10:30", enabled: false, nextRunAt: null, revision: 2 });
        throw new ApiRequestError("This schedule changed. Reload before saving again.", 409);
      }
    } });
    await openSchedules();
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    draftTime("14:45");
    await userEvent.click(save());
    await screen.findByText(/This schedule changed elsewhere/);
    expect(screen.getByText(/Current saved settings:/).parentElement!.textContent).toContain("10:30");
    expect(save().disabled).toBe(true);
    expect((screen.getByLabelText("Time", { exact: true }) as HTMLInputElement).value).toBe("14:45");
    await userEvent.click(screen.getByRole("button", { name: "Keep my draft" }));
    expect(screen.getByText(/This schedule stays paused/)).toBeTruthy();
    await userEvent.click(save());
    await screen.findByText("Schedule saved.");
    expect(JSON.parse(writes("PUT")[1][2]!.body as string)).toMatchObject({ time: "14:45", enabled: false, revision: 2 });
  });

  it("prevents duplicate saves and ignores an old save response after leaving the page", async () => {
    let finish!: () => void;
    const pending = new Promise((resolve) => { finish = () => resolve({ schedule: scheduleFixture(server.id, { revision: 2 }) }); });
    const view = detail({ onRequest: (path, init) => path === itemPath && init?.method === "PUT" ? pending : undefined });
    await openSchedules();
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    draftTime("14:45");
    fireEvent.click(save());
    fireEvent.click(save());
    await waitFor(() => expect(writes("PUT")).toHaveLength(1));
    expect((screen.getByLabelText("Time", { exact: true }) as HTMLInputElement).disabled).toBe(true);
    view.leave();
    view.return();
    await openSchedules();
    draftTimezone("Europe/London");
    const calls = apiJsonMock.mock.calls.length;
    await act(async () => finish());
    expect((screen.getByLabelText("Time zone") as HTMLInputElement).value).toBe("Europe/London");
    expect(apiJsonMock.mock.calls.length).toBe(calls);
    expect(screen.queryByText("Schedule saved.")).toBeNull();
  });

  it("creates a one-time schedule at a future date and time instead of on chosen days", async () => {
    detail();
    await openSchedules();
    await userEvent.click(screen.getByRole("radio", { name: "Once" }));
    expect(screen.queryByRole("group", { name: "Days" })).toBeNull();
    const add = screen.getByRole("button", { name: "Add schedule" }) as HTMLButtonElement;
    fireEvent.change(screen.getByLabelText("Date"), { target: { value: "2000-01-01" } });
    expect(add.disabled).toBe(true);
    expect(screen.getByText("Choose a future date and time that exists in this time zone.")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Date"), { target: { value: "2099-01-01" } });
    draftTime("06:15");
    draftTimezone("UTC");
    expect(add.disabled).toBe(false);
    await userEvent.click(add);
    await screen.findByText("Schedule created.");
    const body = JSON.parse(writes("POST")[0][2]!.body as string);
    expect(body).toMatchObject({ kind: "once", date: "2099-01-01", time: "06:15", timezone: "UTC" });
    expect(body.days).toBeUndefined();
  });

  it("restores chosen days when switching back to a recurring schedule", async () => {
    detail();
    await openSchedules();
    await userEvent.click(screen.getByRole("button", { name: "Weekends" }));
    await userEvent.click(screen.getByRole("radio", { name: "Once" }));
    await userEvent.click(screen.getByRole("radio", { name: "On chosen days" }));
    expect(screen.getByRole("button", { name: "Weekends" }).getAttribute("aria-pressed")).toBe("true");
  });

  it("shows a completed one-time schedule without a next run or resume", async () => {
    detail({ saved: completedOnce });
    await openSchedules();
    const row = screen.getByText("Once, 2026-09-11 at 09:00").closest("tr")!;
    expect(row.textContent).toContain("Completed");
    expect(row.textContent).toContain("None");
    expect(row.textContent).not.toContain("The next run cannot be determined");
    expect((within(row).getByRole("button", { name: "Pause" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("pauses a completed one-time schedule while its operation is still queued", async () => {
    const completed = scheduleFixture(server.id, {
      ...completedOnce,
      lastOperation: operationFixture(server.id, { kind: "restart", status: "queued", phase: "queued" }),
    });
    detail({ saved: completed, onRequest: (path, init) => path === itemPath && init?.method === "PATCH"
      ? { schedule: { ...completed, enabled: false, revision: 2 } } : undefined });
    await openSchedules();
    const row = screen.getByText("Once, 2026-09-11 at 09:00").closest("tr")!;
    await userEvent.click(within(row).getByRole("button", { name: "Pause" }));
    await screen.findByText("Schedule paused.");
    expect(JSON.parse(writes("PATCH")[0][2]!.body as string)).toEqual({ enabled: false, revision: 1 });
  });
});
