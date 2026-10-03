import { describe, expect, it, mock } from "bun:test";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SERVER_CAPABILITIES, nextScheduleRun, type Schedule } from "@ludock/shared";
import { ApiRequestError, apiJson } from "../src/api";
import ServerDetail from "../src/pages/ServerDetail";
import { NavigationProvider } from "../src/navigation";
import TestProviders from "./TestProviders";
import { scheduleFixture, serverDetailResponse, serverFixture } from "./fixtures";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({ ...originalApi, apiJson: apiJsonMock }));
const server = serverFixture({ permissions: [...SERVER_CAPABILITIES] });
const base = `/servers/${server.id}`;
const itemPath = `${base}/schedules/77777777-7777-4777-8777-777777777777`;

function detail({
  onRequest,
}: {
  onRequest?: (path: string, init?: RequestInit) => unknown | Promise<unknown>;
} = {}) {
  window.history.replaceState({}, "", base);
  let schedule = scheduleFixture(server.id);
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
    const custom = await onRequest?.(path, init);
    if (custom !== undefined) return custom;
    if (path === itemPath && init?.method === "PUT") {
      const input = JSON.parse(init.body as string);
      schedule = { ...schedule, ...input, revision: schedule.revision + 1 };
      schedule.nextRunAt = nextScheduleRun(schedule, Date.now(), schedule.lastSlot);
      return { schedule };
    }
    return serverDetailResponse(path, server, {
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

describe("schedules", () => {
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
});
