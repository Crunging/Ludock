import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, mock } from "bun:test";
import {
  SERVER_CAPABILITIES,
  type FileEntry,
  type FileListing,
  type Server,
} from "@ludock/shared";
import { ApiRequestError, apiFetch, apiJson } from "../src/api";
import type { AuthUser } from "../src/auth-context";
import Files from "../src/pages/Files";
import TestProviders from "./TestProviders";
import { serverFixture } from "./fixtures";

const originalApi = { ...await import("../src/api") };
const apiFetchMock = mock<typeof apiFetch>();
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({
  ...originalApi,
  apiFetch: apiFetchMock,
  apiJson: apiJsonMock,
}));

const roots = [
  { id: "data", name: "Game data", path: "/data" },
  { id: "mods", name: "Mods", path: "/mods" },
];
const server = serverFixture({
  fileRoots: roots,
  permissions: [...SERVER_CAPABILITIES],
});
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
  current?: Server;
  role?: AuthUser["role"];
  read?: (path: string, init?: RequestInit) => unknown | Promise<unknown>;
  write?: (path: string, init?: RequestInit) => Response | Promise<Response>;
} = {}) {
  const navigate = mock();
  const authUser = { id: "user1", username: "friend", role };
  apiJsonMock.mockImplementation(async (path, _schema, init) => {
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
  apiFetchMock.mockImplementation(async (path, init) =>
    write ? write(String(path), init) : response(),
  );
  const content = (id: string, user = authUser) => (
    <TestProviders user={user} pathname={`/files/${id}`} navigate={navigate}>
      <Files containerId={id} />
    </TestProviders>
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

function visibleFilenames(): string[] {
  return Array.from(document.querySelectorAll(
    ".file-row__name > span:not(.file-row__icon), .file-row__name > button",
  ), (element) => element.textContent || "");
}

describe("current-folder file filtering and sorting", () => {
  it("filters filenames without new requests and distinguishes no matches from an empty folder", async () => {
    let empty = false;
    filesPage({
      role: "viewer",
      current: { ...server, permissions: ["server.view", "files.read"] },
      read: (path) => empty && path.includes("/files?") ? folderListing("data", "", []) : undefined,
    });
    await screen.findByText(config.name);
    const requestCount = apiJsonMock.mock.calls.length;
    const search = screen.getByRole("searchbox", { name: "Filter filenames" });
    await userEvent.type(search, "PROPERTIES");
    expect(visibleFilenames()).toEqual([config.name]);
    expect(screen.getByText("Showing 1 of 2 entries")).toBeTruthy();
    expect(apiJsonMock.mock.calls).toHaveLength(requestCount);
    expect(screen.queryByRole("button", { name: "Rename" })).toBeNull();

    await userEvent.clear(search);
    await userEvent.type(search, "level.dat");
    expect(screen.getByText("No filenames match “level.dat”.")).toBeTruthy();
    expect(screen.queryByText("This folder is empty.")).toBeNull();
    expect(screen.getByText("Showing 0 of 2 entries")).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Clear filter" }));
    expect(document.activeElement).toBe(search);
    expect(visibleFilenames()).toEqual([folder.name, config.name]);

    await userEvent.type(search, "missing");
    empty = true;
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByText("This folder is empty.");
    expect(screen.queryByText(/No filenames match/)).toBeNull();
    expect(screen.getByText("Showing 0 of 0 entries")).toBeTruthy();
  });

  it("sorts both directions with folders first, stable name ties, and unavailable values last", async () => {
    const entries: FileEntry[] = [
      { name: "Zeta", type: "directory", size: 0, modifiedAt: 100 },
      { name: "alpha", type: "directory", size: 0, modifiedAt: 300 },
      { name: "unknown.txt", type: "file", size: 0, modifiedAt: 0 },
      { name: "log10", type: "file", size: 10, modifiedAt: 100 },
      { name: "Log2", type: "file", size: 20, modifiedAt: 300 },
      { name: "equal.txt", type: "file", size: 20, modifiedAt: 300 },
      { name: "link", type: "symlink", size: 999, modifiedAt: 200 },
    ];
    filesPage({ read: (path) => path.includes("/files?") ? folderListing("data", "", entries) : undefined });
    await screen.findByText("equal.txt");
    const orders = {
      "name-asc": ["alpha", "Zeta", "equal.txt", "link", "Log2", "log10", "unknown.txt"],
      "name-desc": ["Zeta", "alpha", "unknown.txt", "log10", "Log2", "link", "equal.txt"],
      "size-asc": ["alpha", "Zeta", "unknown.txt", "log10", "equal.txt", "Log2", "link"],
      "size-desc": ["alpha", "Zeta", "equal.txt", "Log2", "log10", "unknown.txt", "link"],
      "modified-asc": ["Zeta", "alpha", "log10", "link", "equal.txt", "Log2", "unknown.txt"],
      "modified-desc": ["alpha", "Zeta", "equal.txt", "Log2", "link", "log10", "unknown.txt"],
    };
    for (const [sort, expected] of Object.entries(orders)) {
      await userEvent.selectOptions(screen.getByRole("combobox", { name: "Sort by" }), sort);
      expect(visibleFilenames()).toEqual(expected);
    }
    expect(entries.map((entry) => entry.name)).toEqual([
      "Zeta", "alpha", "unknown.txt", "log10", "Log2", "equal.txt", "link",
    ]);
  });

  it("keeps browsing controls through refresh and file changes, then clears only the query on folder navigation", async () => {
    filesPage({
      read: (path) => path.includes("path=world")
        ? folderListing("data", "world", [{ ...config, name: "level.dat" }])
        : undefined,
    });
    await screen.findByText(config.name);
    const search = screen.getByRole("searchbox", { name: "Filter filenames" });
    const sort = screen.getByRole("combobox", { name: "Sort by" });
    await userEvent.type(search, "world");
    await userEvent.selectOptions(sort, "modified-desc");
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByRole("button", { name: "world", exact: true });
    expect((search as HTMLInputElement).value).toBe("world");
    expect((sort as HTMLSelectElement).value).toBe("modified-desc");

    await userEvent.click(screen.getByRole("button", { name: "New folder" }));
    await userEvent.type(screen.getByRole("textbox", { name: "Folder name" }), "new folder");
    await userEvent.click(screen.getByRole("button", { name: "Create folder" }));
    await screen.findByText("Created “new folder”.");
    await screen.findByRole("button", { name: "world", exact: true });
    expect((search as HTMLInputElement).value).toBe("world");
    expect((sort as HTMLSelectElement).value).toBe("modified-desc");
    await userEvent.click(screen.getByRole("button", { name: "world", exact: true }));
    await screen.findByText("level.dat");
    expect((search as HTMLInputElement).value).toBe("");
    expect((sort as HTMLSelectElement).value).toBe("modified-desc");
    await userEvent.type(search, "level");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Storage location" }), "mods");
    await screen.findByText(config.name);
    expect((search as HTMLInputElement).value).toBe("");
    expect((sort as HTMLSelectElement).value).toBe("modified-desc");
  });

  it("shows loading and refresh errors instead of filter results until a new listing succeeds", async () => {
    const retry = Promise.withResolvers<FileListing>();
    let attempts = 0;
    filesPage({
      read: (path) => {
        if (!path.includes("/files?")) return;
        attempts += 1;
        if (attempts === 2) throw new Error("Storage unavailable");
        if (attempts === 3) return retry.promise;
      },
    });
    await screen.findByText(config.name);
    const search = screen.getByRole("searchbox", { name: "Filter filenames" });
    await userEvent.type(search, "server");
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByText("Unable to load this folder: Storage unavailable");
    expect(screen.queryByText(/Showing \d+ of/)).toBeNull();
    expect(screen.queryByText(/No filenames match/)).toBeNull();
    expect(visibleFilenames()).toEqual([]);
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    await screen.findByText("Loading folder…");
    await userEvent.clear(search);
    await userEvent.type(search, "world");
    await act(async () => retry.resolve(folderListing()));
    expect(visibleFilenames()).toEqual([folder.name]);
    expect(screen.getByText("Showing 1 of 2 entries")).toBeTruthy();
  });

  it("resets query and sort when switching accounts or servers", async () => {
    const view = filesPage();
    await screen.findByText(config.name);
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter filenames" }), "private query");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Sort by" }), "size-desc");
    view.changeUser("other-admin", "admin");
    await screen.findByText(config.name);
    expect((screen.getByRole("searchbox", { name: "Filter filenames" }) as HTMLInputElement).value).toBe("");
    expect((screen.getByRole("combobox", { name: "Sort by" }) as HTMLSelectElement).value).toBe("name-asc");
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter filenames" }), "another query");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Sort by" }), "size-desc");
    view.changeServer("another-server");
    await screen.findByText(config.name);
    expect((screen.getByRole("searchbox", { name: "Filter filenames" }) as HTMLInputElement).value).toBe("");
    expect((screen.getByRole("combobox", { name: "Sort by" }) as HTMLSelectElement).value).toBe("name-asc");
  });
});

describe("file locations and request ownership", () => {
  it("ignores an older root response and uses the visible root for actions", async () => {
    const oldRoot = Promise.withResolvers<FileListing>();
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
    const nextFolder = Promise.withResolvers<FileListing>();
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
    const oldFolder = Promise.withResolvers<FileListing>();
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
    expect(screen.getByText(/Upload canceled\./).closest('[role="status"]')).toBeTruthy();
    expect(apiFetch).toHaveBeenCalledTimes(1);
  });

  it("ignores a late upload completion after the user changes", async () => {
    const pending = Promise.withResolvers<Response>();
    let uploadSignal: AbortSignal | undefined;
    let viewerActive = false;
    const viewerFilename = "viewer-world.zip";
    const view = filesPage({
      read: (path) => viewerActive && path.includes("/files?")
        ? folderListing("data", "", [{ ...config, name: viewerFilename }])
        : undefined,
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
    viewerActive = true;
    view.changeUser("viewer", "viewer");
    // The read-only notice can render before the new account's listing arrives.
    await screen.findByText(viewerFilename);
    await screen.findByText(
      "Your account can browse and download files but cannot change them.",
    );
    const callsBeforeCompletion = apiJsonMock.mock.calls.length;
    await act(async () => pending.resolve(response()));
    expect(uploadSignal?.aborted).toBe(true);
    expect(screen.queryByText(/uploaded\.$/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Choose files" })).toBeNull();
    expect(screen.getByText(viewerFilename)).toBeTruthy();
    expect(apiJsonMock.mock.calls.length).toBe(callsBeforeCompletion);
  });
});

describe("contextual file dialogs", () => {
  it("keeps the new account's pending action when the previous account's write completes", async () => {
    const previousWrite = Promise.withResolvers<Response>();
    const currentWrite = Promise.withResolvers<Response>();
    let previousSignal: AbortSignal | undefined;
    let writes = 0;
    const view = filesPage({
      write: (_path, init) => {
        if (++writes === 1) {
          previousSignal = init?.signal as AbortSignal;
          return previousWrite.promise;
        }
        return currentWrite.promise;
      },
    });
    await screen.findByText(config.name);
    await userEvent.click(within(fileRow(config.name)).getByRole("button", { name: "Delete" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete file" }));

    view.changeUser("other-admin", "admin");
    await screen.findByText(config.name);
    expect(previousSignal?.aborted).toBe(true);
    expect(screen.queryByRole("dialog")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "New folder" }));
    await userEvent.type(screen.getByRole("textbox", { name: "Folder name" }), "new-account-folder");
    await userEvent.click(screen.getByRole("button", { name: "Create folder" }));
    const dialog = screen.getByRole("dialog");
    const readsBeforeCompletion = apiJsonMock.mock.calls.length;
    await act(async () => previousWrite.resolve(response()));

    expect(screen.getByRole("dialog")).toBe(dialog);
    expect((within(dialog).getByRole("button", { name: "Create folder" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByText(`Deleted “${config.name}”.`)).toBeNull();
    expect(apiJsonMock.mock.calls.length).toBe(readsBeforeCompletion);
    await act(async () => currentWrite.resolve(response()));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByText("Created “new-account-folder”.").closest('[role="status"]')).toBeTruthy();
    expect(apiFetch).toHaveBeenCalledTimes(2);
  });

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
    expect(screen.getByText("Created “old-backups”.").closest('[role="status"]')).toBeTruthy();
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
    const deletion = Promise.withResolvers<Response>();
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
  it("discards the previous account's recovery notice and saved name draft", async () => {
    const view = filesPage({ write: () => response({ unexpected: true }) });
    await screen.findByText(config.name);
    await userEvent.click(screen.getByRole("button", { name: "New folder" }));
    await userEvent.type(screen.getByRole("textbox", { name: "Folder name" }), "private-folder-draft");
    await userEvent.click(screen.getByRole("button", { name: "Create folder" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await screen.findByText(/The change may have completed/);

    view.changeUser("other-admin", "admin");
    await screen.findByText(config.name);
    expect(screen.queryByRole("alert")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "New folder" }));
    expect((screen.getByRole("textbox", { name: "Folder name" }) as HTMLInputElement).value).toBe("");
    expect(apiFetch).toHaveBeenCalledTimes(1);
  });

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
    expect(apiJsonMock.mock.calls.filter(([path]) => path.includes("/files?"))).toHaveLength(2);

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
