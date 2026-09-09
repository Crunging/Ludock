import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AuthContext, type AuthContextValue, type AuthUser } from "../src/auth-context";
import { NavigationContext } from "../src/navigation-context";
import ServerCard from "../src/components/ServerCard";
import Dashboard from "../src/pages/Dashboard";
import { useServers } from "../src/hooks/useServers";
import type { ManagedContainer } from "../src/types";

vi.mock("../src/hooks/useServers", () => ({ useServers: vi.fn() }));

const user: AuthUser = { id: "friend", username: "friend", role: "operator" };
const server: ManagedContainer = {
  id: "53bfe195-b78c-4c14-aebb-1bd09384f33b",
  shortId: "container123",
  name: "world",
  displayName: "Friends world",
  image: "itzg/minecraft-server",
  state: "running",
  status: "Up",
  gameType: "minecraft",
  created: 0,
  gameConsole: {
    id: "minecraft-rcon",
    name: "Minecraft RCON",
    commandPlaceholder: "help",
  },
  fileRoots: [{ id: "data", name: "Data", path: "/data" }],
  ports: [{ private: 25565, public: 25565, type: "tcp" }],
  labels: {},
  bindingStatus: "active",
  permissions: ["server.view", "server.stop", "server.restart", "console.execute", "files.read"],
};

function card(value = server, role: AuthUser["role"] = "operator") {
  const action = vi.fn<(id: string, action: "start" | "stop" | "restart") => Promise<void>>()
    .mockResolvedValue(undefined);
  const navigate = vi.fn();
  const content = (current: ManagedContainer) => (
    <AuthContext.Provider value={{ user: { ...user, role } } as AuthContextValue}>
      <NavigationContext.Provider value={{ pathname: "/", navigate }}>
        <ServerCard server={current} onAction={action} />
        <button>Outside action</button>
      </NavigationContext.Provider>
    </AuthContext.Provider>
  );
  const result = render(content(value));
  return { ...result, action, navigate, update: (current: ManagedContainer) => result.rerender(content(current)) };
}

const fixtures = [
  server,
  { ...server, id: "paused", displayName: "Paused world", state: "paused" },
  { ...server, id: "restarting", displayName: "Restarting world", state: "restarting" },
  { ...server, id: "exited", displayName: "Offline world", state: "exited" },
];

beforeEach(() => {
  vi.mocked(useServers).mockReturnValue({
    servers: fixtures,
    loading: false,
    error: null,
    refresh: vi.fn(),
  });
});

describe("server list actions", () => {
  it("uses a real server link and keeps secondary actions behind More", async () => {
    const { navigate } = card();
    const identity = screen.getByRole("link", { name: server.displayName });
    expect(identity.getAttribute("href")).toBe(`/servers/${server.id}`);
    await userEvent.click(identity);
    expect(navigate).toHaveBeenCalledWith(`/servers/${server.id}`);
    expect(screen.queryByRole("button", { name: /Manage/ })).toBeNull();
    expect(screen.getByRole("link", { name: "Console", exact: true })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Stop", exact: true })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Files", exact: true })).toBeNull();
    expect(screen.queryByRole("button", { name: "Restart…", exact: true })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: /More actions/ }));
    await userEvent.click(screen.getByRole("button", { name: "Files", exact: true }));
    expect(navigate).toHaveBeenCalledWith(`/files/${server.id}`);
  });

  it("retains a restart-only grant and requires confirmation", async () => {
    const { action } = card({ ...server, permissions: ["server.view", "server.restart"] });
    expect(screen.queryByRole("button", { name: "Stop", exact: true })).toBeNull();
    expect(screen.queryByRole("link", { name: "Console", exact: true })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: /More actions/ }));
    expect(screen.queryByRole("button", { name: "Files", exact: true })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Restart…", exact: true }));
    expect(action).not.toHaveBeenCalled();
    const dialog = screen.getByRole("dialog", { name: `Restart ${server.displayName}?` });
    expect(dialog.textContent).toContain("Connected players will be disconnected.");
    await userEvent.click(within(dialog).getByRole("button", { name: "Restart server" }));
    expect(action).toHaveBeenCalledWith(server.id, "restart");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("supports keyboard disclosure, Escape, focus departure, and outside clicks", async () => {
    card();
    const more = screen.getByRole("button", { name: /More actions/ });
    more.focus();
    await userEvent.keyboard("{Enter}");
    expect(more.getAttribute("aria-expanded")).toBe("true");
    await userEvent.tab();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Files", exact: true }));
    await userEvent.keyboard("{Escape}");
    expect(more.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(more);

    await userEvent.keyboard("{Enter}");
    await userEvent.tab();
    await userEvent.tab();
    await userEvent.tab();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Outside action" }));
    expect(more.getAttribute("aria-expanded")).toBe("false");

    await userEvent.click(more);
    await userEvent.click(screen.getByRole("button", { name: "Outside action" }));
    expect(more.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Outside action" }));
  });

  it("returns focus after canceling confirmation from More and from Stop", async () => {
    const { action } = card();
    const more = screen.getByRole("button", { name: /More actions/ });
    await userEvent.click(more);
    await userEvent.click(screen.getByRole("button", { name: "Restart…", exact: true }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Cancel" }));
    fireEvent(screen.getByRole("dialog"), new Event("cancel", { cancelable: true }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(more);

    const stop = screen.getByRole("button", { name: "Stop", exact: true });
    await userEvent.click(stop);
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(document.activeElement).toBe(stop);
    expect(action).not.toHaveBeenCalled();
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

  it("keeps viewer grants limited to readable tools", async () => {
    card({ ...server, permissions: [...server.permissions, "logs.read"] }, "viewer");
    expect(screen.getByRole("link", { name: "Logs", exact: true })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Stop", exact: true })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: /More actions/ }));
    expect(screen.getByRole("button", { name: "Files", exact: true })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Restart…", exact: true })).toBeNull();
  });
});

describe("server filters", () => {
  function dashboard() {
    const content = () => (
      <AuthContext.Provider value={{ user } as AuthContextValue}>
        <NavigationContext.Provider value={{ pathname: "/", navigate: vi.fn() }}>
          <Dashboard />
        </NavigationContext.Provider>
      </AuthContext.Provider>
    );
    const result = render(content());
    return { update: () => result.rerender(content()) };
  }

  it("filters exact states independently from search and clears empty results", async () => {
    dashboard();
    const state = screen.getByRole("combobox", { name: "State" });
    expect(within(state).getAllByRole("option").map((option) => option.textContent)).toEqual([
      "All states", "Exited", "Paused", "Restarting", "Running",
    ]);
    await userEvent.selectOptions(state, "paused");
    expect(screen.getAllByRole("article")).toHaveLength(1);
    expect(screen.getByRole("link", { name: "Paused world" })).toBeTruthy();
    expect(screen.getByRole("status").textContent).toBe("1 of 4 servers");
    await userEvent.type(screen.getByRole("searchbox", { name: "Find a server" }), "Minecraft");
    expect(screen.getAllByRole("article")).toHaveLength(1);
    await userEvent.type(screen.getByRole("searchbox", { name: "Find a server" }), "missing");
    expect(screen.queryByRole("article")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(screen.getAllByRole("article")).toHaveLength(4);
    expect((state as HTMLSelectElement).value).toBe("all");
  });

  it("keeps the selected state understandable when the last match changes state", async () => {
    const { update } = dashboard();
    const state = screen.getByRole("combobox", { name: "State" });
    await userEvent.selectOptions(state, "paused");
    vi.mocked(useServers).mockReturnValue({
      servers: fixtures.filter((fixture) => fixture.state !== "paused"),
      loading: false,
      error: null,
      refresh: vi.fn(),
    });
    update();
    expect((state as HTMLSelectElement).value).toBe("paused");
    expect(within(state).getByRole("option", { name: "Paused" })).toBeTruthy();
    expect(screen.getByRole("status").textContent).toBe("0 of 3 servers");
    await userEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(screen.getAllByRole("article")).toHaveLength(3);
  });
});
