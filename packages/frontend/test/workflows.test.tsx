import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AuthContext, type AuthContextValue } from "../src/auth-context";
import { NavigationContext } from "../src/navigation-context";
import ServerDetail from "../src/pages/ServerDetail";
import ServerGrants from "../src/components/ServerGrants";
import { apiJson, apiFetch } from "../src/api";

vi.mock("../src/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../src/api")>()),
  apiJson: vi.fn(),
  apiFetch: vi.fn(),
}));
const server = {
  id: "53bfe195-b78c-4c14-aebb-1bd09384f33b",
  shortId: "docker123",
  name: "world",
  displayName: "Friends world",
  image: "itzg/minecraft-server",
  state: "running",
  status: "Up",
  gameType: "minecraft",
  gameConsole: null,
  fileRoots: [],
  ports: [],
  created: 0,
  labels: {},
  permissions: ["server.view", "server.start", "server.stop"],
  bindingStatus: "active",
};
function detail(role = "admin") {
  vi.mocked(apiJson).mockImplementation(async (path) => {
    if (path.endsWith("/update-capability"))
      return {
        capability: {
          available: true,
          actionLabel: "Update server",
          projectName: "games",
          serviceName: "minecraft",
          image: server.image,
          manager: "compose",
        },
      };
    if (path.endsWith("/availability"))
      return {
        policy: { enabled: false, maintenance: false, graceSeconds: 120 },
      };
    if (path.endsWith("/operations"))
      return {
        operations: [
          {
            id: "op1",
            serverId: server.id,
            kind: "update",
            status: "already_current",
            phase: "finished",
            createdAt: 0,
            updatedAt: 0,
            error: null,
            result: null,
          },
        ],
      };
    if (path.endsWith("/backups")) return { backups: [] };
    if (path.endsWith("/schedules")) return { schedules: [] };
    if (path.endsWith("/updates")) return { operation: { id: "op2" } };
    return { server };
  });
  return render(
    <AuthContext.Provider
      value={
        { user: { id: "user1", role, username: "admin" } } as AuthContextValue
      }
    >
      <NavigationContext.Provider
        value={{ pathname: `/servers/${server.id}`, navigate: vi.fn() }}
      >
        <ServerDetail serverId={server.id} />
      </NavigationContext.Provider>
    </AuthContext.Provider>,
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
    for (const name of ["Update", "Backups", "Schedules", "Availability"])
      expect(screen.queryByRole("tab", { name, exact: true })).toBeNull();
  });
});

describe("server sharing", () => {
  it("start and stop preset saves only the selected server's three explicit grants", async () => {
    vi.mocked(apiFetch).mockImplementation(async (path, options) => {
      if (options?.method === "PUT")
        return new Response(JSON.stringify({ grants: [] }));
      return new Response(
        JSON.stringify(
          String(path).endsWith("/servers")
            ? { servers: [server] }
            : { grants: [] },
        ),
      );
    });
    render(
      <ServerGrants
        userId="friend"
        username="Friend"
        role="operator"
        onClose={vi.fn()}
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
    expect(apiFetch).toHaveBeenCalledWith(
      "/api/v1/users/friend/server-grants",
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
    vi.mocked(apiFetch).mockImplementation(
      async (path) =>
        new Response(
          JSON.stringify(
            String(path).endsWith("/servers")
              ? { servers: [server] }
              : { grants: [] },
          ),
        ),
    );
    render(
      <ServerGrants
        userId="viewer"
        username="Viewer"
        role="viewer"
        onClose={vi.fn()}
      />,
    );
    await screen.findByRole("checkbox", { name: "View server" });
    expect(screen.getAllByRole("checkbox")).toHaveLength(3);
    expect(screen.queryByRole("button", { name: "Start and stop" })).toBeNull();
  });
});
