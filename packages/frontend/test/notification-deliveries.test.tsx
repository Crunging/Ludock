import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, mock, spyOn } from "bun:test";
import type { NotificationDelivery } from "@ludock/shared";
import { apiJson } from "../src/api";
import NotificationDeliveries from "../src/components/NotificationDeliveries";

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

function renderDeliveries() {
  return render(<NotificationDeliveries configured enabled unsavedChanges={false} saving={false} />);
}

const table = () => within(screen.getByRole("table", { name: "Recent notification deliveries" }));

describe("notification deliveries", () => {
  it("prevents duplicate notification requests while submission is pending", async () => {
    const pending = deferred<unknown>();
    apiJsonMock.mockImplementation(async (path) => path === "/notifications/test" ? pending.promise : { deliveries: [] });
    renderDeliveries();
    await screen.findByText(/No notifications have been queued/);
    const send = screen.getByRole("button", { name: "Send test notification" });
    fireEvent.click(send);
    fireEvent.click(send);
    expect(apiJsonMock.mock.calls.filter(([, , init]) => init?.method === "POST")).toHaveLength(1);
    await act(async () => pending.resolve({ delivery: testDelivery }));
    expect(apiJsonMock.mock.calls.filter(([, , init]) => init?.method === "POST")).toHaveLength(1);
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
    await waitFor(() => expect(poll).toBeDefined());
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
});
