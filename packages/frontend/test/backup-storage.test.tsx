import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, mock } from "bun:test";
import { apiJson } from "../src/api";
import BackupStorageSummary from "../src/components/BackupStorageSummary";
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

describe("backup storage visibility", () => {
  it("shows measured archive usage, saved limits, free disk space and reserve together", async () => {
    apiJsonMock.mockImplementation(async () => ({ storage }));
    render(<BackupStorageSummary />);
    await screen.findByText("2.5 GiB");
    expect(metric("Archive usage")).toBe("2.5 GiB");
    expect(metric("Configured limit")).toBe("100 GiB");
    expect(metric("Available disk space")).toBe("24 GiB");
    expect(metric("Free-space reserve")).toBe("5 GiB");
  });

  it("clears an old snapshot when refreshing fails and can recover without showing zero usage", async () => {
    let unavailable = false;
    apiJsonMock.mockImplementation(async () => {
      if (unavailable) throw new Error("Storage check failed");
      return { storage };
    });
    render(<BackupStorageSummary />);
    await screen.findByText("24 GiB");
    unavailable = true;
    fireEvent.click(screen.getByRole("button", { name: "Refresh storage" }));
    await screen.findByText("Backup storage is unavailable: Storage check failed");
    expect(screen.queryByText("24 GiB")).toBeNull();
    expect(screen.queryByText("0 B")).toBeNull();
    unavailable = false;
    fireEvent.click(screen.getByRole("button", { name: "Refresh storage" }));
    await screen.findByText("24 GiB");
  });

  it("shows unavailable measurements and actionable storage issues", async () => {
    apiJsonMock.mockImplementation(async () => ({ storage: {
      ...storage,
      configured: false,
      maxBytes: null,
      reserveBytes: null,
      availableBytes: null,
      issues: [{ code: "not_configured", message: "Save a backup destination in Settings." }],
    } }));
    render(<BackupStorageSummary />);
    await screen.findByText("Save a backup destination in Settings.");
    expect(metric("Available disk space")).toBe("Unavailable");
    expect(metric("Configured limit")).toBe("Not configured");
    expect(metric("Free-space reserve")).toBe("Not configured");
    expect(metric("Archive usage")).toBe("2.5 GiB");
  });

  it("aborts superseded requests and ignores late storage responses", async () => {
    const first = deferred<unknown>();
    let firstSignal: AbortSignal | null | undefined;
    apiJsonMock.mockImplementation(async (_path, _schema, init) => {
      if (!firstSignal) {
        firstSignal = init?.signal;
        return first.promise;
      }
      return { storage: { ...storage, availableBytes: 12 * gib } };
    });
    const view = render(<BackupStorageSummary key={0} />);
    view.rerender(<BackupStorageSummary key={1} />);
    await screen.findByText("12 GiB");
    expect(firstSignal?.aborted).toBe(true);
    await act(async () => first.resolve({ storage }));
    expect(metric("Available disk space")).toBe("12 GiB");
  });

  it("refreshes saved storage after saving while preserving newer settings drafts", async () => {
    const saved = deferred<unknown>();
    let savedMaxBytes = settings.maxBytes;
    let statusReads = 0;
    apiJsonMock.mockImplementation(async (path, _schema, init) => {
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
