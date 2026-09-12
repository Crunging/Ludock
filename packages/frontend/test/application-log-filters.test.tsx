import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, spyOn } from "bun:test";
import type { ApplicationLogEntry } from "@ludock/shared";
import ApplicationLogs from "../src/pages/ApplicationLogs";

const generation = "31a2fb01-b8d7-4bc4-b741-cfefc4e72cc1";
const nextGeneration = "31a2fb01-b8d7-4bc4-b741-cfefc4e72cc2";
const entry = (id: number, overrides: Partial<ApplicationLogEntry> = {}): ApplicationLogEntry => ({
  id, timestamp: 0, level: "info", component: "app", message: `Log ${id}`, ...overrides,
});
const response = (entries: ApplicationLogEntry[], process = generation) => Response.json({ generation: process, entries });
const output = () => screen.getByRole("log", { name: "Ludock application logs" });
const search = (value: string) => fireEvent.change(screen.getByRole("searchbox", { name: "Search logs" }), { target: { value } });
const select = (name: string, value: string) => fireEvent.change(screen.getByRole("combobox", { name }), { target: { value } });
const refresh = () => fireEvent.click(screen.getByRole("button", { name: "Refresh" }));

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((yes) => { resolve = yes; });
  return { promise, resolve };
}

describe("application log browsing", () => {
  it("combines case-insensitive text and redacted context search with exact severity and component filters", async () => {
    const request = spyOn(globalThis, "fetch").mockResolvedValue(response([
      entry(1, { component: "docker", level: "warn", message: "Retrying connection", context: { token: "[REDACTED]", host: "Test-Daemon" } }),
      entry(2, { component: "docker", level: "error", message: "Test-Daemon unreachable" }),
      entry(3, { component: "docker-events", level: "warn", message: "Test-Daemon stream closed" }),
      entry(4, { message: "Ready" }),
    ]));
    render(<ApplicationLogs />);
    await screen.findByText("Ready");
    expect(screen.getByText("4 of 4 recent entries")).toBeTruthy();
    expect(screen.getByText(/recent entries buffered in this page/)).toBeTruthy();
    search("  TEST-daemon  ");
    expect(screen.getByText("3 of 4 recent entries")).toBeTruthy();
    select("Severity", "warn");
    expect(screen.getByText("2 of 4 recent entries")).toBeTruthy();
    select("Component", "docker");
    expect(screen.getByText("1 of 4 recent entries")).toBeTruthy();
    expect(output().textContent).toContain("Retrying connection");
    expect(output().textContent).not.toContain("stream closed");
    search("[redacted]");
    expect(screen.getByText("1 of 4 recent entries")).toBeTruthy();
    search("no matching context");
    expect(screen.getByText("0 of 4 recent entries")).toBeTruthy();
    expect(within(output()).getByText("No recent log entries match these filters.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(screen.getByText("4 of 4 recent entries")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Clear filters" }) as HTMLButtonElement).disabled).toBe(true);
    expect(request).toHaveBeenCalledTimes(1);
  });

  it("keeps the selected component through buffer eviction and a process restart", async () => {
    const request = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(response([entry(1, { component: "startup", message: "Original startup" })]))
      .mockResolvedValueOnce(response(Array.from({ length: 1000 }, (_, index) => entry(index + 2))))
      .mockResolvedValueOnce(response([entry(1, { component: "startup", message: "New process startup" })], nextGeneration));
    render(<ApplicationLogs />);
    await screen.findByText("Original startup");
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    select("Component", "startup");
    refresh();
    await screen.findByText("0 of 1000 recent entries");
    expect(output().textContent).not.toContain("Original startup");
    expect((screen.getByRole("combobox", { name: "Component" }) as HTMLSelectElement).value).toBe("startup");
    expect(screen.getByRole("option", { name: "startup" })).toBeTruthy();
    refresh();
    await screen.findByText("New process startup");
    expect(screen.getByText("1 of 1 recent entries")).toBeTruthy();
    expect((screen.getByRole("combobox", { name: "Component" }) as HTMLSelectElement).value).toBe("startup");
    expect(screen.queryByRole("option", { name: "app", exact: true })).toBeNull();
    expect(String(request.mock.calls[1][0])).toContain(`after=1&generation=${generation}`);
    expect(String(request.mock.calls[2][0])).toContain(`after=1001&generation=${generation}`);
  });

  it("continues polling without scrolling when Follow latest is off and follows again while fetching is paused", async () => {
    const intervals = spyOn(window, "setInterval");
    const clearInterval = spyOn(window, "clearInterval");
    const request = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(response([entry(1)]))
      .mockResolvedValueOnce(response([entry(2)]));
    render(<ApplicationLogs />);
    await screen.findByText("Log 1");
    const log = output();
    Object.defineProperty(log, "scrollHeight", { configurable: true, get: () => 1000 });
    const follow = screen.getByRole("checkbox", { name: "Follow latest" });
    fireEvent.click(follow);
    log.scrollTop = 40;
    const poll = intervals.mock.calls.find(([, delay]) => delay === 2000)![0] as () => void;
    await act(async () => poll());
    await screen.findByText("Log 2");
    expect(log.scrollTop).toBe(40);
    expect(log.getAttribute("aria-live")).toBe("off");
    expect(request).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    expect(clearInterval).toHaveBeenCalled();
    expect(screen.getByText(/Fetching paused/)).toBeTruthy();
    fireEvent.click(follow);
    expect(log.scrollTop).toBe(1000);
    expect(log.getAttribute("aria-live")).toBe("off");
    expect(request).toHaveBeenCalledTimes(2);
  });

  it("applies the latest filter draft when an earlier read finishes and clears all cached component names on denial", async () => {
    const pending = deferred<Response>();
    const request = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(response([entry(1, { component: "private-worker", message: "Earlier task" })]))
      .mockReturnValueOnce(pending.promise)
      .mockResolvedValueOnce(Response.json({ error: "Log access denied" }, { status: 403 }))
      .mockResolvedValueOnce(response([entry(10, { message: "Newer task" })]));
    render(<ApplicationLogs />);
    await screen.findByText("Earlier task");
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    refresh();
    search("newer");
    await act(async () => pending.resolve(response([
      entry(2, { component: "private-worker", message: "Newer task" }),
      entry(3, { message: "Unrelated task" }),
    ])));
    expect(screen.getByText("1 of 3 recent entries")).toBeTruthy();
    expect(output().textContent).toContain("Newer task");
    expect(output().textContent).not.toContain("Earlier task");
    select("Component", "private-worker");
    refresh();
    await screen.findByRole("alert");
    expect(screen.getByText("0 of 0 recent entries")).toBeTruthy();
    expect(output().textContent).not.toContain("Newer task");
    expect(screen.queryByRole("option", { name: "private-worker" })).toBeNull();
    expect((screen.getByRole("searchbox", { name: "Search logs" }) as HTMLInputElement).value).toBe("newer");
    refresh();
    await screen.findByText("Newer task");
    expect(request.mock.calls[3][0]).toBe("/api/v1/application-logs?limit=250&after=0");
  });

  it("ignores an aborted response after pause and resume without replacing the active filters", async () => {
    const pending = deferred<Response>();
    const request = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(response([entry(1, { component: "worker", message: "Task start" })]))
      .mockReturnValueOnce(pending.promise)
      .mockResolvedValueOnce(response([entry(2, { component: "worker", message: "Task complete" })]));
    render(<ApplicationLogs />);
    await screen.findByText("Task start");
    select("Component", "worker");
    refresh();
    const signal = request.mock.calls[1][1]?.signal;
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    expect(signal?.aborted).toBe(true);
    search("complete");
    fireEvent.click(screen.getByRole("button", { name: "Resume" }));
    await screen.findByText("Task complete");
    await act(async () => pending.resolve(response([entry(3, { component: "stale", message: "Stale complete" })], nextGeneration)));
    expect(output().textContent).not.toContain("Stale complete");
    expect(screen.getByText("1 of 2 recent entries")).toBeTruthy();
    expect((screen.getByRole("combobox", { name: "Component" }) as HTMLSelectElement).value).toBe("worker");
  });
});
