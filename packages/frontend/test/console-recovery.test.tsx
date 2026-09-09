import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SERVER_CAPABILITIES } from "@ludock/shared";
import { apiJson } from "../src/api";
import { useWebSocket } from "../src/hooks/useWebSocket";
import { AuthContext, type AuthContextValue, type AuthUser } from "../src/auth-context";
import { NavigationContext } from "../src/navigation-context";
import Console from "../src/pages/Console";
import type { ManagedContainer } from "../src/types";

const { terminals } = vi.hoisted(() => ({
  terminals: [] as Array<{
    write: ReturnType<typeof vi.fn>;
    clear: ReturnType<typeof vi.fn>;
    reset: ReturnType<typeof vi.fn>;
    dispose: ReturnType<typeof vi.fn>;
  }>,
}));
vi.mock("@xterm/xterm", () => ({
  Terminal: class {
    write = vi.fn();
    clear = vi.fn();
    reset = vi.fn();
    dispose = vi.fn();
    loadAddon = vi.fn();
    open = vi.fn();
    constructor() { terminals.push(this); }
  },
}));
vi.mock("@xterm/addon-fit", () => ({ FitAddon: class { fit = vi.fn(); } }));
vi.mock("@xterm/addon-web-links", () => ({ WebLinksAddon: class {} }));
vi.mock("../src/hooks/useWebSocket", () => ({ useWebSocket: vi.fn() }));
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
  gameConsole: { id: "minecraft-rcon", name: "Minecraft RCON", commandPlaceholder: "help" },
  fileRoots: [],
  ports: [],
  created: 0,
  labels: {},
  permissions: [...SERVER_CAPABILITIES],
  bindingStatus: "active",
};
let transport: ReturnType<typeof useWebSocket>;
let socketOptions: Parameters<typeof useWebSocket>[0];

beforeEach(() => {
  terminals.length = 0;
  transport = {
    status: "connected",
    send: vi.fn().mockReturnValue(true),
    retry: vi.fn(),
    canRetry: false,
    accessDenied: false,
    error: null,
  };
  vi.mocked(useWebSocket).mockImplementation((options) => {
    socketOptions = options;
    return options.url ? transport : {
      ...transport, status: "disconnected", canRetry: false, accessDenied: false, error: null,
    };
  });
  vi.mocked(apiJson).mockResolvedValue({ server, stats: null });
});

function consolePage(role: AuthUser["role"] = "admin", containerId = server.id) {
  const navigate = vi.fn();
  const user: AuthUser = { id: "friend", username: "friend", role };
  const content = (id: string) => (
    <AuthContext.Provider value={{ user } as AuthContextValue}>
      <NavigationContext.Provider value={{ pathname: `/console/${id}`, navigate }}>
        <Console containerId={id} />
      </NavigationContext.Provider>
    </AuthContext.Provider>
  );
  const result = render(content(containerId));
  return {
    ...result,
    navigate,
    update: (id = containerId) => result.rerender(content(id)),
  };
}

async function openConnection() {
  await waitFor(() => expect(socketOptions.url).toContain("/ws/v1/"));
  await act(async () => { socketOptions.onOpen?.(); });
}

describe("console recovery", () => {
  it("keeps the draft and does not echo a failed send, then clears only an accepted send", async () => {
    consolePage();
    await openConnection();
    const input = screen.getByRole("textbox", { name: "Game command" });
    await userEvent.type(input, "  list  ");
    vi.mocked(transport.send).mockReturnValueOnce(false);
    await userEvent.click(screen.getByRole("button", { name: "Send", exact: true }));
    expect((input as HTMLInputElement).value).toBe("  list  ");
    expect(transport.send).toHaveBeenCalledWith(JSON.stringify({ type: "input", data: "list" }));
    expect(terminals[0].write).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent).toContain("Check the output before trying again");
    await userEvent.click(screen.getByRole("button", { name: "Send", exact: true }));
    expect((input as HTMLInputElement).value).toBe("");
    expect(terminals[0].write).toHaveBeenCalledWith(expect.stringContaining("> list"));
    expect(screen.getByText("Command sent. Check the output for its result.")).toBeTruthy();
  });

  it("retains an editable disconnected draft and never resends it on recovery", async () => {
    const page = consolePage();
    await openConnection();
    await userEvent.type(screen.getByRole("textbox", { name: "Game command" }), "save-all");
    transport = { ...transport, status: "disconnected", canRetry: true, error: "Connection lost. Automatic retries have stopped." };
    page.update();
    const input = screen.getByRole("textbox", { name: "Game command" }) as HTMLInputElement;
    expect(input.value).toBe("save-all");
    expect(input.disabled).toBe(false);
    expect((screen.getByRole("button", { name: "Send", exact: true }) as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Retry connection" }));
    expect(transport.retry).toHaveBeenCalledTimes(1);
    expect(transport.send).not.toHaveBeenCalled();
    transport = { ...transport, status: "connected", canRetry: false, error: null };
    page.update();
    await openConnection();
    expect(input.value).toBe("save-all");
    expect(transport.send).not.toHaveBeenCalled();
    expect((screen.getByRole("button", { name: "Send", exact: true }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("rechecks grants after reconnecting before allowing a command", async () => {
    let current = server;
    vi.mocked(apiJson).mockImplementation(async () => ({ server: current, stats: null }));
    const page = consolePage("operator");
    await openConnection();
    await userEvent.type(screen.getByRole("textbox", { name: "Game command" }), "list");
    transport = { ...transport, status: "disconnected", canRetry: true };
    page.update();
    current = { ...server, permissions: ["server.view", "logs.read"] };
    transport = { ...transport, status: "connected", canRetry: false };
    page.update();
    await openConnection();
    expect(screen.queryByRole("textbox", { name: "Game command" })).toBeNull();
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual(["Docker Logs"]);
    expect(transport.send).not.toHaveBeenCalled();
    expect(vi.mocked(apiJson).mock.calls.length).toBe(3);
  });

  it("hides stale identity, drafts, and output on explicit access denial until details are reloaded", async () => {
    const page = consolePage();
    await openConnection();
    await userEvent.type(screen.getByRole("textbox", { name: "Game command" }), "private draft");
    act(() => socketOptions.onMessage?.(JSON.stringify({ type: "stdout", data: "old output" })));
    transport = { ...transport, status: "disconnected", accessDenied: true, error: "Connection access is unavailable." };
    page.update();
    expect(screen.queryByText(server.displayName)).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("tabpanel")).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry connection" })).toBeNull();
    expect(terminals[0].reset).toHaveBeenCalled();
    expect(transport.retry).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent).toContain("Reload details to check your access");
    transport = { ...transport, status: "connected", accessDenied: false, error: null };
    page.update();
    expect(screen.queryByRole("textbox")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Reload details" }));
    await openConnection();
    expect((screen.getByRole("textbox", { name: "Game command" }) as HTMLInputElement).value).toBe("private draft");
    expect(transport.send).not.toHaveBeenCalled();
  });

  it("ignores a late permission response after the connection has denied access", async () => {
    let completeVerification: (value: unknown) => void = () => {};
    vi.mocked(apiJson)
      .mockResolvedValueOnce({ server, stats: null })
      .mockImplementationOnce(() => new Promise((resolve) => { completeVerification = resolve; }));
    const page = consolePage();
    await waitFor(() => expect(socketOptions.url).toContain("/ws/v1/"));
    act(() => socketOptions.onOpen?.());
    await userEvent.type(screen.getByRole("textbox", { name: "Game command" }), "list");
    expect((screen.getByRole("button", { name: "Send", exact: true }) as HTMLButtonElement).disabled).toBe(true);
    transport = { ...transport, status: "disconnected", accessDenied: true };
    page.update();
    await act(async () => { completeVerification({ server, stats: null }); });
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("tab")).toBeNull();
    expect(transport.send).not.toHaveBeenCalled();
    expect(transport.retry).not.toHaveBeenCalled();
  });

  it("provides a real server-details link and recovers from a failed details request", async () => {
    vi.mocked(apiJson).mockRejectedValueOnce(new Error("private API detail"));
    const { navigate } = consolePage();
    expect((await screen.findByRole("alert")).textContent).toContain("Server details could not be loaded");
    expect(screen.queryByText("private API detail")).toBeNull();
    const back = screen.getByRole("link", { name: "Back to server details" });
    expect(back.getAttribute("href")).toBe(`/servers/${server.id}`);
    await userEvent.click(back);
    expect(navigate).toHaveBeenCalledWith(`/servers/${server.id}`);
    await userEvent.click(screen.getByRole("button", { name: "Reload details" }));
    await openConnection();
    expect(screen.getByRole("textbox", { name: "Game command" })).toBeTruthy();
  });

  it("keeps actual paused state visible, prevents commands, and still allows granted logs", async () => {
    vi.mocked(apiJson).mockResolvedValue({ server: { ...server, state: "paused" }, stats: null });
    consolePage();
    await screen.findByRole("tab", { name: "Game Console" });
    expect(socketOptions.url).toBe("");
    expect(screen.getByText(/Server state: paused/)).toBeTruthy();
    await userEvent.type(screen.getByRole("textbox", { name: "Game command" }), "list");
    expect((screen.getByRole("button", { name: "Send", exact: true }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/Paused in Docker. Resume it through Docker/)).toBeTruthy();
    const controls = screen.getByRole("link", { name: "Open server details", exact: true });
    expect(controls.getAttribute("href")).toBe(`/servers/${server.id}`);
    await userEvent.click(screen.getByRole("button", { name: "View logs" }));
    expect(document.activeElement).toBe(screen.getByRole("tab", { name: "Docker Logs" }));
    await openConnection();
    expect(socketOptions.url).toContain(`/ws/v1/logs/${server.id}`);
    expect(transport.send).not.toHaveBeenCalled();
  });

  it("does not offer logs or imply start access when neither permission is granted", async () => {
    vi.mocked(apiJson).mockResolvedValue({
      server: { ...server, state: "exited", permissions: ["server.view", "console.execute"] },
      stats: null,
    });
    const { navigate } = consolePage("operator");
    await screen.findByRole("tab", { name: "Game Console" });
    expect(socketOptions.url).toBe("");
    expect(screen.queryByRole("button", { name: "View logs" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Open server controls" })).toBeNull();
    await userEvent.click(screen.getByRole("link", { name: "Open server details", exact: true }));
    expect(navigate).toHaveBeenCalledWith(`/servers/${server.id}`);
    expect(transport.send).not.toHaveBeenCalled();
  });
});

describe("console keyboard navigation", () => {
  it("uses roving tabs and a persistent linked panel while retaining separate mode drafts", async () => {
    consolePage();
    await openConnection();
    const user = userEvent.setup();
    const panel = screen.getByRole("tabpanel", { name: "Game Console" });
    await user.type(screen.getByRole("textbox", { name: "Game command" }), "list");
    const game = screen.getByRole("tab", { name: "Game Console" });
    game.focus();
    await user.keyboard("{End}");
    const shell = screen.getByRole("tab", { name: "Container Shell" });
    expect(document.activeElement).toBe(shell);
    expect(screen.getByRole("tabpanel", { name: "Container Shell" })).toBe(panel);
    await user.type(screen.getByRole("textbox", { name: "Shell command" }), "df -h");
    shell.focus();
    await user.keyboard("{Home}{ArrowRight}");
    expect(document.activeElement).toBe(game);
    expect((screen.getByRole("textbox", { name: "Game command" }) as HTMLInputElement).value).toBe("list");
    for (const tab of within(screen.getByRole("tablist")).getAllByRole("tab")) {
      expect(tab.getAttribute("aria-controls")).toBe(panel.id);
      expect(tab.tabIndex).toBe(tab === game ? 0 : -1);
    }
    expect(panel.getAttribute("aria-labelledby")).toBe(game.id);
    await user.keyboard("{ArrowLeft}{ArrowLeft}");
    expect(document.activeElement).toBe(shell);
    expect((screen.getByRole("textbox", { name: "Shell command" }) as HTMLInputElement).value).toBe("df -h");
    expect(terminals).toHaveLength(1);
    expect(terminals[0].dispose).not.toHaveBeenCalled();
    expect(transport.send).not.toHaveBeenCalled();
  });

  it("does not carry private drafts between different servers", async () => {
    const nextId = "6e174d0b-c46b-4fb3-a1c1-9ad6779517a0";
    vi.mocked(apiJson).mockImplementation(async (path) => ({
      server: { ...server, id: path.includes(nextId) ? nextId : server.id }, stats: null,
    }));
    const page = consolePage();
    await openConnection();
    await userEvent.type(screen.getByRole("textbox", { name: "Game command" }), "private draft");
    page.update(nextId);
    await openConnection();
    expect((screen.getByRole("textbox", { name: "Game command" }) as HTMLInputElement).value).toBe("");
    expect(transport.send).not.toHaveBeenCalled();
  });
});
