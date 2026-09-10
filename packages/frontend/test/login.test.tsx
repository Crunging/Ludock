import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, mock } from "bun:test";
import { AuthContext, type AuthContextValue } from "../src/auth-context";
import Login from "../src/pages/Login";

function loginPage(overrides: Partial<AuthContextValue> = {}) {
  const auth = {
    loading: false,
    statusError: false,
    setupRequired: true,
    setupLocked: false,
    authenticated: false,
    user: null,
    refreshStatus: mock().mockResolvedValue(undefined),
    login: mock().mockResolvedValue(null),
    setup: mock().mockResolvedValue(null),
    logout: mock().mockResolvedValue(undefined),
    ...overrides,
  };
  const content = () => <AuthContext.Provider value={auth}><Login /></AuthContext.Provider>;
  const result = render(content());
  return { auth, update: (values: Partial<AuthContextValue>) => {
    Object.assign(auth, values);
    result.rerender(content());
  } };
}

describe("first sign-in", () => {
  it("explains setup requirements and lets users check both password entries", async () => {
    const { auth } = loginPage();
    const username = screen.getByLabelText("Username");
    const password = screen.getByLabelText("Password", { exact: true });
    const confirmation = screen.getByLabelText("Confirm password");
    expect(username.getAttribute("aria-describedby")).toBe("username-hint");
    expect(screen.getByText(/3–32 characters/)).toBeTruthy();
    expect(screen.getByText(/at least 15 characters/)).toBeTruthy();
    await userEvent.type(username, "invalid name");
    await userEvent.type(password, "three friendly words");
    await userEvent.type(confirmation, "three friendly words");
    await userEvent.click(screen.getByRole("button", { name: "Create administrator" }));
    expect((username as HTMLInputElement).validity.patternMismatch).toBe(true);
    expect(auth.setup).not.toHaveBeenCalled();
    await userEvent.clear(username);
    await userEvent.type(username, "admin-test_user");
    expect((username as HTMLInputElement).validity.patternMismatch).toBe(false);
    await userEvent.click(screen.getByRole("checkbox", { name: "Show passwords" }));
    expect(password.getAttribute("type")).toBe("text");
    expect(confirmation.getAttribute("type")).toBe("text");
    await userEvent.click(screen.getByRole("checkbox", { name: "Show passwords" }));
    expect(password.getAttribute("type")).toBe("password");
    expect(confirmation.getAttribute("type")).toBe("password");
    await userEvent.click(screen.getByRole("button", { name: "Create administrator" }));
    expect(auth.setup).toHaveBeenCalledWith("admin-test_user", "three friendly words");
    expect(auth.login).not.toHaveBeenCalled();
  });

  it("focuses mismatched confirmation and clears its error when corrected", async () => {
    const { auth } = loginPage();
    await userEvent.type(screen.getByLabelText("Username"), "admin");
    await userEvent.type(screen.getByLabelText("Password", { exact: true }), "three friendly words");
    const confirmation = screen.getByLabelText("Confirm password");
    await userEvent.type(confirmation, "three different words");
    await userEvent.click(screen.getByRole("button", { name: "Create administrator" }));
    expect(auth.setup).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent).toBe("Passwords do not match.");
    expect(document.activeElement).toBe(confirmation);
    expect(confirmation.getAttribute("aria-invalid")).toBe("true");
    await userEvent.clear(confirmation);
    await userEvent.type(confirmation, "three friendly words");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(confirmation.hasAttribute("aria-invalid")).toBe(false);
    await userEvent.click(screen.getByRole("button", { name: "Create administrator" }));
    expect(auth.setup).toHaveBeenCalledTimes(1);
  });

  it("keeps expired setup locked while checking the server after a restart", async () => {
    let finish!: () => void;
    const refreshStatus = mock().mockImplementation(() => new Promise<void>((resolve) => { finish = resolve; }));
    const { auth, update } = loginPage({ setupLocked: true, refreshStatus });
    expect(screen.queryByRole("textbox")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Check again" }));
    expect(refreshStatus).toHaveBeenCalledTimes(1);
    expect((screen.getByRole("button", { name: "Checking…" }) as HTMLButtonElement).disabled).toBe(true);
    expect(auth.setup).not.toHaveBeenCalled();
    await act(async () => finish());
    expect(screen.getByRole("heading", { name: "Setup window expired" })).toBeTruthy();
    update({ setupLocked: false });
    expect(screen.getByRole("button", { name: "Create administrator" })).toBeTruthy();
  });

  it("shows account help and preserves normal sign-in without setup controls", async () => {
    const { auth } = loginPage({ setupRequired: false });
    expect(screen.getByText(/Need an account or a password reset/)).toBeTruthy();
    expect(screen.queryByLabelText("Confirm password")).toBeNull();
    expect(screen.queryByText(/3–32 characters/)).toBeNull();
    expect(screen.getByLabelText("Username").hasAttribute("pattern")).toBe(false);
    await userEvent.type(screen.getByLabelText("Username"), "friend");
    const password = screen.getByLabelText("Password", { exact: true });
    await userEvent.type(password, "three friendly words");
    await userEvent.click(screen.getByRole("checkbox", { name: "Show password" }));
    expect(password.getAttribute("type")).toBe("text");
    await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(auth.login).toHaveBeenCalledWith("friend", "three friendly words");
    expect(auth.setup).not.toHaveBeenCalled();
  });
});
