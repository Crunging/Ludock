import { beforeEach, describe, expect, it, mock } from "bun:test";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SERVER_CAPABILITIES } from "@ludock/shared";
import { ApiRequestError, apiJson } from "../src/api";
import { useWebSocket } from "../src/hooks/useWebSocket";
import type { AuthUser } from "../src/auth-context";
import TestProviders from "./TestProviders";
import { serverFixture } from "./fixtures";

const terminals: Array<{
  write: ReturnType<typeof mock>;
  clear: ReturnType<typeof mock>;
  reset: ReturnType<typeof mock>;
  dispose: ReturnType<typeof mock>;
}> = [];
mock.module("@xterm/xterm", () => ({
  Terminal: class {
    write = mock();
    clear = mock();
    reset = mock();
    dispose = mock();
    loadAddon = mock();
    open = mock();
    constructor() { terminals.push(this); }
  },
}));
mock.module("@xterm/addon-fit", () => ({ FitAddon: class { fit = mock(); } }));
mock.module("@xterm/addon-web-links", () => ({ WebLinksAddon: class {} }));
const useWebSocketMock = mock<typeof useWebSocket>();
mock.module("../src/hooks/useWebSocket", () => ({ useWebSocket: useWebSocketMock }));
const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({
  ...originalApi,
  apiJson: apiJsonMock,
}));

const { default: Console } = await import("../src/pages/Console");

const server = serverFixture({
  gameConsole: { id: "minecraft-rcon", name: "Minecraft RCON", commandPlaceholder: "help" },
  permissions: [...SERVER_CAPABILITIES],
});
let transport: ReturnType<typeof useWebSocket>;
let socketOptions: Parameters<typeof useWebSocket>[0];

beforeEach(() => {
  terminals.length = 0;
  transport = {
    status: "connected",
    send: mock().mockReturnValue(true),
    retry: mock(),
    canRetry: false,
    accessDenied: false,
    error: null,
  };
  useWebSocketMock.mockImplementation((options) => {
    socketOptions = options;
    return options.url ? transport : {
      ...transport, status: "disconnected", canRetry: false, accessDenied: false, error: null,
    };
  });
  apiJsonMock.mockResolvedValue({ server, stats: null });
});

function consolePage(role: AuthUser["role"] = "admin", serverId = server.id) {
  const navigate = mock();
  const user: AuthUser = { id: "friend", username: "friend", role };
  const content = (id: string) => (
    <TestProviders user={user} pathname={`/console/${id}`} navigate={navigate}>
      <Console serverId={id} />
    </TestProviders>
  );
  const result = render(content(serverId));
  return {
    ...result,
    navigate,
    update: (id = serverId) => result.rerender(content(id)),
  };
}

async function openConnection() {
  await waitFor(() => expect(socketOptions.url).toContain("/ws/v1/"));
  await act(async () => { socketOptions.onOpen?.(); });
}

describe("console recovery", () => {
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

  it("rechecks grants after reconnecting and fails closed when fallback log access is denied", async () => {
    let current = server;
    apiJsonMock.mockImplementation(async () => ({ server: current, stats: null }));
    const page = consolePage("operator");
    await openConnection();
    await userEvent.type(screen.getByRole("textbox", { name: "Game command" }), "list");
    act(() => socketOptions.onMessage?.(JSON.stringify({ type: "stdout", data: "private game output" })));
    const resets = terminals[0].reset.mock.calls.length;
    transport = { ...transport, status: "disconnected", canRetry: true };
    page.update();
    current = { ...server, permissions: ["server.view", "logs.read"] };
    transport = { ...transport, status: "connected", canRetry: false };
    page.update();
    await openConnection();
    expect(screen.queryByRole("textbox", { name: "Game command" })).toBeNull();
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual(["Docker Logs"]);
    expect(terminals[0].reset.mock.calls.length).toBeGreaterThan(resets);
    expect(terminals[0].write).toHaveBeenLastCalledWith(expect.stringContaining("Switched to Docker Logs"));
    expect(transport.send).not.toHaveBeenCalled();
    expect(apiJsonMock.mock.calls.length).toBe(3);
    apiJsonMock.mockRejectedValueOnce(new ApiRequestError("Log access no longer available", 403));
    await openConnection();
    await screen.findByRole("alert");
    expect(screen.queryByRole("tabpanel")).toBeNull();
    expect(socketOptions.url).toBe("");
    expect(transport.retry).not.toHaveBeenCalled();
  });

  it("ignores a late permission response after the connection has denied access", async () => {
    let completeVerification: (value: unknown) => void = () => {};
    apiJsonMock
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

  it("does not carry private drafts between different servers", async () => {
    const nextId = "6e174d0b-c46b-4fb3-a1c1-9ad6779517a0";
    apiJsonMock.mockImplementation(async (path) => ({
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
