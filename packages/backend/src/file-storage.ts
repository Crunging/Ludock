import path from "node:path";
import { PassThrough, type Readable } from "node:stream";
import type Docker from "dockerode";
import * as tar from "tar-stream";
import { docker } from "./docker-client.js";
import type { ManagedContainer } from "./docker.js";

export const LABEL_FILES = "game-panel.files";

export interface FileRoot {
  id: string;
  name: string;
  path: string;
}

export interface FileEntry {
  name: string;
  type: "file" | "directory" | "symlink";
  size: number;
  modifiedAt: number;
}

export function getFileRoots(
  server: Pick<ManagedContainer, "gameType" | "image" | "labels">
): FileRoot[] {
  const configured = server.labels[LABEL_FILES];
  const paths = configured
    ? configured.split(",").map((value) => value.trim())
    : inferredFilePaths(server);

  return paths
    .map((value) => {
      const normalized = path.posix.normalize(value);
      return normalized === "/" ? normalized : normalized.replace(/\/+$/, "");
    })
    .filter(
      (value, index, values) =>
        value.startsWith("/") &&
        value !== "/" &&
        value.length <= 512 &&
        values.indexOf(value) === index
    )
    .slice(0, 8)
    .map((rootPath, index) => ({
      id: `root-${index}`,
      name: rootName(server.gameType, rootPath),
      path: rootPath,
    }));
}

export async function listFiles(
  server: ManagedContainer,
  rootId: string,
  relativePath: string
): Promise<{ root: FileRoot; path: string; entries: FileEntry[] }> {
  const target = resolveTarget(server, rootId, relativePath);
  const access = await acquireFileContainer(server, target.root);
  try {
    await assertSafeTarget(
      access.container,
      target.root.path,
      target.absolutePath,
      false
    );
    const result = await runExec(access.container, {
      Cmd: [
        "/bin/sh",
        "-c",
        `test -d "$TARGET" || exit 45
for entry in "$TARGET"/* "$TARGET"/.[!.]* "$TARGET"/..?*; do
  if ! test -e "$entry" && ! test -L "$entry"; then continue; fi
  name=\${entry##*/}
  encoded=$(printf "%s" "$name" | base64 | tr -d '\\n')
  if test -L "$entry"; then
    kind=symlink
    size=0
  elif test -d "$entry"; then
    kind=directory
    size=0
  else
    kind=file
    size=$(stat -c %s "$entry" 2>/dev/null || printf 0)
  fi
  modified=$(stat -c %Y "$entry" 2>/dev/null || printf 0)
  printf "%s\\t%s\\t%s\\t%s\\n" "$encoded" "$kind" "$size" "$modified"
done`,
      ],
      Env: [`TARGET=${target.absolutePath}`],
      AttachStdout: true,
      AttachStderr: true,
    });

    const entries = result.stdout
      .split("\n")
      .filter(Boolean)
      .map((line): FileEntry => {
        const [encoded = "", type = "file", size = "0", modified = "0"] =
          line.split("\t");
        return {
          name: Buffer.from(encoded, "base64").toString("utf8"),
          type: type as FileEntry["type"],
          size: Number(size) || 0,
          modifiedAt: (Number(modified) || 0) * 1000,
        };
      })
      .sort((a, b) => {
        if (a.type === "directory" && b.type !== "directory") return -1;
        if (a.type !== "directory" && b.type === "directory") return 1;
        return a.name.localeCompare(b.name, undefined, { sensitivity: "base" });
      });

    return { root: target.root, path: target.relativePath, entries };
  } finally {
    await access.cleanup();
  }
}

export async function createDirectory(
  server: ManagedContainer,
  rootId: string,
  relativeParent: string,
  name: string
): Promise<void> {
  validateName(name);
  const target = resolveTarget(
    server,
    rootId,
    joinRelative(relativeParent, name)
  );
  const access = await acquireFileContainer(server, target.root);
  try {
    await assertSafeTarget(
      access.container,
      target.root.path,
      target.absolutePath,
      true
    );
    await runExec(access.container, {
      Cmd: ["/bin/sh", "-c", 'mkdir "$TARGET"'],
      Env: [`TARGET=${target.absolutePath}`],
      AttachStdout: true,
      AttachStderr: true,
    });
  } finally {
    await access.cleanup();
  }
}

export async function deleteFileEntry(
  server: ManagedContainer,
  rootId: string,
  relativePath: string
): Promise<void> {
  const target = resolveTarget(server, rootId, relativePath);
  if (!target.relativePath) throw new FileStorageError("ROOT_MUTATION", 400);
  const access = await acquireFileContainer(server, target.root);
  try {
    await assertSafeTarget(
      access.container,
      target.root.path,
      target.absolutePath,
      false,
      true
    );
    await runExec(access.container, {
      Cmd: ["/bin/sh", "-c", 'rm -rf -- "$TARGET"'],
      Env: [`TARGET=${target.absolutePath}`],
      AttachStdout: true,
      AttachStderr: true,
    });
  } finally {
    await access.cleanup();
  }
}

export async function renameFileEntry(
  server: ManagedContainer,
  rootId: string,
  relativePath: string,
  newName: string
): Promise<void> {
  validateName(newName);
  const source = resolveTarget(server, rootId, relativePath);
  if (!source.relativePath) throw new FileStorageError("ROOT_MUTATION", 400);
  const destination = resolveTarget(
    server,
    rootId,
    joinRelative(path.posix.dirname(source.relativePath), newName)
  );
  const access = await acquireFileContainer(server, source.root);
  try {
    await assertSafeTarget(
      access.container,
      source.root.path,
      source.absolutePath,
      false
    );
    await assertSafeTarget(
      access.container,
      destination.root.path,
      destination.absolutePath,
      true
    );
    await runExec(access.container, {
      Cmd: [
        "/bin/sh",
        "-c",
        'test ! -e "$DESTINATION" && test ! -L "$DESTINATION" || exit 46\nmv -- "$SOURCE" "$DESTINATION"',
      ],
      Env: [
        `SOURCE=${source.absolutePath}`,
        `DESTINATION=${destination.absolutePath}`,
      ],
      AttachStdout: true,
      AttachStderr: true,
    });
  } finally {
    await access.cleanup();
  }
}

export async function uploadFile(
  server: ManagedContainer,
  rootId: string,
  relativeParent: string,
  name: string,
  size: number,
  source: Readable
): Promise<void> {
  validateName(name);
  const parent = resolveTarget(server, rootId, relativeParent);
  const destination = resolveTarget(
    server,
    rootId,
    joinRelative(relativeParent, name)
  );
  const access = await acquireFileContainer(server, parent.root);
  try {
    await assertSafeTarget(
      access.container,
      parent.root.path,
      parent.absolutePath,
      false
    );
    await assertSafeTarget(
      access.container,
      destination.root.path,
      destination.absolutePath,
      true
    );

    const archive = tar.pack();
    const entry = archive.entry({ name, size, mode: 0o644 });
    source.pipe(entry);
    source.once("error", (error) => archive.destroy(error));
    entry.once("finish", () => archive.finalize());
    await access.container.putArchive(archive, { path: parent.absolutePath });
  } finally {
    await access.cleanup();
  }
}

export async function openDownload(
  server: ManagedContainer,
  rootId: string,
  relativePath: string
): Promise<{
  name: string;
  type: "file" | "directory";
  size: number;
  stream: NodeJS.ReadableStream;
}> {
  const target = resolveTarget(server, rootId, relativePath);
  if (!target.relativePath) throw new FileStorageError("ROOT_DOWNLOAD", 400);
  const access = await acquireFileContainer(server, target.root);
  try {
    await assertSafeTarget(
      access.container,
      target.root.path,
      target.absolutePath,
      false
    );
    const stat = await statTarget(access.container, target.absolutePath);
    const archive = await access.container.getArchive({
      path: target.absolutePath,
    });
    const name = path.posix.basename(target.relativePath);
    if (stat.type === "directory") {
      cleanupAfterStream(archive, access.cleanup);
      return {
        name: `${name}.tar`,
        type: "directory",
        size: 0,
        stream: archive,
      };
    }

    const extract = tar.extract();
    const output = new PassThrough();
    let found = false;
    extract.on("entry", (_header, entryStream, next) => {
      if (!found) {
        found = true;
        entryStream.pipe(output, { end: false });
        entryStream.on("end", next);
      } else {
        entryStream.resume();
        entryStream.on("end", next);
      }
    });
    extract.on("finish", () => output.end());
    extract.on("error", (error) => output.destroy(error));
    archive.on("error", (error) => output.destroy(error));
    archive.pipe(extract);
    cleanupAfterStream(output, access.cleanup);
    return { name, type: "file", size: stat.size, stream: output };
  } catch (error) {
    await access.cleanup();
    throw error;
  }
}

interface FileContainerAccess {
  container: Docker.Container;
  cleanup: () => Promise<void>;
}

async function acquireFileContainer(
  server: ManagedContainer,
  root: FileRoot
): Promise<FileContainerAccess> {
  const target = docker.getContainer(server.id);
  if (server.state === "running") {
    return { container: target, cleanup: async () => {} };
  }

  const inspection = await target.inspect();
  const rootPath = path.posix.normalize(root.path);
  const volumeBacked = (inspection.Mounts || []).some((mount) => {
    const destination = path.posix.normalize(mount.Destination);
    return (
      rootPath === destination || rootPath.startsWith(`${destination}/`)
    );
  });
  if (!volumeBacked) {
    throw new FileStorageError("OFFLINE_ROOT_NOT_VOLUME", 409);
  }

  const helperImage = process.env.FILE_HELPER_IMAGE || "alpine:3.22";
  const helper = await createHelperContainer(helperImage, server.id);
  let removed = false;
  const cleanup = async () => {
    if (removed) return;
    removed = true;
    try {
      await helper.remove({ force: true });
    } catch (error) {
      const statusCode = (error as { statusCode?: number }).statusCode;
      if (statusCode !== 404) {
        console.error("[Files] Failed to remove offline helper:", error);
      }
    }
  };

  try {
    await helper.start();
    return { container: helper, cleanup };
  } catch (error) {
    await cleanup();
    throw error;
  }
}

async function createHelperContainer(
  image: string,
  sourceContainerId: string
): Promise<Docker.Container> {
  const options: Docker.ContainerCreateOptions = {
    Image: image,
    Entrypoint: ["/bin/sh", "-c"],
    Cmd: ["while :; do sleep 3600; done"],
    User: "0",
    Labels: { "game-panel.internal": "file-helper" },
    HostConfig: {
      VolumesFrom: [`${sourceContainerId}:rw`],
      NetworkMode: "none",
      ReadonlyRootfs: true,
      CapDrop: ["ALL"],
      SecurityOpt: ["no-new-privileges"],
    },
  };

  try {
    return await docker.createContainer(options);
  } catch (error) {
    if ((error as { statusCode?: number }).statusCode !== 404) throw error;
    await pullImage(image);
    return docker.createContainer(options);
  }
}

async function pullImage(image: string): Promise<void> {
  const stream = await docker.pull(image);
  await new Promise<void>((resolve, reject) => {
    docker.modem.followProgress(stream, (error) =>
      error ? reject(error) : resolve()
    );
  });
}

function cleanupAfterStream(
  stream: NodeJS.ReadableStream,
  cleanup: () => Promise<void>
): void {
  let cleaned = false;
  const run = () => {
    if (cleaned) return;
    cleaned = true;
    void cleanup();
  };
  stream.once("end", run);
  stream.once("close", run);
  stream.once("error", run);
}

function inferredFilePaths(
  server: Pick<ManagedContainer, "gameType" | "image">
): string[] {
  const game = server.gameType.trim().toLowerCase();
  const image = server.image.toLowerCase();
  if (game === "minecraft") return ["/data"];
  if (
    game === "valheim" &&
    (image.includes("community-valheim-tools/valheim-server") ||
      image.includes("lloesche/valheim-server"))
  ) {
    return ["/config"];
  }
  return [];
}

function rootName(gameType: string, rootPath: string): string {
  if (gameType.toLowerCase() === "minecraft" && rootPath === "/data") {
    return "Minecraft data";
  }
  if (gameType.toLowerCase() === "valheim" && rootPath === "/config") {
    return "Valheim config";
  }
  return path.posix.basename(rootPath) || rootPath;
}

function resolveTarget(
  server: ManagedContainer,
  rootId: string,
  relativePath: string
): { root: FileRoot; relativePath: string; absolutePath: string } {
  const root = getFileRoots(server).find((candidate) => candidate.id === rootId);
  if (!root) throw new FileStorageError("ROOT_NOT_FOUND", 404);
  const normalized = normalizeRelativePath(relativePath);
  return {
    root,
    relativePath: normalized,
    absolutePath: normalized
      ? path.posix.join(root.path, normalized)
      : root.path,
  };
}

export function normalizeRelativePath(value: string): string {
  if (
    value.includes("\0") ||
    value.startsWith("/") ||
    value.split("/").some((part) => part === "..")
  ) {
    throw new FileStorageError("INVALID_PATH", 400);
  }
  const normalized = path.posix.normalize(value || ".").replace(/^\.$/, "");
  if (normalized === ".." || normalized.startsWith("../")) {
    throw new FileStorageError("INVALID_PATH", 400);
  }
  return normalized;
}

function joinRelative(parent: string, name: string): string {
  return parent ? path.posix.join(parent, name) : name;
}

function validateName(name: string): void {
  if (
    !name ||
    name.length > 255 ||
    name === "." ||
    name === ".." ||
    name.includes("/") ||
    name.trim() !== name ||
    // Rejecting control characters is the point of this check: they are
    // illegal in paths and can forge terminal output in logs.
    // eslint-disable-next-line no-control-regex
    /[\x00-\x1f\x7f]/.test(name)
  ) {
    throw new FileStorageError("INVALID_NAME", 400);
  }
}

async function assertSafeTarget(
  container: Docker.Container,
  root: string,
  target: string,
  allowMissing: boolean,
  allowFinalSymlink = false
): Promise<void> {
  await runExec(container, {
    Cmd: [
      "/bin/sh",
      "-c",
      `root_resolved=$(readlink -f "$ROOT") || exit 40
if test -L "$TARGET"; then
  test "$ALLOW_SYMLINK" = 1 || exit 44
  parent_resolved=$(readlink -f "$(dirname "$TARGET")") || exit 41
  target_resolved="$parent_resolved/$(basename "$TARGET")"
elif test -e "$TARGET"; then
  target_resolved=$(readlink -f "$TARGET") || exit 41
else
  test "$ALLOW_MISSING" = 1 || exit 42
  parent_resolved=$(readlink -f "$(dirname "$TARGET")") || exit 42
  target_resolved="$parent_resolved/$(basename "$TARGET")"
fi
case "$target_resolved" in
  "$root_resolved"|"$root_resolved"/*) exit 0 ;;
  *) exit 43 ;;
esac`,
    ],
    Env: [
      `ROOT=${root}`,
      `TARGET=${target}`,
      `ALLOW_MISSING=${allowMissing ? "1" : "0"}`,
      `ALLOW_SYMLINK=${allowFinalSymlink ? "1" : "0"}`,
    ],
    AttachStdout: true,
    AttachStderr: true,
  });
}

async function statTarget(
  container: Docker.Container,
  target: string
): Promise<{ type: "file" | "directory"; size: number }> {
  const result = await runExec(container, {
    Cmd: [
      "/bin/sh",
      "-c",
      'if test -d "$TARGET"; then printf "directory\\t0"; else printf "file\\t%s" "$(stat -c %s "$TARGET")"; fi',
    ],
    Env: [`TARGET=${target}`],
    AttachStdout: true,
    AttachStderr: true,
  });
  const [type, size] = result.stdout.split("\t");
  return {
    type: type === "directory" ? "directory" : "file",
    size: Number(size) || 0,
  };
}

async function runExec(
  container: Docker.Container,
  options: Docker.ExecCreateOptions
): Promise<{ stdout: string; stderr: string }> {
  const exec = await container.exec(options);
  const stream = await exec.start({ hijack: true, stdin: false });
  const stdout = new PassThrough();
  const stderr = new PassThrough();
  const stdoutChunks: Buffer[] = [];
  const stderrChunks: Buffer[] = [];
  stdout.on("data", (chunk: Buffer) => stdoutChunks.push(chunk));
  stderr.on("data", (chunk: Buffer) => stderrChunks.push(chunk));
  docker.modem.demuxStream(stream, stdout, stderr);
  await new Promise<void>((resolve, reject) => {
    stream.once("end", resolve);
    stream.once("error", reject);
  });
  const inspection = await exec.inspect();
  const result = {
    stdout: Buffer.concat(stdoutChunks).toString("utf8"),
    stderr: Buffer.concat(stderrChunks).toString("utf8"),
  };
  if (inspection.ExitCode !== 0) {
    throw new FileStorageError(
      `CONTAINER_FILE_OPERATION_${inspection.ExitCode}`,
      inspection.ExitCode === 42 || inspection.ExitCode === 45 ? 404 : 400,
      result.stderr
    );
  }
  return result;
}

export class FileStorageError extends Error {
  constructor(
    public readonly code: string,
    public readonly statusCode: number,
    details?: string
  ) {
    super(details || code);
  }
}
