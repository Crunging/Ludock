import {
  Router,
  type Request,
  type Response,
  type Router as RouterType,
} from "express";
import {
  fileListingSchema,
  okResponseSchema,
  fileLocationSchema,
  createDirectoryRequestSchema,
  renameFileRequestSchema,
  uploadFileQuerySchema,
} from "@ludock/shared";
import { writeAuditLog, type SessionUser } from "../database.js";
import {
  createDirectory,
  deleteFileEntry,
  FileStorageError,
  listFiles,
  openDownload,
  renameFileEntry,
  uploadFile,
} from "../file-storage.js";
import { createLogger, errorMessage } from "../logger.js";
import { respond } from "./request.js";
import { serverAction } from "./server-action.js";
import { AuthError } from "../auth.js";
import { AuthorizationError } from "../authorization.js";
import { AppError } from "../errors.js";
import { ServerBindingError } from "../identity.js";

export const filesRouter: RouterType = Router();
const logger = createLogger("api");
interface FileRouteError extends Error {
  statusCode?: number;
  code?: string;
}

const maxUploadBytes = Math.max(
  1,
  Number(process.env.MAX_UPLOAD_BYTES) || 2 * 1024 * 1024 * 1024,
);

function queryValue(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

function sendFileError(res: Response, error: unknown): void {
  if (error instanceof AuthError || error instanceof AuthorizationError || error instanceof AppError || error instanceof ServerBindingError) {
    res.status(error.statusCode).json({ error: error.message, code: error.code });
    return;
  }
  // Checked before the Docker cases below: FileStorageError carries its own
  // statusCode and must keep its specific message.
  if (error instanceof FileStorageError) {
    const messages: Record<string, string> = {
      INVALID_PATH: "Invalid file path",
      INVALID_NAME: "Invalid file or folder name",
      ROOT_NOT_FOUND: "File root not found",
      ROOT_MUTATION: "The configured root cannot be changed",
      ROOT_DOWNLOAD: "Choose a file or folder to download",
    };
    res
      .status(error.statusCode)
      .json({ error: messages[error.code] || "File operation failed" });
    return;
  }

  const routeError = error as FileRouteError;
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
  details: Record<string, unknown>,
): void {
  const user = res.locals.user as SessionUser;
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action: `server.file.${action}`,
    targetType: "server",
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

filesRouter.get(
  "/api/v1/servers/:id/files",
  serverAction("files.read", async (req, res, context) => {
    const parsed = fileLocationSchema.safeParse({
      root: queryValue(req.query.root),
      path: queryValue(req.query.path) || "",
    });
    if (!parsed.success) {
      res.status(400).json({ error: "Invalid file location" });
      return;
    }
    try {
      const server = context.container;
      respond(
        res,
        fileListingSchema,
        await listFiles(server, parsed.data.root, parsed.data.path, context.assertAccess),
      );
    } catch (error) {
      sendFileError(res, error);
    }
  }),
);

filesRouter.get(
  "/api/v1/servers/:id/files/download",
  serverAction("files.read", async (req, res, context) => {
    const parsed = fileLocationSchema.safeParse({
      root: queryValue(req.query.root),
      path: queryValue(req.query.path) || "",
    });
    if (!parsed.success) {
      res.status(400).json({ error: "Invalid file location" });
      return;
    }
    try {
      const id = context.logical.id;
      const server = context.container;
      const download = await openDownload(
        server,
        parsed.data.root,
        parsed.data.path,
        context.assertAccess,
      );
      res.setHeader("Content-Disposition", contentDisposition(download.name));
      res.setHeader(
        "Content-Type",
        download.type === "directory"
          ? "application/x-tar"
          : "application/octet-stream",
      );
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
      await download.completed;
    } catch (error) {
      if (res.headersSent || res.destroyed) res.destroy();
      else sendFileError(res, error);
    }
  }),
);

filesRouter.put(
  "/api/v1/servers/:id/files/upload",
  serverAction("files.write", async (req, res, context) => {
    if (!req.is("application/octet-stream")) {
      res
        .status(415)
        .json({ error: "Uploads must use application/octet-stream" });
      return;
    }
    const parsed = uploadFileQuerySchema.safeParse({
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
      const id = context.logical.id;
      const server = context.container;
      await uploadFile(
        server,
        parsed.data.root,
        parsed.data.path,
        parsed.data.name,
        size,
        req,
        context.assertAccess,
      );
      fileAudit(req, res, "uploaded", id, {
        root: parsed.data.root,
        path: parsed.data.path,
        name: parsed.data.name,
        size,
      });
      respond(res.status(201), okResponseSchema, { ok: true });
    } catch (error) {
      sendFileError(res, error);
    }
  }),
);

filesRouter.post(
  "/api/v1/servers/:id/files/directory",
  serverAction("files.write", async (req, res, context) => {
    const parsed = createDirectoryRequestSchema.safeParse(req.body);
    if (!parsed.success) {
      res.status(400).json({ error: "Invalid folder request" });
      return;
    }
    try {
      const id = context.logical.id;
      const server = context.container;
      await createDirectory(
        server,
        parsed.data.root,
        parsed.data.path,
        parsed.data.name,
        context.assertAccess,
      );
      fileAudit(req, res, "directory.created", id, parsed.data);
      respond(res.status(201), okResponseSchema, { ok: true });
    } catch (error) {
      sendFileError(res, error);
    }
  }),
);

filesRouter.patch(
  "/api/v1/servers/:id/files/rename",
  serverAction("files.write", async (req, res, context) => {
    const parsed = renameFileRequestSchema.safeParse(req.body);
    if (!parsed.success) {
      res.status(400).json({ error: "Invalid rename request" });
      return;
    }
    try {
      const id = context.logical.id;
      const server = context.container;
      await renameFileEntry(
        server,
        parsed.data.root,
        parsed.data.path,
        parsed.data.newName,
        context.assertAccess,
      );
      fileAudit(req, res, "renamed", id, parsed.data);
      respond(res, okResponseSchema, { ok: true });
    } catch (error) {
      sendFileError(res, error);
    }
  }),
);

filesRouter.delete(
  "/api/v1/servers/:id/files",
  serverAction("files.write", async (req, res, context) => {
    const parsed = fileLocationSchema.safeParse({
      root: queryValue(req.query.root),
      path: queryValue(req.query.path) || "",
    });
    if (!parsed.success) {
      res.status(400).json({ error: "Invalid file location" });
      return;
    }
    try {
      const id = context.logical.id;
      const server = context.container;
      await deleteFileEntry(server, parsed.data.root, parsed.data.path, context.assertAccess);
      fileAudit(req, res, "deleted", id, parsed.data);
      respond(res, okResponseSchema, { ok: true });
    } catch (error) {
      sendFileError(res, error);
    }
  }),
);
