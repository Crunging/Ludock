import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import {
  SERVER_CAPABILITIES,
  type FileEntry,
  type FileListing,
} from "@ludock/shared";
import { ApiRequestError, apiFetch, apiJson } from "../src/api";
import {
  AuthContext,
  type AuthContextValue,
  type AuthUser,
} from "../src/auth-context";
import { NavigationContext } from "../src/navigation-context";
import Files from "../src/pages/Files";
import type { ManagedContainer } from "../src/types";

vi.mock("../src/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../src/api")>()),
  apiFetch: vi.fn(),
  apiJson: vi.fn(),
}));

const roots = [
  { id: "data", name: "Game data", path: "/data" },
  { id: "mods", name: "Mods", path: "/mods" },
];
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
  fileRoots: roots,
  ports: [],
  created: 0,
  labels: {},
  permissions: [...SERVER_CAPABILITIES],
  bindingStatus: "active",
};
const folder: FileEntry = {
  name: "world",
  type: "directory",
  size: 0,
  modifiedAt: 1,
};
const config: FileEntry = {
  name: "server.properties",
  type: "file",
  size: 100,
  modifiedAt: 1,
};

function folderListing(
  root = "data",
  path = "",
  entries: FileEntry[] = [folder, config],
): FileListing {
  return { root: roots.find((item) => item.id === root)!, path, entries };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function response(body: unknown = { ok: true }, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function filesPage({
  current = server,
  role = "admin",
  read,
  write,
}: {
  current?: ManagedContainer;
  role?: AuthUser["role"];
  read?: (path: string, init?: RequestInit) => unknown | Promise<unknown>;
  write?: (path: string, init?: RequestInit) => Response | Promise<Response>;
} = {}) {
  const navigate = vi.fn();
  const authUser = { id: "user1", username: "friend", role };
  vi.mocked(apiJson).mockImplementation(async (path, _schema, init) => {
    const custom = await read?.(path, init);
    if (custom !== undefined) return custom;
    if (/^\/servers\/[^/]+$/.test(path))
      return { server: current, stats: null };
    const url = new URL(path, "http://localhost");
    return folderListing(
      url.searchParams.get("root") || "data",
      url.searchParams.get("path") || "",
    );
  });
  vi.mocked(apiFetch).mockImplementation(async (path, init) =>
    write ? write(String(path), init) : response(),
  );
  const content = (id: string, user = authUser) => (
    <AuthContext.Provider value={{ user } as AuthContextValue}>
      <NavigationContext.Provider
        value={{ pathname: `/files/${id}`, navigate }}
      >
        <Files containerId={id} />
      </NavigationContext.Provider>
    </AuthContext.Provider>
  );
  const view = render(content(current.id));
  return {
    ...view,
    navigate,
    changeServer: (id: string) => view.rerender(content(id)),
    changeUser: (id: string, nextRole: AuthUser["role"]) =>
      view.rerender(
        content(current.id, { id, username: "other", role: nextRole }),
      ),
  };
}

function fileRow(name: string): HTMLElement {
  const row = screen
    .getByText(name, {
      selector:
        ".file-row__name > span:not(.file-row__icon), .file-row__name > button",
    })
    .closest<HTMLElement>(".file-row");
  if (!row) throw new Error(`Missing file row: ${name}`);
  return row;
}

describe("file locations and request ownership", () => {
  it("ignores an older root response and uses the visible root for actions", async () => {
    const oldRoot = deferred<FileListing>();
    let oldSignal: AbortSignal | undefined;
    filesPage({
      read: (path, init) => {
        if (path.includes("/files?root=data")) {
          oldSignal = init?.signal as AbortSignal;
          return oldRoot.promise;
        }
        if (path.includes("/files?root=mods"))
          return folderListing("mods", "", [{ ...config, name: "mod.jar" }]);
      },
    });
    const rootSelect = await screen.findByRole("combobox", {
      name: "Storage location",
    });
    await userEvent.selectOptions(rootSelect, "mods");
    await screen.findByText("mod.jar");
    expect(oldSignal?.aborted).toBe(true);
    await act(async () =>
      oldRoot.resolve(
        folderListing("data", "", [{ ...config, name: "old.txt" }]),
      ),
    );
    expect(screen.queryByText("old.txt")).toBeNull();
    const row = fileRow("mod.jar");
    expect(
      within(row).getByRole("link", { name: "Download" }).getAttribute("href"),
    ).toContain("root=mods&path=mod.jar");
    await userEvent.click(within(row).getByRole("button", { name: "Rename" }));
    expect(screen.getByRole("dialog").textContent).toContain(
      "Friends world · Mods",
    );
    const input = screen.getByRole("textbox", { name: "New name" });
    await userEvent.clear(input);
    await userEvent.type(input, "new-mod.jar");
    await userEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", {
        name: "Rename",
        exact: true,
      }),
    );
    await waitFor(() =>
      expect(apiFetch).toHaveBeenCalledWith(
        `/api/v1/servers/${server.id}/files/rename`,
        expect.objectContaining({
          method: "PATCH",
          body: JSON.stringify({
            root: "mods",
            path: "mod.jar",
            newName: "new-mod.jar",
          }),
        }),
      ),
    );
  });

  it("removes old rows immediately when entering a folder and keeps a failed load distinct from an empty folder", async () => {
    const nextFolder = deferred<FileListing>();
    let retry = false;
    filesPage({
      read: (path) => {
        if (path.endsWith("path=world"))
          return retry
            ? folderListing("data", "world", [])
            : nextFolder.promise;
      },
    });
    await userEvent.click(
      await screen.findByRole("button", { name: "world", exact: true }),
    );
    expect(screen.queryByText("server.properties")).toBeNull();
    expect(screen.getByRole("status").textContent).toBe("Loading folder…");
    await act(async () =>
      nextFolder.reject(new Error("Storage is temporarily unavailable.")),
    );
    expect((await screen.findByRole("alert")).textContent).toContain(
      "Storage is temporarily unavailable.",
    );
    expect(screen.queryByText("This folder is empty.")).toBeNull();
    expect(
      screen.queryByRole("button", { name: "Delete", exact: true }),
    ).toBeNull();
    retry = true;
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("This folder is empty.")).toBeTruthy();
  });

  it("clears rows and roots when a refresh revokes file access", async () => {
    let forbidden = false;
    filesPage({
      read: (path) => {
        if (forbidden && path.includes("/files?"))
          throw new ApiRequestError("File access was revoked.", 403);
      },
    });
    await screen.findByText("server.properties");
    forbidden = true;
    await userEvent.click(
      screen.getByRole("button", { name: "Refresh", exact: true }),
    );
    expect((await screen.findByRole("alert")).textContent).toContain(
      "File access was revoked.",
    );
    expect(screen.queryByText("server.properties")).toBeNull();
    expect(
      screen.queryByRole("combobox", { name: "Storage location" }),
    ).toBeNull();
    expect(screen.queryByRole("button", { name: "New folder" })).toBeNull();
    expect(screen.queryByText("File access is not configured")).toBeNull();
  });

  it("aborts reads when switching servers and ignores their late results", async () => {
    const oldFolder = deferred<FileListing>();
    const secondServer = {
      ...server,
      id: "997c5e98-7489-42c5-bf94-6e0e2ea3ead8",
      displayName: "Other world",
    };
    let oldSignal: AbortSignal | undefined;
    const view = filesPage({
      read: (path, init) => {
        if (path === `/servers/${secondServer.id}`)
          return { server: secondServer, stats: null };
        if (path.startsWith(`/servers/${server.id}/files?`)) {
          oldSignal = init?.signal as AbortSignal;
          return oldFolder.promise;
        }
        if (path.startsWith(`/servers/${secondServer.id}/files?`))
          return folderListing("data", "", [{ ...config, name: "other.conf" }]);
      },
    });
    await screen.findByRole("combobox", { name: "Storage location" });
    view.changeServer(secondServer.id);
    await screen.findByRole("heading", { name: "Other world" });
    await screen.findByText("other.conf");
    expect(oldSignal?.aborted).toBe(true);
    await act(async () => oldFolder.resolve(folderListing()));
    expect(screen.queryByText("server.properties")).toBeNull();
    expect(
      screen.getByRole("link", { name: "Back to server" }).getAttribute("href"),
    ).toBe(`/servers/${secondServer.id}`);
  });
});

describe("file uploads", () => {
  it("keeps the partial upload error visible after refreshing completed files", async () => {
    let completed = false;
    filesPage({
      read: (path) =>
        path.includes("/files?") && completed
          ? folderListing("data", "", [{ ...config, name: "one.txt" }])
          : undefined,
      write: (path) => {
        if (path.endsWith("name=one.txt")) {
          completed = true;
          return response();
        }
        return response(
          { error: "A file with that name already exists." },
          409,
        );
      },
    });
    await screen.findByText("server.properties");
    await userEvent.upload(screen.getByLabelText("Upload files"), [
      new File(["one"], "one.txt"),
      new File(["two"], "two.txt"),
    ]);
    await screen.findByText("one.txt");
    const error = await screen.findByRole("alert");
    expect(error.textContent).toContain("Uploaded 1 of 2 files.");
    expect(error.textContent).toContain("two.txt");
    expect(error.textContent).toContain(
      "A file with that name already exists.",
    );
    expect(
      (
        screen.getByRole("button", {
          name: "Choose files",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false);
  });

  it("locks navigation and duplicate drops during upload, supports cancellation, and does not retry writes", async () => {
    let uploadSignal: AbortSignal | undefined;
    const { navigate } = filesPage({
      write: (_path, init) =>
        new Promise((_resolve, reject) => {
          uploadSignal = init?.signal as AbortSignal;
          uploadSignal.addEventListener("abort", () =>
            reject(new DOMException("Aborted", "AbortError")),
          );
        }),
    });
    await screen.findByText("server.properties");
    const uploaded = new File(["world"], "world.zip");
    await userEvent.upload(screen.getByLabelText("Upload files"), uploaded);
    expect(
      (
        screen.getByRole("combobox", {
          name: "Storage location",
        }) as HTMLSelectElement
      ).disabled,
    ).toBe(true);
    expect(
      (
        screen.getByRole("button", {
          name: "world",
          exact: true,
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    const rootButton = within(
      screen.getByRole("navigation", { name: "Current folder" }),
    ).getByRole("button", { name: "Game data" });
    expect((rootButton as HTMLButtonElement).disabled).toBe(true);
    const back = screen.getByRole("link", { name: "Back to server" });
    expect(back.getAttribute("aria-disabled")).toBe("true");
    await userEvent.click(back);
    expect(navigate).not.toHaveBeenCalled();
    fireEvent.drop(
      screen.getByText("Drop files here to upload them to this folder"),
      {
        dataTransfer: { files: [new File(["extra"], "extra.zip")] },
      },
    );
    expect(apiFetch).toHaveBeenCalledTimes(1);
    await userEvent.click(
      screen.getByRole("button", { name: "Cancel upload" }),
    );
    await waitFor(() =>
      expect(
        (
          screen.getByRole("button", {
            name: "Choose files",
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false),
    );
    expect(uploadSignal?.aborted).toBe(true);
    expect(screen.getByRole("status").textContent).toContain(
      "Upload canceled.",
    );
    expect(apiFetch).toHaveBeenCalledTimes(1);
  });

  it("ignores a late upload completion after the user changes", async () => {
    const pending = deferred<Response>();
    let uploadSignal: AbortSignal | undefined;
    const view = filesPage({
      write: (_path, init) => {
        uploadSignal = init?.signal as AbortSignal;
        return pending.promise;
      },
    });
    await screen.findByText("server.properties");
    await userEvent.upload(
      screen.getByLabelText("Upload files"),
      new File(["data"], "world.zip"),
    );
    view.changeUser("viewer", "viewer");
    await screen.findByText(
      "Your account can browse and download files but cannot change them.",
    );
    const callsBeforeCompletion = vi.mocked(apiJson).mock.calls.length;
    await act(async () => pending.resolve(response()));
    expect(uploadSignal?.aborted).toBe(true);
    expect(screen.queryByText(/uploaded\.$/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Choose files" })).toBeNull();
    expect(vi.mocked(apiJson).mock.calls.length).toBe(callsBeforeCompletion);
  });
});

describe("contextual file dialogs", () => {
  it("focuses and preserves the new folder draft after errors, then refreshes on success", async () => {
    let attempt = 0;
    const { navigate } = filesPage({
      write: () =>
        ++attempt === 1
          ? response({ error: "A folder with that name already exists." }, 409)
          : response(),
    });
    await screen.findByText("server.properties");
    await userEvent.click(screen.getByRole("button", { name: "New folder" }));
    const dialog = screen.getByRole("dialog", { name: "New folder" });
    const name = within(dialog).getByRole("textbox", { name: "Folder name" });
    expect(document.activeElement).toBe(name);
    expect(dialog.textContent).toContain("Friends world · Game data");
    await userEvent.type(name, "backups");
    await userEvent.click(
      within(dialog).getByRole("button", { name: "Create folder" }),
    );
    expect((await within(dialog).findByRole("alert")).textContent).toContain(
      "already exists",
    );
    expect((name as HTMLInputElement).value).toBe("backups");
    await userEvent.clear(name);
    await userEvent.type(name, "old-backups");
    await userEvent.click(
      within(dialog).getByRole("button", { name: "Create folder" }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByRole("status").textContent).toContain(
      "Created “old-backups”.",
    );
    expect(apiFetch).toHaveBeenLastCalledWith(
      `/api/v1/servers/${server.id}/files/directory`,
      expect.objectContaining({
        body: JSON.stringify({ root: "data", path: "", name: "old-backups" }),
      }),
    );
    await userEvent.click(screen.getByRole("link", { name: "Back to server" }));
    expect(navigate).toHaveBeenCalledWith(`/servers/${server.id}`);
  });

  it("focuses Cancel for deletion, restores its trigger, and disables repeated submissions while pending", async () => {
    const deletion = deferred<Response>();
    filesPage({ write: () => deletion.promise });
    await screen.findByText("server.properties");
    const trigger = within(fileRow("world")).getByRole("button", {
      name: "Delete",
      exact: true,
    });
    await userEvent.click(trigger);
    let dialog = screen.getByRole("dialog", { name: "Delete “world”?" });
    expect(dialog.textContent).toContain("folder and everything inside it");
    expect(document.activeElement).toBe(
      within(dialog).getByRole("button", { name: "Cancel" }),
    );
    await userEvent.keyboard("{Enter}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(trigger);
    expect(apiFetch).not.toHaveBeenCalled();

    await userEvent.click(trigger);
    dialog = screen.getByRole("dialog");
    const confirm = within(dialog).getByRole("button", {
      name: "Delete folder",
    });
    await userEvent.click(confirm);
    expect((confirm as HTMLButtonElement).disabled).toBe(true);
    expect(
      (
        within(dialog).getByRole("button", {
          name: "Cancel",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);
    fireEvent(dialog, new Event("cancel", { cancelable: true }));
    expect(screen.getByRole("dialog")).toBe(dialog);
    await userEvent.click(confirm);
    expect(apiFetch).toHaveBeenCalledTimes(1);
    await act(async () =>
      deletion.resolve(
        response(
          { error: "Server is busy. Try again when the backup finishes." },
          409,
        ),
      ),
    );
    expect((await within(dialog).findByRole("alert")).textContent).toContain(
      "backup finishes",
    );
    expect((confirm as HTMLButtonElement).disabled).toBe(false);
    expect(apiFetch).toHaveBeenCalledWith(
      `/api/v1/servers/${server.id}/files?root=data&path=world`,
      expect.objectContaining({ method: "DELETE" }),
    );
  });

  it("keeps viewer and missing-root states distinct from writable file access", async () => {
    const view = filesPage({
      role: "viewer",
      current: { ...server, permissions: ["server.view", "files.read"] },
    });
    await screen.findByText("server.properties");
    for (const name of ["New folder", "Choose files", "Rename", "Delete"])
      expect(screen.queryByRole("button", { name, exact: true })).toBeNull();
    expect(screen.getAllByRole("link", { name: "Download" })).toHaveLength(2);
    view.unmount();
    filesPage({ current: { ...server, fileRoots: [] } });
    expect(
      await screen.findByRole("heading", {
        name: "File access is not configured",
      }),
    ).toBeTruthy();
    expect(screen.queryByText("This folder is empty.")).toBeNull();
  });
});

describe("uncertain file mutation results", () => {
  it("reconciles a rename whose response was lost and requires a fresh confirmation", async () => {
    let renamed = false;
    const newName = "renamed.properties";
    filesPage({
      read: (path) =>
        renamed && path.includes("/files?")
          ? folderListing("data", "", [folder, { ...config, name: newName }])
          : undefined,
      write: () => {
        renamed = true;
        throw new TypeError("Failed to fetch");
      },
    });
    await screen.findByText(config.name);
    await userEvent.click(within(fileRow(config.name)).getByRole("button", { name: "Rename" }));
    const name = screen.getByRole("textbox", { name: "New name" });
    await userEvent.clear(name);
    await userEvent.type(name, newName);
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Rename", exact: true }));
    await screen.findByText(newName);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByText(config.name)).toBeNull();
    expect(screen.getByRole("alert").textContent).toContain("The change may have completed");
    expect(apiFetch).toHaveBeenCalledTimes(1);

    await userEvent.click(within(fileRow(newName)).getByRole("button", { name: "Rename" }));
    expect((screen.getByRole("textbox", { name: "New name" }) as HTMLInputElement).value).toBe(newName);
    expect((within(screen.getByRole("dialog")).getByRole("button", { name: "Rename", exact: true }) as HTMLButtonElement).disabled).toBe(true);
    expect(apiFetch).toHaveBeenCalledTimes(1);
  });

  it("reconciles a completed deletion with a lost response without retaining its confirmation", async () => {
    let deleted = false;
    filesPage({
      read: (path) =>
        deleted && path.includes("/files?")
          ? folderListing("data", "", [folder])
          : undefined,
      write: () => {
        deleted = true;
        throw new TypeError("Network connection closed");
      },
    });
    await screen.findByText(config.name);
    await userEvent.click(within(fileRow(config.name)).getByRole("button", { name: "Delete" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete file", exact: true }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(screen.queryByText(config.name)).toBeNull());
    expect(screen.getByRole("alert").textContent).toContain("Review the folder before starting another action");
    expect(screen.getByRole("button", { name: "world", exact: true })).toBeTruthy();
    expect(apiFetch).toHaveBeenCalledTimes(1);
  });

  it("treats malformed success as uncertain and restores only the name draft after reopening", async () => {
    filesPage({ write: () => response({ unexpected: true }) });
    await screen.findByText(config.name);
    await userEvent.click(within(fileRow(config.name)).getByRole("button", { name: "Rename" }));
    const name = screen.getByRole("textbox", { name: "New name" });
    await userEvent.clear(name);
    await userEvent.type(name, "edited.properties");
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Rename", exact: true }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await screen.findByText(config.name);
    expect(screen.getByRole("alert").textContent).toContain("could not be confirmed");
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(vi.mocked(apiJson).mock.calls.filter(([path]) => path.includes("/files?"))).toHaveLength(2);

    await userEvent.click(within(fileRow(config.name)).getByRole("button", { name: "Rename" }));
    expect((screen.getByRole("textbox", { name: "New name" }) as HTMLInputElement).value).toBe("edited.properties");
    expect(apiFetch).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole("button", { name: "Cancel", exact: true }));
    await userEvent.click(within(fileRow(folder.name)).getByRole("button", { name: "Rename" }));
    expect((screen.getByRole("textbox", { name: "New name" }) as HTMLInputElement).value).toBe(folder.name);
  });

  it("keeps mutations unavailable if the folder cannot be reconciled after an uncertain create", async () => {
    let attempted = false;
    filesPage({
      read: (path) => {
        if (attempted && path.includes("/files?"))
          throw new Error("Storage is temporarily unavailable.");
      },
      write: () => {
        attempted = true;
        return response({ unexpected: true }, 201);
      },
    });
    await screen.findByText(config.name);
    await userEvent.click(screen.getByRole("button", { name: "New folder" }));
    await userEvent.type(screen.getByRole("textbox", { name: "Folder name" }), "backups");
    await userEvent.click(screen.getByRole("button", { name: "Create folder" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await screen.findByText(/Unable to load this folder/);
    expect((screen.getByRole("button", { name: "New folder" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: "Rename", exact: true })).toBeNull();
    expect(apiFetch).toHaveBeenCalledTimes(1);

    attempted = false;
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    await screen.findByText(config.name);
    await userEvent.click(screen.getByRole("button", { name: "New folder" }));
    expect((screen.getByRole("textbox", { name: "Folder name" }) as HTMLInputElement).value).toBe("backups");
    expect(apiFetch).toHaveBeenCalledTimes(1);
  });
});
