import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, mock } from "bun:test";
import { apiJson } from "../src/api";
import Settings from "../src/pages/Settings";
import { NavigationProvider } from "../src/navigation";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({ ...originalApi, apiJson: apiJsonMock }));

const gib = 1024 ** 3;
const settings = { destination: "/backups", retentionCount: 10, maxBytes: 100 * gib, reserveBytes: 5 * gib };
const storage = {
  configured: true,
  archiveBytes: 2.5 * gib,
  maxBytes: settings.maxBytes,
  reserveBytes: settings.reserveBytes,
  availableBytes: 24 * gib,
  issues: [],
};

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((complete) => { resolve = complete; });
  return { promise, resolve };
}

function metric(label: string) {
  return screen.getByText(label, { selector: "dt" }).nextElementSibling?.textContent;
}

describe("backup storage", () => {
  it("refreshes saved storage after saving while preserving newer settings drafts", async () => {
    const saved = deferred<unknown>();
    let savedMaxBytes = settings.maxBytes;
    let statusReads = 0;
    apiJsonMock.mockImplementation(async (path, _schema, init) => {
      if (path === "/notifications/deliveries") return { deliveries: [] };
      if (path === "/settings/backups/status") {
        statusReads++;
        return { storage: { ...storage, maxBytes: savedMaxBytes } };
      }
      if (path === "/settings/backups") return init?.method === "PUT" ? saved.promise : { settings };
      if (path === "/settings/deployment") return { backupRoots: ["/backups"], composeRoots: [], composeAvailable: false };
      return { configured: false, enabled: false };
    });
    render(<NavigationProvider><Settings /></NavigationProvider>);
    await screen.findByText("100 GiB");
    const limit = screen.getByLabelText("Total backup limit (GiB)") as HTMLInputElement;
    fireEvent.change(limit, { target: { value: "75" } });
    fireEvent.click(screen.getByRole("button", { name: "Refresh storage" }));
    await waitFor(() => expect(statusReads).toBe(2));
    expect(limit.value).toBe("75");
    expect(metric("Configured limit")).toBe("100 GiB");
    fireEvent.click(screen.getByRole("button", { name: "Save backup settings" }));
    fireEvent.change(limit, { target: { value: "50" } });
    savedMaxBytes = 75 * gib;
    await act(async () => saved.resolve({ settings: { ...settings, maxBytes: savedMaxBytes } }));
    await within(screen.getByRole("region", { name: "Current storage" })).findByText("75 GiB");
    expect(statusReads).toBe(3);
    expect(limit.value).toBe("50");
  });
});
