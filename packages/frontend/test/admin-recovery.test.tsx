import { act, fireEvent, render as renderComponent, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, mock } from "bun:test";
import { apiJson } from "../src/api";
import ServerGrants from "../src/components/ServerGrants";
import Settings from "../src/pages/Settings";
import Users from "../src/pages/Users";
import { AuthContext, type AuthContextValue } from "../src/auth-context";
import { NavigationProvider } from "../src/navigation";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({
  ...originalApi,
  apiJson: apiJsonMock,
}));

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((complete) => { resolve = complete; });
  return { promise, resolve };
}

const member = { id: "member", username: "friend", role: "operator", disabled: false, createdAt: 0 };
const deployment = { backupRoots: ["/backups"], composeRoots: ["/compose"], composeAvailable: true };

function render(ui: ReactNode) {
  return renderComponent(<NavigationProvider>{ui}</NavigationProvider>);
}

function renderUsers() {
  return render(<AuthContext.Provider value={{ user: { id: "admin", role: "admin", username: "admin" } } as AuthContextValue}><Users /></AuthContext.Provider>);
}

describe("administration recovery", () => {
  it("does not resubmit hidden operator grants after a user becomes a viewer", async () => {
    apiJsonMock.mockImplementation(async (path) => path === "/servers"
      ? { servers: [{ id: "world", displayName: "World" }] }
      : { grants: [{ serverId: "world", capabilities: ["server.view", "server.stop", "logs.read"] }] });
    render(<ServerGrants userId="member" username="friend" role="viewer" onClose={mock()} />);
    await userEvent.click(await screen.findByRole("button", { name: "Save server access" }));
    expect(apiJson).toHaveBeenLastCalledWith("/users/member/server-grants", expect.anything(), expect.objectContaining({
      method: "PUT", body: JSON.stringify({ grants: [{ serverId: "world", capabilities: ["server.view", "logs.read"] }] }),
    }));
  });

  it("keeps grants unavailable after a failed read and retries without saving empty defaults", async () => {
    let unavailable = true;
    apiJsonMock.mockImplementation(async (path) => {
      if (path === "/servers") return { servers: [] };
      if (unavailable) throw new Error("Server access unavailable");
      return { grants: [] };
    });
    render(<ServerGrants userId="member" username="friend" role="operator" onClose={mock()} />);
    await screen.findByRole("alert");
    expect(screen.queryByRole("button", { name: "Save server access" })).toBeNull();
    expect(screen.queryByText("No eligible servers are available to assign.")).toBeNull();
    unavailable = false;
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    await screen.findByRole("button", { name: "Save server access" });
    expect(apiJsonMock.mock.calls.some(([, , init]) => init?.method === "PUT")).toBe(false);
  });

  it("does not expose settings defaults when one of the initial reads fails", async () => {
    let unavailable = true;
    apiJsonMock.mockImplementation(async (path) => {
      if (path === "/notifications/deliveries") return { deliveries: [] };
      if (path === "/settings/deployment") return deployment;
      if (path === "/settings/backups") return { settings: { destination: "/saved", retentionCount: 4, maxBytes: 1024 ** 3, reserveBytes: 0 } };
      if (unavailable) throw new Error("Notifications unavailable");
      return { configured: true, enabled: true };
    });
    render(<Settings />);
    await screen.findByRole("alert");
    expect(screen.queryByRole("button", { name: "Save backup settings" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save notifications" })).toBeNull();
    unavailable = false;
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect((await screen.findByLabelText("Mounted destination path") as HTMLInputElement).value).toBe("/saved");
    expect((screen.getByRole("checkbox", { name: "Enable Discord delivery" }) as HTMLInputElement).checked).toBe(true);
  });

  it("keeps a newer webhook draft after an earlier settings save finishes", async () => {
    const pending = deferred<unknown>();
    apiJsonMock.mockImplementation(async (path, _schema, init) => {
      if (init?.method === "PUT") return pending.promise;
      if (path === "/notifications/deliveries") return { deliveries: [] };
      if (path === "/settings/deployment") return deployment;
      if (path === "/settings/backups") return { settings: null };
      return { configured: false, enabled: false };
    });
    render(<Settings />);
    const webhook = await screen.findByLabelText("Webhook URL");
    fireEvent.change(webhook, { target: { value: "https://discord.com/api/webhooks/first" } });
    await userEvent.click(screen.getByRole("button", { name: "Save notifications" }));
    fireEvent.change(webhook, { target: { value: "https://discord.com/api/webhooks/next" } });
    await act(async () => pending.resolve({ configured: true, enabled: false }));
    await screen.findByRole("status");
    expect((screen.getByLabelText("Replace webhook URL") as HTMLInputElement).value).toBe("https://discord.com/api/webhooks/next");
  });

  it("allows clearing and typing decimal backup limits and keeps edits made during a save", async () => {
    const user = userEvent.setup();
    const pending = deferred<unknown>();
    let submitted: unknown;
    let saveCount = 0;
    apiJsonMock.mockImplementation(async (path, _schema, init) => {
      if (init?.method === "PUT") {
        submitted = JSON.parse(String(init.body));
        if (++saveCount > 1) return { settings: submitted };
        return pending.promise;
      }
      if (path === "/notifications/deliveries") return { deliveries: [] };
      if (path === "/settings/deployment") return deployment;
      if (path === "/settings/backups") return { settings: {
        destination: "/backups", retentionCount: 10,
        maxBytes: 100 * 1024 ** 3, reserveBytes: 5 * 1024 ** 3,
      } };
      return { configured: false, enabled: false };
    });
    render(<Settings />);
    const retention = await screen.findByLabelText("Backups per server") as HTMLInputElement;
    const limit = screen.getByLabelText("Total backup limit (GiB)") as HTMLInputElement;
    const reserve = screen.getByLabelText("Minimum free space (GiB)") as HTMLInputElement;
    const save = screen.getByRole("button", { name: "Save backup settings" });

    await user.clear(retention);
    expect(retention.value).toBe("");
    await user.type(retention, "4");
    await user.clear(limit);
    expect(limit.value).toBe("");
    await user.click(save);
    expect(submitted).toBeUndefined();
    await user.type(limit, "1.25");
    expect(limit.value).toBe("1.25");
    await user.clear(reserve);
    expect(reserve.value).toBe("");
    await user.type(reserve, "0.1");
    expect(reserve.value).toBe("0.1");
    await user.click(save);
    expect(submitted).toEqual({
      destination: "/backups", retentionCount: 4,
      maxBytes: 1.25 * 1024 ** 3, reserveBytes: Math.round(0.1 * 1024 ** 3),
    });

    await user.clear(limit);
    await user.type(limit, "2.5");
    await act(async () => pending.resolve({ settings: submitted }));
    await screen.findByText("Backup settings saved.");
    expect(limit.value).toBe("2.5");
    expect(reserve.value).toBe("0.1");
    expect(retention.value).toBe("4");
    await user.click(save);
    expect(saveCount).toBe(2);
    expect(limit.value).toBe("2.5");
    expect(reserve.value).toBe("0.1");
  });

  it("preserves exact saved byte limits when only the backup destination changes", async () => {
    const user = userEvent.setup();
    const settings = {
      destination: "/backups", retentionCount: 4,
      maxBytes: 10_000_017, reserveBytes: 1,
    };
    apiJsonMock.mockImplementation(async (path, _schema, init) => {
      if (init?.method === "PUT") return { settings: JSON.parse(String(init.body)) };
      if (path === "/notifications/deliveries") return { deliveries: [] };
      if (path === "/settings/deployment") return deployment;
      if (path === "/settings/backups") return { settings };
      return { configured: false, enabled: false };
    });
    render(<Settings />);
    const destination = await screen.findByLabelText("Mounted destination path");
    await user.clear(destination);
    await user.type(destination, "/new-backups");
    await user.click(screen.getByRole("button", { name: "Save backup settings" }));
    await screen.findByText("Backup settings saved.");
    expect(apiJson).toHaveBeenCalledWith("/settings/backups", expect.anything(), expect.objectContaining({
      method: "PUT", body: JSON.stringify({ ...settings, destination: "/new-backups" }),
    }));
  });

  it("reloads deployment guidance without replacing drafts and validates a chosen root only on Save", async () => {
    const user = userEvent.setup();
    const pending = deferred<unknown>();
    let unavailable = true;
    apiJsonMock.mockImplementation(async (path, _schema, init) => {
      if (path === "/notifications/deliveries") return { deliveries: [] };
      if (path === "/settings/deployment") {
        if (unavailable) throw new Error("Deployment unavailable");
        return pending.promise;
      }
      if (init?.method === "PUT") throw new Error("Destination is not mounted");
      if (path === "/settings/backups") return { settings: null };
      return { configured: false, enabled: false };
    });
    render(<Settings />);
    const destination = await screen.findByLabelText("Mounted destination path") as HTMLInputElement;
    await user.type(destination, "/my-draft");
    unavailable = false;
    await user.click(await screen.findByRole("button", { name: "Reload setup guidance" }));
    await act(async () => pending.resolve({ ...deployment, backupRoots: ["/approved/backups"] }));
    expect(destination.value).toBe("/my-draft");
    await user.click(screen.getByRole("button", { name: "Use /approved/backups" }));
    expect(destination.value).toBe("/approved/backups");
    expect(document.activeElement).toBe(destination);
    expect(apiJsonMock.mock.calls.some(([, , init]) => init?.method === "PUT")).toBe(false);
    await user.click(screen.getByRole("button", { name: "Save backup settings" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Destination is not mounted");
    expect(alert.closest("form")).toBe(destination.closest("form"));
    expect(destination.value).toBe("/approved/backups");
  });

  it("opens server sharing after creating an operator and returns focus when sharing is done", async () => {
    const user = userEvent.setup();
    apiJsonMock.mockImplementation(async (path, _schema, init) => {
      if (path === "/users") return init?.method === "POST" ? { user: member } : { users: [] };
      if (path === "/servers") return { servers: [{ id: "world", displayName: "World" }] };
      return init?.method === "PUT" ? JSON.parse(String(init.body)) : { grants: [] };
    });
    renderUsers();
    await user.type(screen.getByLabelText("Username"), "friend");
    await user.type(screen.getByLabelText("Initial password"), "initial-password-123");
    await user.click(screen.getByRole("button", { name: "Create user", exact: true }));
    const heading = await screen.findByRole("heading", { name: "Server access for friend" });
    expect(document.activeElement).toBe(heading);
    expect((screen.getByLabelText("Initial password") as HTMLInputElement).value).toBe("");
    expect(apiJsonMock.mock.calls.some(([, , init]) => init?.method === "PUT")).toBe(false);
    await user.click(await screen.findByRole("button", { name: "Start and stop" }));
    await user.click(screen.getByRole("button", { name: "Save server access" }));
    await screen.findByText("Server access saved for friend. Access changes take effect immediately.");
    expect(apiJson).toHaveBeenLastCalledWith("/users/member/server-grants", expect.anything(), expect.objectContaining({
      method: "PUT", body: JSON.stringify({ grants: [{ serverId: "world", capabilities: ["server.view", "server.start", "server.stop"] }] }),
    }));
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("heading", { name: "Server access for friend" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Server access", exact: true }));
  });

  it("explains automatic Compose updates without loading or offering project registration", async () => {
    apiJsonMock.mockImplementation(async (path) => {
      if (path === "/notifications/deliveries") return { deliveries: [] };
      if (path === "/settings/deployment") return deployment;
      if (path === "/settings/backups") return { settings: null };
      return { configured: false, enabled: false };
    });
    render(<Settings />);
    await screen.findByRole("heading", { name: "Compose updates" });
    expect(screen.getByText(/No project registration is needed/)).toBeTruthy();
    expect(screen.queryByLabelText("Compose project name")).toBeNull();
    expect(apiJsonMock.mock.calls.some(([path]) => path === "/compose-projects")).toBe(false);
  });

  it("serializes user access writes and uses the returned role for the next change", async () => {
    const pending = deferred<unknown>();
    apiJsonMock.mockImplementation(async (_path, _schema, init) => {
      if (!init?.method) return { users: [member] };
      if (JSON.parse(String(init.body)).disabled) return { user: { ...member, role: "viewer", disabled: true } };
      return pending.promise;
    });
    renderUsers();
    await screen.findByText("friend");
    const role = screen.getAllByRole("combobox")[1];
    fireEvent.change(role, { target: { value: "viewer" } });
    const disable = screen.getByRole("button", { name: "Disable" });
    expect((disable as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(disable);
    expect(apiJsonMock.mock.calls.filter(([, , init]) => init?.method === "PATCH")).toHaveLength(1);
    await act(async () => pending.resolve({ user: { ...member, role: "viewer" } }));
    await waitFor(() => expect((disable as HTMLButtonElement).disabled).toBe(false));
    await userEvent.click(disable);
    expect(apiJson).toHaveBeenLastCalledWith("/users/member", expect.anything(), expect.objectContaining({ body: JSON.stringify({ role: "viewer", disabled: true }) }));
  });

  it("preserves a newer password reset draft and rejects duplicate submissions", async () => {
    const pending = deferred<unknown>();
    apiJsonMock.mockImplementation(async (_path, _schema, init) => init?.method ? pending.promise : { users: [member] });
    renderUsers();
    const input = await screen.findByLabelText("New password for friend");
    fireEvent.change(input, { target: { value: "first-password-123" } });
    const reset = screen.getByRole("button", { name: "Reset password" });
    await userEvent.click(reset);
    await userEvent.click(reset);
    fireEvent.change(input, { target: { value: "second-password-123" } });
    await act(async () => pending.resolve({ ok: true }));
    expect((input as HTMLInputElement).value).toBe("second-password-123");
    expect(apiJsonMock.mock.calls.filter(([, , init]) => init?.method === "POST")).toHaveLength(1);
  });
});
