import { act, fireEvent, render as renderComponent, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, mock } from "bun:test";
import { apiJson } from "../src/api";
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
