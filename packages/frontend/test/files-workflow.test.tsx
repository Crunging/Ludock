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
import { apiFetch, apiJson } from "../src/api";
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
      <Files serverId={id} />
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

describe("file preview", () => {
  it("shows a small text file", async () => {
    filesPage({
      write: (path) => path.includes("/files/download?")
        ? new Response("motd=Friends world\nmax-players=10\n")
        : response(),
    });
    await userEvent.click(await screen.findByRole("button", { name: config.name, exact: true }));
    const contents = await screen.findByLabelText(`Contents of ${config.name}`);
    expect(contents.textContent).toContain("max-players=10");
  });

  it("stops reading a file that grew past the preview limit after it was listed", async () => {
    let pulls = 0;
    let canceled = false;
    const chunk = new Uint8Array(64 * 1024).fill(97);
    filesPage({
      write: (path) => path.includes("/files/download?")
        ? new Response(new ReadableStream<Uint8Array>({
            pull(controller) {
              pulls += 1;
              controller.enqueue(chunk);
            },
            cancel() {
              canceled = true;
            },
          }))
        : response(),
    });
    await userEvent.click(await screen.findByRole("button", { name: config.name, exact: true }));
    await screen.findByText(/aren’t shown here/);
    expect(canceled).toBe(true);
    // 512 KiB is eight chunks; a ninth crosses the limit, plus at most a little read-ahead.
    expect(pulls).toBeLessThan(12);
  });
});

describe("file mutation safety", () => {
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
    await userEvent.click(within(row).getByRole("button", { name: "Actions for mod.jar" }));
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
    await userEvent.click(within(fileRow(config.name)).getByRole("button", { name: `Actions for ${config.name}` }));
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
    await userEvent.click(within(fileRow(config.name)).getByRole("button", { name: `Actions for ${config.name}` }));
    await userEvent.click(within(fileRow(config.name)).getByRole("button", { name: "Delete" }));
    await userEvent.click(screen.getByRole("button", { name: "Delete file", exact: true }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(screen.queryByText(config.name)).toBeNull());
    expect(screen.getByRole("alert").textContent).toContain("Review the folder before starting another action");
    expect(screen.getByRole("button", { name: "world", exact: true })).toBeTruthy();
    expect(apiFetch).toHaveBeenCalledTimes(1);
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
