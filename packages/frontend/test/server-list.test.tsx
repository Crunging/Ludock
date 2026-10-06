import type { Server } from "@ludock/shared";
import { describe, expect, it, jest, mock } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AuthContext, type AuthContextValue, type AuthUser } from "../src/auth-context";
import { NavigationContext } from "../src/navigation-context";
import ServerCard from "../src/components/ServerCard";

const user: AuthUser = { id: "friend", username: "friend", role: "operator" };
const server: Server = {
  id: "53bfe195-b78c-4c14-aebb-1bd09384f33b",
  shortId: "container123",
  name: "world",
  displayName: "Friends world",
  image: "itzg/minecraft-server",
  state: "running",
  status: "Up",
  health: null,
  stateSince: null,
  exit: null,
  gameType: "minecraft",
  gameName: "Minecraft",
  connection: { host: "play.example.com", port: 25565, source: "detected" },
  created: 0,
  gameConsole: {
    id: "minecraft-rcon",
    name: "Minecraft RCON",
    commandPlaceholder: "help",
    commands: [],
  },
  fileRoots: [{ id: "data", name: "Data", path: "/data" }],
  ports: [{ private: 25565, public: 25565, type: "tcp" }],
  labels: {},
  latestBackup: null,
  bindingStatus: "active",
  permissions: ["server.view", "server.stop", "server.restart", "console.execute", "files.read"],
};

function card() {
  const action = mock<(id: string, action: "start" | "stop" | "restart") => Promise<void>>()
    .mockResolvedValue(undefined);
  const navigate = mock();
  const content = (current: Server) => (
    <AuthContext.Provider value={{ user } as AuthContextValue}>
      <NavigationContext.Provider value={{ pathname: "/", navigate }}>
        <ServerCard server={current} onAction={action} />
      </NavigationContext.Provider>
    </AuthContext.Provider>
  );
  const result = render(content(server));
  return { action, update: (current: Server) => result.rerender(content(current)) };
}

describe("server list", () => {
  it("keeps relative times current on an idle page", () => {
    const start = Date.UTC(2026, 8, 15, 12);
    jest.useFakeTimers({ now: start });
    try {
      render(
        <AuthContext.Provider value={{ user } as AuthContextValue}>
          <NavigationContext.Provider value={{ pathname: "/", navigate: mock() }}>
            <ServerCard
              server={{
                ...server,
                permissions: [...server.permissions, "backups.create"],
                stateSince: start,
                latestBackup: { createdAt: start - 3_600_000, size: 1 },
              }}
              showBackup
              onAction={mock()}
            />
          </NavigationContext.Provider>
        </AuthContext.Provider>,
      );
      expect(screen.getByText("Started just now")).toBeTruthy();
      expect(screen.getByText("1 hour ago")).toBeTruthy();
      act(() => jest.advanceTimersByTime(5 * 60_000));
      expect(screen.getByText("Started 5 minutes ago")).toBeTruthy();
      act(() => jest.advanceTimersByTime(60 * 60_000));
      expect(screen.getByText("2 hours ago")).toBeTruthy();
    } finally {
      jest.useRealTimers();
    }
  });

  it("rechecks authorization and binding if they change during confirmation", async () => {
    const { action, update } = card();
    await userEvent.click(screen.getByRole("button", { name: "Stop", exact: true }));
    update({ ...server, bindingStatus: "review_required" });
    await userEvent.click(screen.getByRole("button", { name: "Stop server" }));
    expect(action).not.toHaveBeenCalled();

    update(server);
    await userEvent.click(screen.getByRole("button", { name: "Stop", exact: true }));
    update({ ...server, permissions: ["server.view"] });
    await userEvent.click(screen.getByRole("button", { name: "Stop server" }));
    expect(action).not.toHaveBeenCalled();

    update(server);
    await userEvent.click(screen.getByRole("button", { name: "Stop", exact: true }));
    update({ ...server, state: "paused" });
    await userEvent.click(screen.getByRole("button", { name: "Stop server" }));
    expect(action).not.toHaveBeenCalled();
  });

  it("disables further lifecycle controls while a request is pending", async () => {
    const { action } = card();
    let complete: () => void = () => {};
    action.mockReturnValue(new Promise<void>((resolve) => { complete = resolve; }));
    await userEvent.click(screen.getByRole("button", { name: "Stop", exact: true }));
    await userEvent.click(screen.getByRole("button", { name: "Stop server" }));
    expect((screen.getByRole("button", { name: "Working…" }) as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: /More actions/ }));
    expect((screen.getByRole("button", { name: "Restart…", exact: true }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => { complete(); });
    expect(action).toHaveBeenCalledTimes(1);
  });
});
