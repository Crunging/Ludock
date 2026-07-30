import { Router } from "express";
import type { Request, Response, Router as RouterType } from "express";
import { z } from "zod";
import { requireRole } from "./auth.js";
import { writeAuditLog, type SessionUser } from "./database.js";
import {
  getManagedContainer,
  getContainerStats,
  listManagedContainers,
  restartContainer,
  startContainer,
  stopContainer,
} from "./docker.js";
import {
  createDirectory,
  deleteFileEntry,
  FileStorageError,
  listFiles,
  openDownload,
  renameFileEntry,
  uploadFile,
} from "./file-storage.js";
import { createLogger, errorMessage } from "./logger.js";

export const router: RouterType = Router();
const logger = createLogger("api");

interface DockerRouteError extends Error {
  statusCode?: number;
  code?: string;
}

function sendDockerError(
  res: Response,
  caught: unknown,
  fallbackMessage: string
): void {
  const error = caught as DockerRouteError;

  if (error.statusCode === 304) {
    res.json({ ok: true });
    return;
  }

  if (error.code === "INVALID_CONTAINER_ID") {
    res.status(400).json({ error: "Invalid container identifier" });
    return;
  }

  if (error.statusCode === 403 || error.code === "FORBIDDEN") {
    res.status(403).json({ error: "Container is not managed by Ludock" });
    return;
  }

  if (error.statusCode === 404) {
    res.status(404).json({ error: "Container not found" });
    return;
  }

  logger.error(fallbackMessage, { error: errorMessage(caught) });
  res.status(500).json({ error: fallbackMessage });
}

function auditContainerAction(
  req: Request,
  res: Response,
  action: string,
  containerId: string
): void {
  const user = res.locals.user as SessionUser;
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action: `container.${action}`,
    targetType: "container",
    targetId: containerId,
    ipAddress: req.ip,
  });
}

const fileLocationSchema = z.object({
  root: z.string().min(1).max(64),
  path: z.string().max(2048).default(""),
});

const directorySchema = fileLocationSchema.extend({
  name: z.string().min(1).max(255),
});

const renameSchema = fileLocationSchema.extend({
  newName: z.string().min(1).max(255),
});

const uploadQuerySchema = fileLocationSchema.extend({
  name: z.string().min(1).max(255),
});

const maxUploadBytes = Math.max(
  1,
  Number(process.env.MAX_UPLOAD_BYTES) || 2 * 1024 * 1024 * 1024
);

function queryValue(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

function sendFileError(res: Response, error: unknown): void {
  // Checked before the Docker cases below: FileStorageError carries its own
  // statusCode and must keep its specific message.
  if (error instanceof FileStorageError) {
    const messages: Record<string, string> = {
      INVALID_PATH: "Invalid file path",
      INVALID_NAME: "Invalid file or folder name",
      ROOT_NOT_FOUND: "File root not found",
      ROOT_MUTATION: "The configured root cannot be changed",
      ROOT_DOWNLOAD: "Choose a file or folder to download",
      OFFLINE_ROOT_NOT_VOLUME:
        "This server must be running because the file root is not backed by a Docker volume",
      CONTAINER_FILE_OPERATION_42: "File or folder not found",
      CONTAINER_FILE_OPERATION_44: "Symbolic links cannot be accessed",
      CONTAINER_FILE_OPERATION_45: "Folder not found",
      CONTAINER_FILE_OPERATION_46: "A file or folder with that name already exists",
    };
    res
      .status(error.statusCode)
      .json({ error: messages[error.code] || "File operation failed" });
    return;
  }

  const routeError = error as DockerRouteError;
  if (routeError.code === "INVALID_CONTAINER_ID") {
    res.status(400).json({ error: "Invalid container identifier" });
    return;
  }
  if (routeError.code === "FORBIDDEN") {
    res.status(403).json({ error: "Container is not managed by Ludock" });
    return;
  }
  if (routeError.statusCode === 404) {
    res.status(404).json({ error: "Container not found" });
    return;
  }

  logger.error("File operation failed", { error: errorMessage(error) });
  res.status(500).json({ error: "File operation failed" });
}

function fileAudit(
  req: Request,
  res: Response,
  action: string,
  containerId: string,
  details: Record<string, unknown>
): void {
  const user = res.locals.user as SessionUser;
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action: `container.file.${action}`,
    targetType: "container",
    targetId: containerId,
    details,
    ipAddress: req.ip,
  });
}

function contentDisposition(name: string): string {
  const fallback = name
    .replace(/[^\x20-\x7e]/g, "_")
    .replace(/["\\]/g, "_")
    .slice(0, 180);
  return `attachment; filename="${fallback || "download"}"; filename*=UTF-8''${encodeURIComponent(name)}`;
}

router.get("/api/servers", async (_req: Request, res: Response) => {
  try {
    const servers = await listManagedContainers();
    res.json({ servers });
  } catch (error) {
    logger.error("Failed to list servers", { error: errorMessage(error) });
    res.status(500).json({ error: "Failed to list servers" });
  }
});

router.get("/api/servers/:id", async (req: Request, res: Response) => {
  try {
    const id = req.params.id as string;
    const [server, stats] = await Promise.allSettled([
      getManagedContainer(id),
      getContainerStats(id),
    ]);

    if (server.status === "rejected") {
      // Never reflect the underlying dockerode message; it can expose socket
      // paths and daemon internals to any authenticated role.
      sendDockerError(res, server.reason, "Failed to get server details");
      return;
    }

    res.json({
      server: server.value,
      stats: stats.status === "fulfilled" ? stats.value : null,
    });
  } catch (error) {
    logger.error("Failed to get server", { error: errorMessage(error) });
    res.status(500).json({ error: "Failed to get server details" });
  }
});

router.get("/api/servers/:id/files", async (req: Request, res: Response) => {
  const parsed = fileLocationSchema.safeParse({
    root: queryValue(req.query.root),
    path: queryValue(req.query.path) || "",
  });
  if (!parsed.success) {
    res.status(400).json({ error: "Invalid file location" });
    return;
  }
  try {
    const id = req.params.id as string;
    const server = await getManagedContainer(id);
    res.json(await listFiles(server, parsed.data.root, parsed.data.path));
  } catch (error) {
    sendFileError(res, error);
  }
});

router.get(
  "/api/servers/:id/files/download",
  async (req: Request, res: Response) => {
    const parsed = fileLocationSchema.safeParse({
      root: queryValue(req.query.root),
      path: queryValue(req.query.path) || "",
    });
    if (!parsed.success) {
      res.status(400).json({ error: "Invalid file location" });
      return;
    }
    try {
      const id = req.params.id as string;
      const server = await getManagedContainer(id);
      const download = await openDownload(
        server,
        parsed.data.root,
        parsed.data.path
      );
      res.setHeader("Content-Disposition", contentDisposition(download.name));
      res.setHeader(
        "Content-Type",
        download.type === "directory"
          ? "application/x-tar"
          : "application/octet-stream"
      );
      if (download.type === "file") {
        res.setHeader("Content-Length", String(download.size));
      }
      fileAudit(req, res, "downloaded", id, parsed.data);
      download.stream.on("error", (error) => {
        logger.error("Download stream failed", {
          container: id.slice(0, 12),
          error: errorMessage(error),
        });
        res.destroy();
      });
      res.once("close", () => {
        const stream = download.stream as NodeJS.ReadableStream & {
          destroy?: () => void;
        };
        if (!res.writableEnded) stream.destroy?.();
      });
      download.stream.pipe(res);
    } catch (error) {
      sendFileError(res, error);
    }
  }
);

router.put(
  "/api/servers/:id/files/upload",
  requireRole("admin", "operator"),
  async (req: Request, res: Response) => {
    if (!req.is("application/octet-stream")) {
      res
        .status(415)
        .json({ error: "Uploads must use application/octet-stream" });
      return;
    }
    const parsed = uploadQuerySchema.safeParse({
      root: queryValue(req.query.root),
      path: queryValue(req.query.path) || "",
      name: queryValue(req.query.name),
    });
    const size = Number(req.get("content-length"));
    if (!parsed.success || !Number.isSafeInteger(size) || size < 0) {
      res.status(400).json({ error: "Invalid upload request" });
      return;
    }
    if (size > maxUploadBytes) {
      res.status(413).json({ error: "File exceeds the upload size limit" });
      return;
    }
    try {
      const id = req.params.id as string;
      const server = await getManagedContainer(id);
      await uploadFile(
        server,
        parsed.data.root,
        parsed.data.path,
        parsed.data.name,
        size,
        req
      );
      fileAudit(req, res, "uploaded", id, {
        root: parsed.data.root,
        path: parsed.data.path,
        name: parsed.data.name,
        size,
      });
      res.status(201).json({ ok: true });
    } catch (error) {
      sendFileError(res, error);
    }
  }
);

router.post(
  "/api/servers/:id/files/directory",
  requireRole("admin", "operator"),
  async (req: Request, res: Response) => {
    const parsed = directorySchema.safeParse(req.body);
    if (!parsed.success) {
      res.status(400).json({ error: "Invalid folder request" });
      return;
    }
    try {
      const id = req.params.id as string;
      const server = await getManagedContainer(id);
      await createDirectory(
        server,
        parsed.data.root,
        parsed.data.path,
        parsed.data.name
      );
      fileAudit(req, res, "directory.created", id, parsed.data);
      res.status(201).json({ ok: true });
    } catch (error) {
      sendFileError(res, error);
    }
  }
);

router.patch(
  "/api/servers/:id/files/rename",
  requireRole("admin", "operator"),
  async (req: Request, res: Response) => {
    const parsed = renameSchema.safeParse(req.body);
    if (!parsed.success) {
      res.status(400).json({ error: "Invalid rename request" });
      return;
    }
    try {
      const id = req.params.id as string;
      const server = await getManagedContainer(id);
      await renameFileEntry(
        server,
        parsed.data.root,
        parsed.data.path,
        parsed.data.newName
      );
      fileAudit(req, res, "renamed", id, parsed.data);
      res.json({ ok: true });
    } catch (error) {
      sendFileError(res, error);
    }
  }
);

router.delete(
  "/api/servers/:id/files",
  requireRole("admin", "operator"),
  async (req: Request, res: Response) => {
    const parsed = fileLocationSchema.safeParse({
      root: queryValue(req.query.root),
      path: queryValue(req.query.path) || "",
    });
    if (!parsed.success) {
      res.status(400).json({ error: "Invalid file location" });
      return;
    }
    try {
      const id = req.params.id as string;
      const server = await getManagedContainer(id);
      await deleteFileEntry(
        server,
        parsed.data.root,
        parsed.data.path
      );
      fileAudit(req, res, "deleted", id, parsed.data);
      res.json({ ok: true });
    } catch (error) {
      sendFileError(res, error);
    }
  }
);

router.post(
  "/api/servers/:id/start",
  requireRole("admin", "operator"),
  async (req: Request, res: Response) => {
    try {
      const id = req.params.id as string;
      await startContainer(id);
      auditContainerAction(req, res, "start", id);
      res.json({ ok: true });
    } catch (error: unknown) {
      sendDockerError(res, error, "Failed to start container");
    }
  }
);

router.post(
  "/api/servers/:id/stop",
  requireRole("admin", "operator"),
  async (req: Request, res: Response) => {
    try {
      const id = req.params.id as string;
      await stopContainer(id);
      auditContainerAction(req, res, "stop", id);
      res.json({ ok: true });
    } catch (error: unknown) {
      sendDockerError(res, error, "Failed to stop container");
    }
  }
);

router.post(
  "/api/servers/:id/restart",
  requireRole("admin", "operator"),
  async (req: Request, res: Response) => {
    try {
      const id = req.params.id as string;
      await restartContainer(id);
      auditContainerAction(req, res, "restart", id);
      res.json({ ok: true });
    } catch (error: unknown) {
      sendDockerError(res, error, "Failed to restart container");
    }
  }
);
