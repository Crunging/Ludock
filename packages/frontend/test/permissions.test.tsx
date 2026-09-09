import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  AuthContext,
  type AuthContextValue,
  type AuthUser,
} from "../src/auth-context";
import { NavigationContext } from "../src/navigation-context";
import ServerCard from "../src/components/ServerCard";
import { can, toggleGrant } from "../src/permissions";
import type { ManagedContainer } from "../src/types";

const friend: AuthUser = { id: "friend", username: "friend", role: "operator" };
const server: ManagedContainer = {
  id: "53bfe195-b78c-4c14-aebb-1bd09384f33b",
  shortId: "container123",
  name: "minecraft",
  displayName: "Friends' world",
  image: "itzg/minecraft-server",
  state: "exited",
  status: "Exited",
  gameType: "minecraft",
  created: 0,
  gameConsole: {
    id: "minecraft-rcon",
    name: "Minecraft RCON",
    commandPlaceholder: "help",
  },
  fileRoots: [{ id: "data", name: "Data", path: "/data" }],
  ports: [],
  labels: {},
  bindingStatus: "active",
  permissions: ["server.view", "server.start", "server.stop"],
};
function renderServer(value: ManagedContainer, user = friend) {
  const action = vi.fn().mockResolvedValue(undefined);
  const navigate = vi.fn();
  render(
    <AuthContext.Provider value={{ user } as AuthContextValue}>
      <NavigationContext.Provider value={{ pathname: "/", navigate }}>
        <ServerCard server={value} onAction={action} />
      </NavigationContext.Provider>
    </AuthContext.Provider>,
  );
  return { action, navigate };
}

describe("server controls", () => {
  it("start/stop sharing never shows commands, logs, files, or restart", async () => {
    const { action } = renderServer(server);
    expect(screen.queryByRole("link", { name: "Console" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Logs" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Files" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Restart" })).toBeNull();
    await userEvent.click(
      screen.getByRole("button", { name: "Start", exact: true }),
    );
    expect(action).toHaveBeenCalledWith(server.id, "start");
  });
  it("keeps console and logs separate, and evaluates each server", () => {
    const scoped = {
      ...server,
      permissions: [
        "server.view",
        "console.execute",
      ] as ManagedContainer["permissions"],
    };
    renderServer(scoped);
    expect(
      screen.getByRole("link", { name: "Console", exact: true }),
    ).toBeTruthy();
    expect(can(friend, server, "console.execute")).toBe(false);
    expect(can(friend, scoped, "logs.read")).toBe(false);
  });
  it("viewer ceiling suppresses actions even in malformed grants", () => {
    renderServer(
      {
        ...server,
        permissions: [
          "server.view",
          "server.start",
          "console.execute",
          "files.write",
        ],
      },
      { ...friend, role: "viewer" },
    );
    expect(
      screen.queryByRole("button", { name: "Start", exact: true }),
    ).toBeNull();
    expect(
      screen.queryByRole("link", { name: "Console", exact: true }),
    ).toBeNull();
  });
  it("blocks lifecycle actions while identity needs review", async () => {
    const { action } = renderServer({
      ...server,
      bindingStatus: "review_required",
    });
    const start = screen.getByRole("button", {
      name: "Start",
      exact: true,
    }) as HTMLButtonElement;
    expect(start.disabled).toBe(true);
    await userEvent.click(start);
    expect(action).not.toHaveBeenCalled();
  });
  it("honors restricted capabilities when an administrator is reviewing a binding", () => {
    expect(
      can(
        { ...friend, role: "admin" },
        {
          ...server,
          bindingStatus: "review_required",
          permissions: ["server.view"],
        },
        "console.shell",
      ),
    ).toBe(false);
  });
  it("an operator without grants has no privileged fallback", () => {
    expect(can(friend, { ...server, permissions: [] }, "server.start")).toBe(
      false,
    );
    expect(
      can(
        friend,
        { ...server, permissions: ["server.view", "server.update"] },
        "server.update",
      ),
    ).toBe(false);
    expect(can(null, server, "server.view")).toBe(false);
  });
});

describe("grant dependencies", () => {
  it("requires file reading when writing is selected and removes writing with reading", () => {
    expect(toggleGrant([], "files.write", true)).toEqual([
      "server.view",
      "files.read",
      "files.write",
    ]);
    expect(
      toggleGrant(
        ["server.view", "files.read", "files.write"],
        "files.read",
        false,
      ),
    ).toEqual(["server.view"]);
  });
  it("removing server visibility removes every action", () => {
    expect(toggleGrant(server.permissions, "server.view", false)).toEqual([]);
  });
});
