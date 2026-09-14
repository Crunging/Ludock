import { describe, expect, it, mock, spyOn } from "bun:test";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SERVER_CAPABILITIES, nextScheduleRun, scheduleSlot, type Operation, type Schedule, type Server } from "@ludock/shared";
import { ApiRequestError, apiJson } from "../src/api";
import type { AuthUser } from "../src/auth-context";
import ServerDetail from "../src/pages/ServerDetail";
import TestProviders from "./TestProviders";
import { operationFixture, scheduleFixture, serverDetailResponse, serverFixture } from "./fixtures";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({ ...originalApi, apiJson: apiJsonMock }));
const server = serverFixture({ permissions: [...SERVER_CAPABILITIES] });
const base = `/servers/${server.id}`;
const itemPath = `${base}/schedules/77777777-7777-4777-8777-777777777777`;

function detail({
  initial = scheduleFixture(server.id),
  current = server,
  role = "admin",
  activeOperation = false,
  onRequest,
}: {
  initial?: Schedule;
  current?: Server;
  role?: AuthUser["role"];
  activeOperation?: boolean;
  onRequest?: (path: string, init?: RequestInit) => unknown | Promise<unknown>;
} = {}) {
  let schedule = initial;
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
    const custom = await onRequest?.(path, init);
    if (custom !== undefined) return custom;
    if (path === `${base}/schedules` && init?.method === "POST") {
      schedule = scheduleFixture(server.id, {
        ...JSON.parse(init.body as string),
        lastOperation: null, lastRunAt: null, lastResult: null,
      });
      schedule.nextRunAt = nextScheduleRun(schedule, Date.now());
      return { schedule };
    }
    if (schedule.lastOperation && path === `/operations/${schedule.lastOperation.id}`)
      return { operation: schedule.lastOperation };
    if (path === itemPath && (init?.method === "PUT" || init?.method === "PATCH")) {
      const input = JSON.parse(init.body as string);
      schedule = { ...schedule, ...input, revision: schedule.revision + 1 };
      schedule.nextRunAt = nextScheduleRun(schedule, Date.now(), schedule.lastSlot);
      return { schedule };
    }
    return serverDetailResponse(path, current, {
      schedules: [schedule],
      operations: activeOperation ? [operationFixture(server.id, { status: "running" })] : [],
    });
  });
  const content = (visible: boolean) => (
    <TestProviders user={{ id: initial.ownerId, username: "friend", role }} pathname={base} navigate={mock()}>
      {visible ? <ServerDetail serverId={server.id} /> : <p>Other page</p>}
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

const preview = () => document.querySelector(".schedule-preview")!;
const save = () => screen.getByRole("button", { name: "Save changes" }) as HTMLButtonElement;

describe("schedule management", () => {
  it("pauses and resumes with fresh revisions while keeping state separate from the last result", async () => {
    detail();
    await openSchedules();
    const row = within(screen.getByRole("table", { name: "Schedules" })).getAllByRole("row")[1];
    expect(within(row).getAllByRole("cell")[2].textContent).toBe("Enabled");
    await userEvent.click(within(row).getByRole("button", { name: "Pause" }));
    await screen.findByText("Schedule paused.");
    expect(within(row).getAllByRole("cell")[2].textContent).toBe("Paused");
    expect(within(row).getAllByRole("cell")[3].textContent).toBe("Paused");
    expect(within(row).getByText("Succeeded")).toBeTruthy();
    await userEvent.click(within(row).getByRole("button", { name: "Resume" }));
    await screen.findByText("Schedule resumed.");
    expect(writes("PATCH").map(([, , init]) => JSON.parse(init!.body as string))).toEqual([
      { enabled: false, revision: 1 }, { enabled: true, revision: 2 },
    ]);
  });

  it("allows a manager to pause after the action grant is revoked and while an operation is active", async () => {
    detail({
      role: "operator", activeOperation: true,
      current: { ...server, permissions: ["server.view", "schedules.manage"] },
    });
    await openSchedules();
    expect((screen.getByRole("button", { name: "Edit", exact: true }) as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Pause" }));
    await screen.findByText("Schedule paused.");
    expect((screen.getByRole("button", { name: "Resume" }) as HTMLButtonElement).disabled).toBe(true);
    expect(writes()).toHaveLength(1);
  });

  it("keeps separate add and edit drafts through tabs and a failed save, then updates the existing paused schedule", async () => {
    let reject = true;
    const view = detail({ initial: scheduleFixture(server.id, { enabled: false, nextRunAt: null }),
      onRequest: (path, init) => {
        if (path === itemPath && init?.method === "PUT" && reject) {
          reject = false;
          throw new ApiRequestError("Temporary failure. Try again.", 503);
        }
      },
    });
    await openSchedules();
    draftTimezone("Europe/London");
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Edit schedule" }));
    draftTime("14:45");
    draftTimezone("America/Los_Angeles");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Action" }), "stop");
    await userEvent.click(screen.getByRole("checkbox", { name: "Sun" }));
    await userEvent.click(screen.getByRole("tab", { name: "Activity" }));
    await openSchedules();
    expect((screen.getByLabelText("Time zone") as HTMLInputElement).value).toBe("America/Los_Angeles");
    expect(preview().textContent).toContain("Next run when resumed:");
    await userEvent.click(save());
    expect((await screen.findByRole("alert")).textContent).toContain("Temporary failure");
    expect((screen.getByLabelText("Time", { exact: true }) as HTMLInputElement).value).toBe("14:45");
    await userEvent.click(save());
    await screen.findByText("Schedule saved.");
    expect(view.schedule.id).toBe("77777777-7777-4777-8777-777777777777");
    expect(view.schedule.ownerId).toBe("88888888-8888-4888-8888-888888888888");
    expect(writes("PUT").map(([, , init]) => JSON.parse(init!.body as string))).toEqual([
      { action: "stop", enabled: false, time: "14:45", days: [1, 2, 3, 4, 5, 6], timezone: "America/Los_Angeles", revision: 1 },
      { action: "stop", enabled: false, time: "14:45", days: [1, 2, 3, 4, 5, 6], timezone: "America/Los_Angeles", revision: 1 },
    ]);
    expect((screen.getByLabelText("Time zone") as HTMLInputElement).value).toBe("Europe/London");
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

  it("preserves a draft when polling finds a new revision and can explicitly load the saved settings", async () => {
    const intervals = spyOn(window, "setInterval");
    const view = detail();
    await openSchedules();
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    draftTime("14:45");
    view.replace({ ...view.schedule, time: "11:15", revision: 2 });
    const poll = intervals.mock.calls.find(([, delay]) => delay === 10000)![0] as () => void;
    await act(async () => poll());
    await screen.findByText(/This schedule changed elsewhere/);
    expect((screen.getByLabelText("Time", { exact: true }) as HTMLInputElement).value).toBe("14:45");
    expect(save().disabled).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Load saved settings" }));
    expect((screen.getByLabelText("Time", { exact: true }) as HTMLInputElement).value).toBe("11:15");
    expect(save().disabled).toBe(false);
    expect(writes()).toHaveLength(0);
  });

  it("can repair a schedule by selecting another independently granted action", async () => {
    detail({ role: "operator", current: { ...server, permissions: ["server.view", "schedules.manage", "server.start"] } });
    await openSchedules();
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    expect(save().disabled).toBe(true);
    expect((screen.getByRole("combobox", { name: "Action" }) as HTMLSelectElement).value).toBe("");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Action" }), "start");
    await userEvent.click(save());
    await screen.findByText("Schedule saved.");
    expect(JSON.parse(writes("PUT")[0][2]!.body as string)).toMatchObject({ action: "start", revision: 1 });
  });

  it("does not preview a consumed fall-back slot while editing an existing schedule", async () => {
    const now = Date.UTC(2026, 10, 1, 6, 30);
    spyOn(Date, "now").mockReturnValue(now);
    const input = scheduleFixture(server.id, { time: "01:30", days: [0], timezone: "America/New_York" });
    input.lastSlot = scheduleSlot(input, Date.UTC(2026, 10, 1, 5, 30));
    input.nextRunAt = nextScheduleRun(input, now, input.lastSlot);
    detail({ initial: input });
    await openSchedules();
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    expect(preview().textContent).toContain("Nov 8, 2026");
    expect(preview().textContent).toContain("America/New_York");
    expect(preview().textContent).not.toContain("Due now");
  });

  it("updates due-now previews locally each minute and prevents saving invalid input", async () => {
    const clock = spyOn(Date, "now").mockReturnValue(Date.UTC(2026, 8, 11, 12));
    const intervals = spyOn(window, "setInterval");
    detail();
    await openSchedules();
    draftTime("12:00");
    draftTimezone("UTC");
    expect(preview().textContent).toContain("Due now");
    const calls = apiJsonMock.mock.calls.length;
    clock.mockReturnValue(Date.UTC(2026, 8, 11, 12, 1));
    const tick = intervals.mock.calls.find(([, delay]) => delay === 1000)![0] as () => void;
    await act(async () => tick());
    expect(preview().textContent).not.toContain("Due now");
    expect(preview().textContent).toContain("Sep 12, 2026");
    expect(apiJsonMock.mock.calls.length).toBe(calls);
    draftTimezone("Invalid/Timezone");
    expect(preview().textContent).toContain("Choose a valid time");
    expect((screen.getByRole("button", { name: "Add schedule" }) as HTMLButtonElement).disabled).toBe(true);
    expect(writes()).toHaveLength(0);
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
});


describe("schedule results and paused creation", () => {
  it("keeps create-paused choices through tabs, pending saves, and failure before creating without a run", async () => {
    let fail!: () => void;
    let first = true;
    const pending = new Promise((_resolve, reject) => { fail = () => reject(new ApiRequestError("Unable to save. Try again.", 503)); });
    const view = detail({ onRequest: (path, init) => {
      if (first && path === `${base}/schedules` && init?.method === "POST") {
        first = false;
        return pending;
      }
    } });
    await openSchedules();
    await userEvent.click(screen.getByRole("checkbox", { name: "Create paused" }));
    draftTimezone("UTC");
    draftTime("12:00");
    expect(preview().textContent).toContain("Next run when resumed:");
    await userEvent.click(screen.getByRole("tab", { name: "Activity" }));
    await openSchedules();
    expect((screen.getByRole("checkbox", { name: "Create paused" }) as HTMLInputElement).checked).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Add schedule" }));
    expect((screen.getByRole("checkbox", { name: "Create paused" }) as HTMLInputElement).disabled).toBe(true);
    await act(async () => fail());
    await screen.findByText("Unable to save. Try again.");
    expect((screen.getByRole("checkbox", { name: "Create paused" }) as HTMLInputElement).checked).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Add schedule" }));
    await screen.findByText("Schedule created.");
    expect(writes("POST").map(([, , init]) => JSON.parse(init!.body as string))).toEqual([
      { action: "start", enabled: false, time: "12:00", days: [0, 1, 2, 3, 4, 5, 6], timezone: "UTC" },
      { action: "start", enabled: false, time: "12:00", days: [0, 1, 2, 3, 4, 5, 6], timezone: "UTC" },
    ]);
    expect(view.schedule.enabled).toBe(false);
    expect(view.schedule.lastOperation).toBeNull();
    expect(view.schedule.nextRunAt).toBeNull();
    expect(writes("PATCH")).toHaveLength(0);
    await userEvent.click(screen.getByRole("button", { name: "Edit", exact: true }));
    expect(screen.queryByRole("checkbox", { name: "Create paused" })).toBeNull();
    expect(preview().textContent).toContain("Next run when resumed:");
  });

  it.each([
    ["queued", "Queued"], ["running", "Running"], ["succeeded", "Succeeded"],
    ["failed", "Failed"], ["interrupted", "Interrupted"],
  ] as const)("shows the actual %s operation result and attempt time instead of a stale queue message", async (status, label) => {
    const operation = operationFixture(server.id, { kind: "restart", status });
    detail({ initial: scheduleFixture(server.id, { lastOperation: operation }) });
    await openSchedules();
    const row = within(screen.getByRole("table", { name: "Schedules" })).getAllByRole("row")[1];
    const result = within(row).getAllByRole("cell")[4];
    expect(within(result).getByText(label)).toBeTruthy();
    expect(result.textContent).toContain("Sep 11, 2026");
    expect(result.textContent).toContain("UTC");
    expect(result.textContent).not.toContain("Queued operation");
    expect(within(result).getByRole("button", { name: "View activity" })).toBeTruthy();
  });

  it("shows the latest skipped attempt without an unrelated prior operation link", async () => {
    detail({ initial: scheduleFixture(server.id, { lastOperation: null, lastResult: "Skipped: another operation is active" }) });
    await openSchedules();
    expect(screen.getByText("Skipped: another operation is active")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "View activity" })).toBeNull();
  });

  it.each([
    ["owner_disabled", "enable the owner’s account"],
    ["action_access_removed", "restore the action grant"],
    ["binding_changed", "review the binding, then recreate this schedule"],
  ] as const)("explains %s with its recovery step", async (reason, guidance) => {
    detail({ initial: scheduleFixture(server.id, { nextRunAt: null, nextRunUnavailableReason: reason }) });
    await openSchedules();
    const nextRun = within(screen.getByRole("table", { name: "Schedules" })).getAllByRole("row")[1].children[3];
    expect(nextRun.textContent).toContain("Unavailable");
    expect(nextRun.textContent).toContain(guidance);
  });

  it("loads and focuses an older scheduled operation with its current status without blocking server controls", async () => {
    const embedded = operationFixture(server.id, { kind: "restart", status: "queued" });
    const fresh = { ...embedded, status: "failed" as const, error: "The game server could not be stopped.", phase: "finished" };
    detail({ initial: scheduleFixture(server.id, { lastOperation: embedded }), onRequest: (path) =>
      path === `/operations/${embedded.id}` ? { operation: fresh } : undefined });
    await openSchedules();
    draftTimezone("Europe/London");
    await userEvent.click(screen.getByRole("button", { name: "View activity" }));
    const region = await screen.findByRole("region", { name: "Operation details" });
    await within(region).findByText("Failed");
    expect(within(region).getByText(fresh.error)).toBeTruthy();
    expect(document.activeElement).toBe(within(region).getByRole("heading", { name: "Operation details" }));
    expect(screen.getByText("No operations yet.")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Stop", exact: true }) as HTMLButtonElement).disabled).toBe(false);
    await userEvent.click(screen.getByRole("button", { name: "Close operation" }));
    expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Recent operations" }));
    await openSchedules();
    expect((screen.getByLabelText("Time zone") as HTMLInputElement).value).toBe("Europe/London");
  });

  it("refreshes a running historical result without treating it as a current server operation", async () => {
    const intervals = spyOn(window, "setInterval");
    let operation: Operation = operationFixture(server.id, { kind: "restart", status: "running" });
    detail({ initial: scheduleFixture(server.id, { lastOperation: operation }), onRequest: (path) =>
      path === `/operations/${operation.id}` ? { operation } : undefined });
    await openSchedules();
    await userEvent.click(screen.getByRole("button", { name: "View activity" }));
    const region = await screen.findByRole("region", { name: "Operation details" });
    await within(region).findByText("Running");
    expect((screen.getByRole("button", { name: "Stop", exact: true }) as HTMLButtonElement).disabled).toBe(false);
    operation = { ...operation, status: "succeeded", phase: "finished" };
    const poll = intervals.mock.calls.find(([, delay]) => delay === 2000)![0] as () => void;
    await act(async () => poll());
    await within(region).findByText("Succeeded");
  });

  it("hides a previously visible operation after access is denied and supports a fresh retry", async () => {
    const intervals = spyOn(window, "setInterval");
    const operation = operationFixture(server.id, { kind: "restart", status: "succeeded" });
    let denied = false;
    detail({ initial: scheduleFixture(server.id, { lastOperation: operation }), onRequest: (path) => {
      if (path === `/operations/${operation.id}`) {
        if (denied) throw new ApiRequestError("Forbidden", 403);
        return { operation };
      }
    } });
    await openSchedules();
    await userEvent.click(screen.getByRole("button", { name: "View activity" }));
    const region = await screen.findByRole("region", { name: "Operation details" });
    await within(region).findByText("Succeeded");
    denied = true;
    const poll = intervals.mock.calls.filter(([, delay]) => delay === 10000).at(-1)![0] as () => void;
    await act(async () => poll());
    await within(region).findByRole("alert");
    expect(within(region).queryByText("Succeeded")).toBeNull();
    denied = false;
    await userEvent.click(within(region).getByRole("button", { name: "Retry operation" }));
    await within(region).findByText("Succeeded");
  });

  it("ignores an obsolete operation response when another view is opened", async () => {
    const operation = operationFixture(server.id, { kind: "restart", status: "failed", error: "Old operation failed" });
    let finish!: () => void;
    const pending = new Promise((resolve) => { finish = () => resolve({ operation }); });
    detail({ onRequest: (path) => path === `/operations/${operation.id}` ? pending : undefined });
    await openSchedules();
    await userEvent.click(screen.getByRole("button", { name: "View activity" }));
    await screen.findByRole("region", { name: "Operation details" });
    await openSchedules();
    draftTimezone("Europe/London");
    await act(async () => finish());
    expect(screen.queryByRole("region", { name: "Operation details" })).toBeNull();
    expect(screen.queryByText("Old operation failed")).toBeNull();
    expect((screen.getByLabelText("Time zone") as HTMLInputElement).value).toBe("Europe/London");
  });
});
