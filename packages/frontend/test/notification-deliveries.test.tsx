import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, mock, spyOn } from "bun:test";
import type { NotificationDelivery } from "@ludock/shared";
import { apiJson } from "../src/api";
import NotificationDeliveries from "../src/components/NotificationDeliveries";
import Settings from "../src/pages/Settings";
import { NavigationProvider } from "../src/navigation";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({ ...originalApi, apiJson: apiJsonMock }));

const CREATED_AT = Date.UTC(2026, 8, 14, 12);
const FAILED_ID = "11111111-1111-4111-8111-111111111111";
const TEST_ID = "22222222-2222-4222-8222-222222222222";
const failed: NotificationDelivery = {
  id: FAILED_ID,
  kind: "event",
  state: "failed",
  attempts: 5,
  createdAt: CREATED_AT,
  lastAttemptAt: CREATED_AT + 10_000,
  deliveredAt: null,
  nextAttemptAt: null,
  lastFailure: "The saved Discord webhook no longer exists. Replace it and retry.",
  retryable: true,
};
const queued: NotificationDelivery = {
  ...failed, state: "queued", retryable: false, nextAttemptAt: CREATED_AT + 20_000,
};
const testDelivery: NotificationDelivery = {
  ...queued, id: TEST_ID, kind: "test", createdAt: CREATED_AT + 20_000,
  attempts: 0, lastAttemptAt: null, lastFailure: null,
};

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((complete) => { resolve = complete; });
  return { promise, resolve };
}

function renderDeliveries(overrides: Partial<Parameters<typeof NotificationDeliveries>[0]> = {}) {
  return render(<NotificationDeliveries configured enabled unsavedChanges={false} saving={false} {...overrides} />);
}

const table = () => within(screen.getByRole("table", { name: "Recent notification deliveries" }));

describe("notification delivery troubleshooting", () => {
  it("shows delivery outcomes, total attempts, retry time, and safe failure guidance", async () => {
    apiJsonMock.mockResolvedValue({ deliveries: [
      failed,
      { ...testDelivery, state: "delivered", attempts: 1, deliveredAt: CREATED_AT + 30_000, nextAttemptAt: null },
      { ...queued, id: "33333333-3333-4333-8333-333333333333", retryable: true },
    ] });
    renderDeliveries();
    await screen.findByText("Failed", { exact: true });
    expect(table().getByText("Delivered", { exact: true, selector: "strong" })).toBeTruthy();
    expect(table().getByText("Pending retry", { exact: true })).toBeTruthy();
    expect(table().getAllByText("5 attempts")).toHaveLength(2);
    expect(table().getByText("1 attempt")).toBeTruthy();
    expect(table().getByText(/^Next retry/)).toBeTruthy();
    expect(table().getAllByText(failed.lastFailure!)).toHaveLength(2);
    expect(table().getAllByRole("button", { name: /^Retry/ })).toHaveLength(2);
  });

  it("queues one test without claiming delivery or submitting settings", async () => {
    const pending = deferred<unknown>();
    apiJsonMock.mockImplementation(async (path) => path === "/notifications/test" ? pending.promise : { deliveries: [] });
    renderDeliveries();
    await screen.findByText(/No notifications have been queued/);
    const send = screen.getByRole("button", { name: "Send test notification" });
    fireEvent.click(send);
    fireEvent.click(send);
    expect(apiJsonMock.mock.calls.filter(([, , init]) => init?.method === "POST")).toHaveLength(1);
    await act(async () => pending.resolve({ delivery: testDelivery }));
    expect(table().getByText("Queued", { exact: true, selector: "strong" })).toBeTruthy();
    expect(table().queryByText("Delivered", { exact: true, selector: "strong" })).toBeNull();
    expect(screen.getByRole("status").textContent).toBe("Test notification queued. Delivery status will update below.");
    expect(apiJsonMock).toHaveBeenLastCalledWith("/notifications/test", expect.anything(), expect.objectContaining({ method: "POST" }));
    expect(apiJsonMock.mock.calls.some(([, , init]) => init?.body !== undefined)).toBe(false);
  });

  it("ignores an older history response after an explicit retry", async () => {
    const staleRead = deferred<unknown>();
    let reads = 0;
    let readSignal: AbortSignal | undefined;
    apiJsonMock.mockImplementation(async (path, _schema, init) => {
      if (path.endsWith("/retry")) return { delivery: queued };
      if (++reads === 1) return { deliveries: [failed] };
      readSignal = init?.signal as AbortSignal;
      return staleRead.promise;
    });
    renderDeliveries();
    await screen.findByText("Failed", { exact: true });
    await userEvent.click(screen.getByRole("button", { name: "Refresh deliveries" }));
    await userEvent.click(screen.getByRole("button", { name: /^Retry notification/ }));
    await screen.findByText("Pending retry", { exact: true });
    expect(readSignal?.aborted).toBe(true);
    await act(async () => staleRead.resolve({ deliveries: [failed] }));
    expect(table().queryByText("Failed", { exact: true })).toBeNull();
    expect(table().getByText("Pending retry", { exact: true })).toBeTruthy();
    expect(table().queryByRole("button", { name: /^Retry notification/ })).toBeNull();
  });

  it("refreshes queued deliveries automatically and stops polling once delivered", async () => {
    let poll: (() => void) | undefined;
    const interval = spyOn(window, "setInterval").mockImplementation((handler) => {
      poll = handler as () => void;
      return 123;
    });
    const clearInterval = spyOn(window, "clearInterval");
    let current = testDelivery;
    apiJsonMock.mockImplementation(async () => ({ deliveries: [current] }));
    renderDeliveries();
    await screen.findByText("Queued", { exact: true, selector: "strong" });
    expect(interval).toHaveBeenCalledWith(expect.any(Function), 5_000);
    current = { ...testDelivery, state: "delivered", attempts: 1, deliveredAt: CREATED_AT + 30_000, nextAttemptAt: null };
    await act(async () => { poll?.(); });
    expect(table().getByText("Delivered", { exact: true, selector: "strong" })).toBeTruthy();
    expect(clearInterval).toHaveBeenCalledWith(123);
    expect(apiJsonMock).toHaveBeenCalledTimes(2);
  });

  it("retains loaded history after a refresh failure and can recover", async () => {
    let unavailable = false;
    apiJsonMock.mockImplementation(async () => {
      if (unavailable) throw new Error("Delivery history unavailable.");
      return { deliveries: [failed] };
    });
    renderDeliveries();
    await screen.findByText("Failed", { exact: true });
    unavailable = true;
    await userEvent.click(screen.getByRole("button", { name: "Refresh deliveries" }));
    expect((await screen.findByRole("alert")).textContent).toContain("Showing the last loaded deliveries.");
    expect(table().getByText("Failed", { exact: true })).toBeTruthy();
    unavailable = false;
    await userEvent.click(screen.getByRole("button", { name: "Refresh deliveries" }));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  });

  it("lets a slow history poll finish before starting another one", async () => {
    let poll: (() => void) | undefined;
    spyOn(window, "setInterval").mockImplementation((handler) => {
      poll = handler as () => void;
      return 123;
    });
    const pending = deferred<unknown>();
    let readSignal: AbortSignal | undefined;
    let reads = 0;
    apiJsonMock.mockImplementation(async (_path, _schema, init) => {
      if (++reads === 1) return { deliveries: [testDelivery] };
      readSignal = init?.signal as AbortSignal;
      return pending.promise;
    });
    renderDeliveries();
    await screen.findByText("Queued", { exact: true, selector: "strong" });
    await act(async () => { poll?.(); });
    const firstPoll = readSignal!;
    await act(async () => { poll?.(); });
    expect(reads).toBe(2);
    expect(firstPoll.aborted).toBe(false);
    await act(async () => pending.resolve({ deliveries: [{
      ...testDelivery, state: "delivered", attempts: 1,
      deliveredAt: CREATED_AT + 30_000, nextAttemptAt: null,
    }] }));
    expect(table().getByText("Delivered", { exact: true, selector: "strong" })).toBeTruthy();
  });

  it("pauses queued delivery polling while disabled and refreshes retry availability after enabling", async () => {
    const interval = spyOn(window, "setInterval");
    let enabled = false;
    apiJsonMock.mockImplementation(async () => ({ deliveries: [{ ...failed, retryable: enabled }, testDelivery] }));
    const view = renderDeliveries({ enabled });
    await screen.findByText("Paused", { exact: true });
    expect(screen.getByText(/Queued notifications resume when delivery is enabled/)).toBeTruthy();
    expect(table().queryByText(/^Next attempt/)).toBeNull();
    expect(table().queryByRole("button", { name: /^Retry notification/ })).toBeNull();
    expect(interval).not.toHaveBeenCalled();
    enabled = true;
    view.rerender(<NotificationDeliveries configured enabled unsavedChanges={false} saving={false} />);
    expect(await screen.findByRole("button", { name: /^Retry notification/ })).toBeTruthy();
    expect(table().getByText("Queued", { exact: true, selector: "strong" })).toBeTruthy();
    expect(apiJsonMock).toHaveBeenCalledTimes(2);
    expect(interval).toHaveBeenCalledWith(expect.any(Function), 5_000);
  });

  it("does not replace a failed retry with optimistic success and allows another attempt", async () => {
    let rejectRetry = true;
    apiJsonMock.mockImplementation(async (path) => {
      if (!path.endsWith("/retry")) return { deliveries: [failed] };
      if (rejectRetry) throw new Error("Notification delivery is disabled.");
      return { delivery: queued };
    });
    renderDeliveries();
    const retry = await screen.findByRole("button", { name: /^Retry notification/ });
    await userEvent.click(retry);
    expect((await screen.findByRole("alert")).textContent).toBe("Notification delivery is disabled.");
    expect(table().getByText("Failed", { exact: true })).toBeTruthy();
    expect(screen.queryByText(/Notification queued for retry/)).toBeNull();
    rejectRetry = false;
    await userEvent.click(retry);
    await screen.findByText("Pending retry", { exact: true });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps settings editable when delivery history cannot load", async () => {
    apiJsonMock.mockImplementation(async (path) => {
      if (path === "/settings/deployment") return { backupRoots: [], composeRoots: [], composeAvailable: false };
      if (path === "/settings/backups") return { settings: null };
      if (path === "/notifications/deliveries") throw new Error("Delivery history unavailable.");
      return { configured: true, enabled: true };
    });
    render(<NavigationProvider><Settings /></NavigationProvider>);
    await screen.findByText("Delivery history unavailable.");
    const webhook = screen.getByLabelText("Replace webhook URL") as HTMLInputElement;
    await userEvent.type(webhook, "https://discord.com/api/webhooks/disposable-fixture");
    expect((screen.getByRole("button", { name: "Send test notification" }) as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Refresh deliveries" }));
    expect(webhook.value).toBe("https://discord.com/api/webhooks/disposable-fixture");
    expect((screen.getByRole("button", { name: "Save notifications" }) as HTMLButtonElement).disabled).toBe(false);
    expect(apiJsonMock.mock.calls.filter(([, , init]) => init?.method)).toHaveLength(0);
  });

  it.each([
    { configured: false }, { enabled: false }, { unsavedChanges: true }, { saving: true },
  ])("requires saved, enabled configuration before a test or retry: %j", async (props) => {
    apiJsonMock.mockResolvedValue({ deliveries: [failed] });
    renderDeliveries(props);
    const retry = await screen.findByRole("button", { name: /^Retry notification/ });
    const send = screen.getByRole("button", { name: "Send test notification" });
    expect((retry as HTMLButtonElement).disabled).toBe(true);
    expect((send as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(send);
    fireEvent.click(retry);
    expect(apiJsonMock.mock.calls.filter(([, , init]) => init?.method === "POST")).toHaveLength(0);
  });
});
