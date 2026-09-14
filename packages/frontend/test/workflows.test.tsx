import { describe, expect, it, mock, spyOn } from "bun:test";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  SERVER_CAPABILITIES,
  type Backup,
  type Operation,
  type Server,
} from "@ludock/shared";
import type { AuthUser } from "../src/auth-context";
import TestProviders from "./TestProviders";
import { operationFixture, serverDetailResponse, serverFixture, type ServerDetailData } from "./fixtures";
import ServerDetail from "../src/pages/ServerDetail";
import ServerGrants from "../src/components/ServerGrants";
import { apiJson } from "../src/api";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({
  ...originalApi,
  apiJson: apiJsonMock,
}));
const server = serverFixture({
  permissions: ["server.view", "server.start", "server.stop"],
});
const completedUpdate = operationFixture(server.id);
const backup: Backup = {
  id: "60c15d4e-b46a-4e57-b52d-fc08ee134b75",
  serverId: server.id,
  createdAt: 0,
  roots: [{ id: "root-0", path: "/data" }],
  size: 4096,
  checksum: "a".repeat(64),
  state: "complete",
};
interface DetailOptions extends ServerDetailData {
  server?: Partial<Server>;
  onRequest?: (path: string, init?: RequestInit) => unknown | Promise<unknown>;
}
function detail(role: AuthUser["role"] = "admin", options: DetailOptions = {}) {
  const currentServer = {
    ...server,
    ...(role === "admin" ? { permissions: [...SERVER_CAPABILITIES] } : {}),
    ...options.server,
  };
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
    const response = await options.onRequest?.(path, init);
    if (response !== undefined) return response;
    if (path.endsWith("/availability") && init?.method === "PUT")
      return { ...serverDetailResponse(path, currentServer), policy: JSON.parse(init.body as string) };
    if (init?.method && init.method !== "GET")
      return { operation: { ...completedUpdate, status: "queued" }, ok: true };
    return serverDetailResponse(path, currentServer, {
      ...options,
      operations: options.operations ?? [completedUpdate],
    });
  });
  return render(
    <TestProviders
      user={{ id: "user1", role, username: "admin" }}
      pathname={`/servers/${server.id}`}
      navigate={mock()}
    >
      <ServerDetail serverId={server.id} />
    </TestProviders>,
  );
}

describe("update confirmation", () => {
  it("same-image recreation requires fresh confirmation and preserves the backup choice", async () => {
    detail();
    await userEvent.click(
      await screen.findByRole("button", {
        name: "Recreate anyway",
        exact: true,
      }),
    );
    const recreate = screen.getByRole("button", {
      name: "Recreate service",
    }) as HTMLButtonElement;
    expect(recreate.disabled).toBe(true);
    await userEvent.click(
      screen.getByRole("checkbox", { name: /I understand/ }),
    );
    await userEvent.click(recreate);
    await waitFor(() =>
      expect(apiJson).toHaveBeenCalledWith(
        `/servers/${server.id}/updates`,
        expect.anything(),
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({ createBackup: true, forceRecreate: true }),
        }),
      ),
    );
  });
  it("skipping backup requires the exact server name in addition to consent", async () => {
    detail();
    await userEvent.click(
      await screen.findByRole("tab", { name: "Update", exact: true }),
    );
    await userEvent.click(
      screen.getByRole("checkbox", { name: /Create a stopped-server backup/ }),
    );
    await userEvent.click(
      screen.getByRole("checkbox", { name: /I understand/ }),
    );
    const update = screen.getByRole("button", {
      name: "Update server",
      exact: true,
    }) as HTMLButtonElement;
    expect(update.disabled).toBe(true);
    await userEvent.type(
      screen.getByRole("textbox", { name: /to skip the backup/ }),
      server.displayName,
    );
    expect(update.disabled).toBe(false);
    await userEvent.click(update);
    await waitFor(() =>
      expect(apiJson).toHaveBeenCalledWith(
        `/servers/${server.id}/updates`,
        expect.anything(),
        expect.objectContaining({
          body: JSON.stringify({
            createBackup: false,
            forceRecreate: false,
            skipBackupConfirmation: server.displayName,
          }),
        }),
      ),
    );
  });
  it("lifecycle-only friends receive no privileged management tabs", async () => {
    detail("operator");
    await screen.findByRole("heading", { name: server.displayName });
    for (const name of ["Update", "Backups", "Schedules"])
      expect(screen.queryByRole("tab", { name, exact: true })).toBeNull();
  });
});

describe("server management panels", () => {
  it("preserves update choices across tabs and resets consent when recreating from Activity", async () => {
    detail();
    await userEvent.click(
      await screen.findByRole("tab", { name: "Update", exact: true }),
    );
    await userEvent.click(
      screen.getByRole("checkbox", { name: /Create a stopped-server backup/ }),
    );
    await userEvent.type(
      screen.getByRole("textbox", { name: /to skip the backup/ }),
      server.displayName,
    );
    await userEvent.click(
      screen.getByRole("checkbox", { name: /I understand/ }),
    );
    await userEvent.click(screen.getByRole("tab", { name: "Activity" }));
    await userEvent.click(
      screen.getByRole("button", { name: "Recreate anyway", exact: true }),
    );
    expect(
      (
        screen.getByRole("checkbox", {
          name: /Create a stopped-server backup/,
        }) as HTMLInputElement
      ).checked,
    ).toBe(false);
    expect(
      (
        screen.getByRole("textbox", {
          name: /to skip the backup/,
        }) as HTMLInputElement
      ).value,
    ).toBe(server.displayName);
    expect(
      (
        screen.getByRole("checkbox", {
          name: /I understand/,
        }) as HTMLInputElement
      ).checked,
    ).toBe(false);
    expect(
      (
        screen.getByRole("button", {
          name: "Recreate service",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    const panel = screen.getByRole("tabpanel");
    expect(panel.getAttribute("aria-labelledby")).toBe(
      screen.getByRole("tab", { name: "Update", exact: true }).id,
    );
    expect(panel.id).toBe(
      screen.getByRole("tab", { name: "Update", exact: true }).getAttribute("aria-controls"),
    );
    expect(
      screen
        .getByRole("tab", { name: "Update", exact: true })
        .getAttribute("aria-selected"),
    ).toBe("true");
  });

  it("keeps new draft choices made while an update request is pending", async () => {
    let completeUpdate: (value: unknown) => void = () => {};
    const pending = new Promise((resolve) => {
      completeUpdate = resolve;
    });
    detail("admin", {
      onRequest: (path, init) =>
        path.endsWith("/updates") && init?.method === "POST"
          ? pending
          : undefined,
    });
    await userEvent.click(
      await screen.findByRole("tab", { name: "Update", exact: true }),
    );
    await userEvent.click(
      screen.getByRole("checkbox", { name: /I understand/ }),
    );
    await userEvent.click(
      screen.getByRole("button", { name: "Update server", exact: true }),
    );
    expect(
      (
        screen.getByRole("button", {
          name: "Update server",
          exact: true,
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    await userEvent.click(
      screen.getByRole("checkbox", { name: /Create a stopped-server backup/ }),
    );
    await userEvent.click(
      screen.getByRole("checkbox", { name: /Recreate anyway/ }),
    );
    await act(async () => {
      completeUpdate({ operation: { ...completedUpdate, status: "queued" } });
    });
    await waitFor(() =>
      expect(screen.getByRole("tabpanel").getAttribute("aria-labelledby")).toBe(
        screen.getByRole("tab", { name: "Activity", selected: true }).id,
      ),
    );
    await userEvent.click(
      screen.getByRole("tab", { name: "Update", exact: true }),
    );
    expect(
      (
        screen.getByRole("checkbox", {
          name: /Create a stopped-server backup/,
        }) as HTMLInputElement
      ).checked,
    ).toBe(false);
    expect(
      (
        screen.getByRole("checkbox", {
          name: /Recreate anyway/,
        }) as HTMLInputElement
      ).checked,
    ).toBe(true);
    expect(
      (
        screen.getByRole("checkbox", {
          name: /I understand/,
        }) as HTMLInputElement
      ).checked,
    ).toBe(false);
  });

  it("asks before creating a stopped backup and refreshes Activity after queuing", async () => {
    const confirm = spyOn(window, "confirm").mockReturnValue(false);
    detail();
    await userEvent.click(await screen.findByRole("tab", { name: "Backups" }));
    await userEvent.click(
      screen.getByRole("button", { name: "Create backup" }),
    );
    expect(confirm).toHaveBeenCalledWith(
      expect.stringContaining("stops for the entire copy"),
    );
    expect(
      apiJsonMock
        .mock.calls.some(
          ([path, , init]) =>
            path.endsWith("/backups") && init?.method === "POST",
        ),
    ).toBe(false);
    confirm.mockReturnValue(true);
    const initialRefreshes = apiJsonMock
      .mock.calls.filter(([path]) => path.endsWith("/operations")).length;
    await userEvent.click(
      screen.getByRole("button", { name: "Create backup" }),
    );
    await screen.findByText("Backup queued. Follow its progress in Activity.");
    await waitFor(() =>
      expect(screen.getByRole("tabpanel").getAttribute("aria-labelledby")).toBe(
        screen.getByRole("tab", { name: "Activity", selected: true }).id,
      ),
    );
    expect(apiJson).toHaveBeenCalledWith(
      `/servers/${server.id}/backups`,
      expect.anything(),
      expect.objectContaining({ method: "POST", body: "{}" }),
    );
    expect(
      apiJsonMock
        .mock.calls.filter(([path]) => path.endsWith("/operations")).length,
    ).toBeGreaterThan(initialRefreshes);
  });

  it("keeps restore selection and confirmation after an error, then clears them on success", async () => {
    let attempts = 0;
    detail("admin", {
      backups: [backup],
      onRequest: (path, init) => {
        if (path.endsWith("/restores") && init?.method === "POST") {
          if (++attempts === 1)
            throw new Error("Safety backup needs more space.");
          return {
            operation: {
              ...completedUpdate,
              kind: "restore",
              status: "queued",
            },
          };
        }
      },
    });
    await userEvent.click(await screen.findByRole("tab", { name: "Backups" }));
    expect(
      screen.getByRole("link", { name: "Download" }).getAttribute("href"),
    ).toBe(`/api/v1/servers/${server.id}/backups/${backup.id}/download`);
    await userEvent.click(screen.getByRole("button", { name: "Restore…" }));
    const submit = screen.getByRole("button", {
      name: "Restore game data",
    }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    await userEvent.type(
      screen.getByRole("textbox", { name: /to confirm/ }),
      server.displayName,
    );
    await userEvent.click(submit);
    await screen.findByRole("alert");
    expect(screen.getByRole("alert").textContent).toContain(
      "Safety backup needs more space.",
    );
    expect(
      (screen.getByRole("textbox", { name: /to confirm/ }) as HTMLInputElement)
        .value,
    ).toBe(server.displayName);
    await userEvent.click(screen.getByRole("tab", { name: "Activity" }));
    await userEvent.click(screen.getByRole("tab", { name: "Backups" }));
    expect(
      (screen.getByRole("textbox", { name: /to confirm/ }) as HTMLInputElement)
        .value,
    ).toBe(server.displayName);
    await userEvent.click(
      screen.getByRole("button", { name: "Restore game data" }),
    );
    await waitFor(() =>
      expect(screen.getByRole("tabpanel").getAttribute("aria-labelledby")).toBe(
        screen.getByRole("tab", { name: "Activity", selected: true }).id,
      ),
    );
    expect(apiJson).toHaveBeenCalledWith(
      `/servers/${server.id}/restores`,
      expect.anything(),
      expect.objectContaining({
        body: JSON.stringify({
          backupId: backup.id,
          confirmation: server.displayName,
        }),
      }),
    );
    await userEvent.click(screen.getByRole("tab", { name: "Backups" }));
    expect(
      screen.queryByRole("button", { name: "Restore game data" }),
    ).toBeNull();
  });

  it("lets a backup-only friend queue a backup without exposing archives or restoration", async () => {
    detail("operator", {
      server: { permissions: ["server.view", "backups.create"] },
      backups: [backup],
    });
    await userEvent.click(await screen.findByRole("tab", { name: "Backups" }));
    expect(screen.getByRole("button", { name: "Create backup" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Download" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Restore…" })).toBeNull();
    expect(
      apiJsonMock.mock.calls.some(([path]) => path.endsWith("/backups")),
    ).toBe(false);
  });

  it("limits schedule actions to grants and preserves a draft while navigating", async () => {
    detail("operator", {
      server: {
        permissions: ["server.view", "server.stop", "schedules.manage"],
      },
    });
    await userEvent.click(
      await screen.findByRole("tab", { name: "Schedules" }),
    );
    const action = screen.getByRole("combobox", { name: "Action" });
    expect(
      within(action)
        .getAllByRole("option")
        .map((option) => option.textContent),
    ).toEqual(["Select action", "Stop"]);
    expect(
      (
        screen.getByRole("button", {
          name: "Add schedule",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    await userEvent.selectOptions(action, "stop");
    await userEvent.clear(screen.getByLabelText("Time zone"));
    await userEvent.type(screen.getByLabelText("Time zone"), "Etc/UTC");
    for (const checkbox of within(screen.getByRole("group", { name: "Days" })).getAllByRole("checkbox"))
      await userEvent.click(checkbox);
    expect(
      (
        screen.getByRole("button", {
          name: "Add schedule",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    await userEvent.click(screen.getByRole("checkbox", { name: "Tue" }));
    await userEvent.click(screen.getByRole("tab", { name: "Activity" }));
    await userEvent.click(screen.getByRole("tab", { name: "Schedules" }));
    expect(
      (screen.getByRole("combobox", { name: "Action" }) as HTMLSelectElement)
        .value,
    ).toBe("stop");
    expect((screen.getByLabelText("Time zone") as HTMLInputElement).value).toBe(
      "Etc/UTC",
    );
    await userEvent.click(screen.getByRole("button", { name: "Add schedule" }));
    await screen.findByText("Schedule created.");
    expect(apiJson).toHaveBeenCalledWith(
      `/servers/${server.id}/schedules`,
      expect.anything(),
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          action: "stop",
          enabled: true,
          time: "08:00",
          days: [2],
          timezone: "Etc/UTC",
        }),
      }),
    );
  });

  it("loads and saves availability settings without refreshing away an unsaved draft", async () => {
    detail();
    await userEvent.click(
      await screen.findByRole("tab", { name: "Availability" }),
    );
    const grace = screen.getByRole("spinbutton", {
      name: /Failure grace period/,
    });
    expect((grace as HTMLInputElement).value).toBe("120");
    await userEvent.click(
      screen.getByRole("checkbox", { name: "Monitor this server" }),
    );
    await userEvent.click(
      screen.getByRole("checkbox", { name: /Maintenance mode/ }),
    );
    await userEvent.clear(grace);
    await userEvent.type(grace, "300");
    await userEvent.click(screen.getByRole("tab", { name: "Activity" }));
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await userEvent.click(screen.getByRole("tab", { name: "Availability" }));
    expect(
      (
        screen.getByRole("spinbutton", {
          name: /Failure grace period/,
        }) as HTMLInputElement
      ).value,
    ).toBe("300");
    await userEvent.click(
      screen.getByRole("button", { name: "Save monitoring" }),
    );
    await screen.findByText("Availability settings saved.");
    expect(apiJson).toHaveBeenCalledWith(
      `/servers/${server.id}/availability`,
      expect.anything(),
      expect.objectContaining({
        method: "PUT",
        body: JSON.stringify({
          enabled: true,
          maintenance: true,
          graceSeconds: 300,
        }),
      }),
    );
  });

  it("requires the exact server name before accepting a changed binding", async () => {
    let reviewed = false;
    detail("admin", {
      server: {
        bindingStatus: "review_required",
        permissions: ["server.view"],
      },
      onRequest: (path, init) => {
        if (path.endsWith("/binding-review") && init?.method === "POST") {
          reviewed = true;
          return { server };
        }
        if (reviewed && path === `/servers/${server.id}`)
          return {
            server: { ...server, permissions: [...SERVER_CAPABILITIES] },
            stats: null,
          };
      },
    });
    await screen.findByRole("heading", {
      name: "Review changed server identity",
    });
    const accept = screen.getByRole("button", {
      name: "Accept binding",
    }) as HTMLButtonElement;
    expect(accept.disabled).toBe(true);
    const confirmation = screen.getByRole("textbox", {
      name: /to accept the changed binding/,
    });
    await userEvent.type(confirmation, server.displayName.toLowerCase());
    expect(accept.disabled).toBe(true);
    await userEvent.clear(confirmation);
    await userEvent.type(confirmation, server.displayName);
    await userEvent.click(accept);
    await screen.findByText("Server binding reviewed.");
    await waitFor(() =>
      expect(
        screen.queryByRole("button", { name: "Accept binding" }),
      ).toBeNull(),
    );
    expect(apiJson).toHaveBeenCalledWith(
      `/servers/${server.id}/binding-review`,
      expect.anything(),
      expect.objectContaining({
        body: JSON.stringify({ confirmation: server.displayName }),
      }),
    );
  });

  it("polls active operations faster and returns to the idle cadence after refresh", async () => {
    const interval = spyOn(window, "setInterval");
    let operations: Operation[] = [
      { ...completedUpdate, status: "running", phase: "backing_up" },
    ];
    detail("admin", {
      onRequest: (path) =>
        path.endsWith("/operations") ? { operations } : undefined,
    });
    await screen.findByText("backing up");
    const pollingDelay = () =>
      interval.mock.calls
        .filter(([, delay]) => delay === 2000 || delay === 10000)
        .at(-1)?.[1];
    await waitFor(() => expect(pollingDelay()).toBe(2000));
    operations = [];
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByText("No operations yet.");
    await waitFor(() => expect(pollingDelay()).toBe(10000));
  });
});

describe("server sharing", () => {
  it("start and stop preset saves only the selected server's three explicit grants", async () => {
    apiJsonMock.mockImplementation(async (path) =>
      path === "/servers" ? { servers: [server] } : { grants: [] },
    );
    render(
      <ServerGrants
        userId="friend"
        username="Friend"
        role="operator"
        onClose={mock()}
      />,
    );
    await userEvent.click(
      await screen.findByRole("button", { name: "Start and stop" }),
    );
    expect(
      (
        screen.getByRole("checkbox", {
          name: "Send console commands",
        }) as HTMLInputElement
      ).checked,
    ).toBe(false);
    expect(
      (
        screen.getByRole("checkbox", {
          name: "Read and download files",
        }) as HTMLInputElement
      ).checked,
    ).toBe(false);
    await userEvent.click(
      screen.getByRole("button", { name: "Save server access" }),
    );
    expect(apiJson).toHaveBeenCalledWith(
      "/users/friend/server-grants",
      expect.anything(),
      expect.objectContaining({
        method: "PUT",
        body: JSON.stringify({
          grants: [
            {
              serverId: server.id,
              capabilities: ["server.view", "server.start", "server.stop"],
            },
          ],
        }),
      }),
    );
  });
  it("viewer grants expose only status, logs, and file read options", async () => {
    apiJsonMock.mockImplementation(async (path) =>
      path === "/servers" ? { servers: [server] } : { grants: [] },
    );
    render(
      <ServerGrants
        userId="viewer"
        username="Viewer"
        role="viewer"
        onClose={mock()}
      />,
    );
    await screen.findByRole("checkbox", { name: "View server" });
    expect(screen.getAllByRole("checkbox")).toHaveLength(3);
    expect(screen.queryByRole("button", { name: "Start and stop" })).toBeNull();
  });
});


describe("server detail request ownership", () => {
  it("ignores an older refresh after a newer binding snapshot arrives", async () => {
    let completeOlder!: (value: unknown) => void;
    const older = new Promise((resolve) => { completeOlder = resolve; });
    let reads = 0;
    detail("admin", { onRequest: (path) => {
      if (path !== `/servers/${server.id}`) return;
      reads += 1;
      if (reads === 2) return older;
      if (reads >= 3) return { server: { ...server, bindingStatus: "review_required", permissions: ["server.view"] } };
    } });
    await screen.findByRole("button", { name: "Refresh" });
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByText(/This server’s binding is review required/);
    await act(async () => completeOlder({ server: { ...server, state: "exited", permissions: [...SERVER_CAPABILITIES] } }));
    expect(screen.getByText(/This server’s binding is review required/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Start", exact: true })).toBeNull();
  });

  it("keeps monitoring defaults unavailable until server settings can be loaded", async () => {
    let failed = true;
    detail("admin", { onRequest: (path) => {
      if (path.endsWith("/availability") && failed) throw new Error("Monitoring unavailable");
    } });
    await userEvent.click(await screen.findByRole("tab", { name: "Availability" }));
    await screen.findByText(/Reload them before making changes/);
    expect(screen.queryByRole("button", { name: "Save monitoring" })).toBeNull();
    expect(screen.queryByRole("checkbox", { name: "Monitor this server" })).toBeNull();
    failed = false;
    await userEvent.click(screen.getByRole("button", { name: "Reload server settings" }));
    await screen.findByRole("button", { name: "Save monitoring" });
  });

  it("blocks an already-open restore when an operation starts", async () => {
    let active = false;
    detail("admin", { backups: [backup], onRequest: (path) => {
      if (path.endsWith("/operations") && active) return { operations: [{ ...completedUpdate, status: "running" }] };
    } });
    await userEvent.click(await screen.findByRole("tab", { name: "Backups" }));
    await userEvent.click(screen.getByRole("button", { name: "Restore…" }));
    fireEvent.change(screen.getByRole("textbox", { name: /to confirm/ }), { target: { value: server.displayName } });
    await userEvent.click(screen.getByRole("tab", { name: "Activity" }));
    active = true;
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await userEvent.click(screen.getByRole("tab", { name: "Backups" }));
    const submit = screen.getByRole("button", { name: "Restore game data" });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.submit(submit.closest("form")!);
    expect(apiJsonMock.mock.calls.some(([path, , init]) => path.endsWith("/restores") && init?.method === "POST")).toBe(false);
  });

  it("consumes update confirmation when the action succeeds but its refresh fails", async () => {
    let queued = false;
    detail("admin", { onRequest: (path, init) => {
      if (path.endsWith("/updates") && init?.method === "POST") {
        queued = true;
        return { operation: { ...completedUpdate, status: "queued" } };
      }
      if (path === `/servers/${server.id}` && queued) throw new Error("Unable to read updated server");
    } });
    await userEvent.click(await screen.findByRole("tab", { name: "Update", exact: true }));
    await userEvent.click(screen.getByRole("checkbox", { name: /I understand/ }));
    await userEvent.click(screen.getByRole("button", { name: "Update server", exact: true }));
    await screen.findByText("Update queued.");
    await screen.findByText("Unable to read updated server");
    await waitFor(() => expect(screen.getByRole("tab", { name: "Activity", selected: true })).toBeTruthy());
    await userEvent.click(screen.getByRole("tab", { name: "Update", exact: true }));
    expect((screen.getByRole("checkbox", { name: /I understand/ }) as HTMLInputElement).checked).toBe(false);
    expect((screen.getByRole("button", { name: "Update server", exact: true }) as HTMLButtonElement).disabled).toBe(true);
  });
});
