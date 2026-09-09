import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SERVER_CAPABILITIES } from "@ludock/shared";
import { apiJson } from "../src/api";
import {
  AuthContext,
  type AuthContextValue,
  type AuthUser,
} from "../src/auth-context";
import { NavigationContext } from "../src/navigation-context";
import ServerDetail from "../src/pages/ServerDetail";
import type { ManagedContainer } from "../src/types";

vi.mock("../src/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../src/api")>()),
  apiJson: vi.fn(),
}));

const server: ManagedContainer = {
  id: "53bfe195-b78c-4c14-aebb-1bd09384f33b",
  shortId: "docker123",
  name: "world",
  displayName: "Friends world",
  image: "itzg/minecraft-server",
  state: "running",
  status: "Up",
  gameType: "minecraft",
  gameConsole: null,
  fileRoots: [{ id: "root-0", path: "/data" }],
  ports: [
    { private: 25565, public: 25565, type: "tcp" },
    { private: 25575, public: 0, type: "tcp" },
  ],
  created: 0,
  labels: {},
  permissions: [...SERVER_CAPABILITIES],
  bindingStatus: "active",
};

function detail({
  role = "admin",
  current = server,
  onRequest,
}: {
  role?: AuthUser["role"];
  current?: ManagedContainer;
  onRequest?: (path: string, init?: RequestInit) => unknown | Promise<unknown>;
} = {}) {
  const navigate = vi.fn();
  vi.mocked(apiJson).mockImplementation(async (path, _schema, init) => {
    const response = await onRequest?.(path, init);
    if (response !== undefined) return response;
    if (path.endsWith("/operations")) return { operations: [] };
    if (path.endsWith("/backups")) return { backups: [] };
    if (path.endsWith("/schedules")) return { schedules: [] };
    if (path.endsWith("/update-capability"))
      return {
        capability: {
          available: true,
          actionLabel: "Update server",
          projectName: "games",
          serviceName: "minecraft",
          image: current.image,
          manager: "compose",
        },
      };
    if (path.endsWith("/availability"))
      return {
        policy: { enabled: false, maintenance: false, graceSeconds: 120 },
      };
    return { server: current, stats: null };
  });
  render(
    <AuthContext.Provider
      value={
        { user: { id: "user1", role, username: "friend" } } as AuthContextValue
      }
    >
      <NavigationContext.Provider
        value={{ pathname: `/servers/${current.id}`, navigate }}
      >
        <ServerDetail serverId={current.id} />
      </NavigationContext.Provider>
    </AuthContext.Provider>,
  );
  return { navigate };
}

describe("server detail navigation", () => {
  it("uses one tab stop with arrow, Home, End and linked panels without discarding drafts", async () => {
    detail();
    const user = userEvent.setup();
    const activity = await screen.findByRole("tab", { name: "Activity" });
    const tabs = within(screen.getByRole("tablist")).getAllByRole("tab");
    for (const tab of tabs) {
      const panel = document.getElementById(tab.getAttribute("aria-controls")!);
      expect(panel?.getAttribute("aria-labelledby")).toBe(tab.id);
      expect(tab.tabIndex).toBe(tab === activity ? 0 : -1);
    }

    activity.focus();
    await user.keyboard("{ArrowRight}{ArrowRight}");
    const schedules = screen.getByRole("tab", { name: "Schedules" });
    expect(document.activeElement).toBe(schedules);
    expect(schedules.getAttribute("aria-selected")).toBe("true");
    await user.tab();
    expect(document.activeElement).toBe(
      screen.getByRole("tabpanel", { name: "Schedules" }),
    );
    const timezone = screen.getByRole("textbox", { name: "Time zone" });
    await user.clear(timezone);
    await user.type(timezone, "Europe/London");

    schedules.focus();
    await user.keyboard("{End}");
    expect(document.activeElement).toBe(
      screen.getByRole("tab", { name: "Availability" }),
    );
    await user.keyboard("{ArrowRight}");
    expect(document.activeElement).toBe(activity);
    await user.keyboard("{ArrowLeft}");
    expect(document.activeElement).toBe(
      screen.getByRole("tab", { name: "Availability" }),
    );
    await user.keyboard("{Home}{ArrowRight}{ArrowRight}");
    expect(
      (screen.getByRole("textbox", { name: "Time zone" }) as HTMLInputElement)
        .value,
    ).toBe("Europe/London");
    expect(screen.getAllByRole("tabpanel")).toHaveLength(1);
  });

  it("keeps granted logs and files reachable with published ports and a real back link", async () => {
    const { navigate } = detail({
      role: "viewer",
      current: {
        ...server,
        permissions: ["server.view", "logs.read", "files.read"],
      },
    });
    await screen.findByRole("heading", { name: server.displayName });
    expect(screen.getByText("25565/tcp")).toBeTruthy();
    expect(screen.queryByText("0/tcp")).toBeNull();
    const logs = screen.getByRole("link", { name: "Logs" });
    expect(logs.getAttribute("href")).toBe(`/console/${server.id}`);
    expect(screen.getByRole("link", { name: "Files" }).getAttribute("href"))
      .toBe(`/files/${server.id}`);
    for (const action of ["Start", "Stop", "Restart"])
      expect(screen.queryByRole("button", { name: action, exact: true })).toBeNull();
    await userEvent.click(logs);
    expect(navigate).toHaveBeenCalledWith(`/console/${server.id}`);
    await userEvent.click(screen.getByRole("link", { name: "All servers" }));
    expect(navigate).toHaveBeenLastCalledWith("/");
    expect(
      vi.mocked(apiJson).mock.calls.some(([path]) =>
        /\/(backups|schedules|availability|update-capability)$/.test(path),
      ),
    ).toBe(false);
  });

  it("moves keyboard focus into Update when Activity opens the recreation form", async () => {
    detail({
      onRequest: (path) => {
        if (path.endsWith("/operations"))
          return {
            operations: [{
              id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df0",
              serverId: server.id,
              kind: "update",
              status: "already_current",
              phase: "finished",
              createdAt: 0,
              updatedAt: 0,
              error: null,
              result: null,
            }],
          };
      },
    });
    const recreate = await screen.findByRole("button", { name: "Recreate anyway" });
    recreate.focus();
    await userEvent.keyboard("{Enter}");
    expect(document.activeElement).toBe(
      screen.getByRole("tabpanel", { name: "Update" }),
    );
    expect(
      (screen.getByRole("button", { name: "Recreate service" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
  });

  it("reports a failed start and restores the control without granting other actions", async () => {
    detail({
      role: "operator",
      current: {
        ...server,
        state: "exited",
        permissions: ["server.view", "server.start"],
      },
      onRequest: (path, init) => {
        if (path.endsWith("/start") && init?.method === "POST")
          throw new Error("Another operation is using this server.");
      },
    });
    const start = await screen.findByRole("button", { name: "Start" });
    await userEvent.click(start);
    expect((await screen.findByRole("alert")).textContent).toContain(
      "Another operation is using this server.",
    );
    await waitFor(() => expect((start as HTMLButtonElement).disabled).toBe(false));
    expect(apiJson).toHaveBeenCalledWith(
      `/servers/${server.id}/start`,
      expect.anything(),
      { method: "POST" },
    );
    expect(screen.queryByRole("link", { name: "Console" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Files" })).toBeNull();
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "Activity",
    ]);
  });

  it("requires confirmation for stop, restores focus on cancel, and refreshes state after success", async () => {
    let stopped = false;
    const current = {
      ...server,
      permissions: ["server.view", "server.stop"] as ManagedContainer["permissions"],
    };
    detail({
      role: "operator",
      current,
      onRequest: (path, init) => {
        if (path.endsWith("/stop") && init?.method === "POST") {
          stopped = true;
          return { ok: true };
        }
        if (stopped && path === `/servers/${server.id}`)
          return { server: { ...current, state: "exited" }, stats: null };
      },
    });
    const user = userEvent.setup();
    const stop = await screen.findByRole("button", { name: "Stop", exact: true });
    await user.click(stop);
    expect(screen.getByRole("dialog", { name: "Stop Friends world?" })).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Cancel" }));
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(stop);
    expect(stopped).toBe(false);

    await user.click(stop);
    await user.click(screen.getByRole("button", { name: "Stop server", exact: true }));
    expect((await screen.findByRole("status")).textContent).toBe("Server stopped.");
    await waitFor(() => expect(screen.queryByRole("button", { name: "Stop", exact: true })).toBeNull());
    expect(screen.getByText("exited")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Start", exact: true })).toBeNull();
    expect(apiJson).toHaveBeenCalledWith(
      `/servers/${server.id}/stop`,
      expect.anything(),
      { method: "POST" },
    );
  });

  it("disables lifecycle controls when server identity needs review", async () => {
    detail({
      role: "operator",
      current: {
        ...server,
        bindingStatus: "review_required",
        permissions: ["server.view", "server.stop", "server.restart"],
      },
    });
    const stop = await screen.findByRole("button", { name: "Stop", exact: true });
    expect((stop as HTMLButtonElement).disabled).toBe(true);
    expect(
      (screen.getByRole("button", { name: "Restart", exact: true }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    await userEvent.click(stop);
    expect(vi.mocked(apiJson).mock.calls.some(([, , init]) => init?.method === "POST"))
      .toBe(false);
  });
});
