import { createDirectoryRequestSchema, fileListingSchema, fileLocationSchema, formatByteSize, okResponseSchema, renameFileRequestSchema, uploadFileQuerySchema, } from "@ludock/shared";
import { writeAuditLog } from "../database.js";
import { AppError, errorResponse } from "../errors.js";
import { createDirectory, deleteFileEntry, listFiles, openDownload, renameFileEntry, uploadFile, } from "../file-storage.js";
import { createLogger, errorMessage } from "../logger.js";
import { getMaxUploadBytes } from "../upload-limit.js";
import { requestUser, respond, type ApiRoutes, type RequestContext } from "./request.js";
import { serverAction } from "./server-action.js";
const logger = createLogger("api");
interface FileRouteError extends Error {
  statusCode?: number;
  code?: string;
}
const maxUploadBytes = getMaxUploadBytes();
function sendFileError(error: unknown): Response {
  if (error instanceof AppError) return errorResponse(error);
  const routeError = error as FileRouteError;
  if (routeError.code === "INVALID_CONTAINER_ID") {
    return Response.json({ error: "Invalid container identifier" }, { status: 400 });
  }
  if (routeError.code === "FORBIDDEN") {
    return Response.json({ error: "Container is not managed by Ludock" }, { status: 403 });
  }
  if (routeError.statusCode === 404) {
    return Response.json({ error: "Container not found" }, { status: 404 });
  }
  logger.error("File operation failed", { error: errorMessage(error) });
  return Response.json({ error: "File operation failed" }, { status: 500 });
}
function fileAudit(ctx: RequestContext, action: string, containerId: string, details: Record<string, unknown>): void {
  const user = requestUser(ctx);
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action: `server.file.${action}`,
    targetType: "server",
    targetId: containerId,
    details,
    ipAddress: ctx.ipAddress,
  });
}
function contentDisposition(name: string): string {
  const fallback = name
    .replace(/[^\x20-\x7e]/g, "_")
    .replace(/["\\]/g, "_")
    .slice(0, 180);
  return `attachment; filename="${fallback || "download"}"; filename*=UTF-8''${encodeURIComponent(name)}`;
}

export const filesRoutes: ApiRoutes = {
  "/api/v1/servers/:id/files": {
    GET: serverAction("files.read", async (ctx, context) => {
      const parsed = fileLocationSchema.safeParse({
        root: ctx.url.searchParams.get("root") ?? undefined,
        path: ctx.url.searchParams.get("path") ?? "",
      });
      if (!parsed.success) {
        return Response.json({ error: "Invalid file location" }, { status: 400 });
      }
      try {
        const server = context.container;
        return respond(fileListingSchema, await listFiles(server, parsed.data.root, parsed.data.path, context.assertAccess));
      }
      catch (error) {
        return sendFileError(error);
      }
    }),
    DELETE: serverAction("files.write", async (ctx, context) => {
      const parsed = fileLocationSchema.safeParse({
        root: ctx.url.searchParams.get("root") ?? undefined,
        path: ctx.url.searchParams.get("path") ?? "",
      });
      if (!parsed.success) {
        return Response.json({ error: "Invalid file location" }, { status: 400 });
      }
      try {
        const id = context.logical.id;
        const server = context.container;
        await deleteFileEntry(server, parsed.data.root, parsed.data.path, context.assertAccess);
        fileAudit(ctx, "deleted", id, parsed.data);
        return respond(okResponseSchema, { ok: true });
      }
      catch (error) {
        return sendFileError(error);
      }
    })
  },
  "/api/v1/servers/:id/files/download": {
    GET: serverAction("files.read", async (ctx, context) => {
      const parsed = fileLocationSchema.safeParse({
        root: ctx.url.searchParams.get("root") ?? undefined,
        path: ctx.url.searchParams.get("path") ?? "",
      });
      if (!parsed.success) {
        return Response.json({ error: "Invalid file location" }, { status: 400 });
      }
      try {
        const id = context.logical.id;
        const server = context.container;
        const download = await openDownload(server, parsed.data.root, parsed.data.path, context.assertAccess);
        ctx.headers.set("Content-Disposition", contentDisposition(download.name));
        ctx.headers.set("Content-Type", download.type === "directory"
          ? "application/x-tar"
          : "application/octet-stream");
        fileAudit(ctx, "downloaded", id, parsed.data);
        context.waitForCleanup(download.completed);
        return new Response(download.stream);
      }
      catch (error) {
        return sendFileError(error);
      }
    })
  },
  "/api/v1/servers/:id/files/upload": {
    PUT: serverAction("files.write", async (ctx, context) => {
      if (ctx.request.headers.get("content-type")?.split(";", 1)[0].trim().toLowerCase() !== "application/octet-stream") {
        return Response.json({ error: "Uploads must use application/octet-stream" }, { status: 415 });
      }
      const parsed = uploadFileQuerySchema.safeParse({
        root: ctx.url.searchParams.get("root") ?? undefined,
        path: ctx.url.searchParams.get("path") ?? "",
        name: ctx.url.searchParams.get("name") ?? undefined,
      });
      const size = Number(ctx.request.headers.get("content-length") ?? undefined);
      if (!parsed.success || !Number.isSafeInteger(size) || size < 0) {
        return Response.json({ error: "Invalid upload request" }, { status: 400 });
      }
      if (size > maxUploadBytes) {
        return Response.json({
          error: `File exceeds the upload size limit of ${formatByteSize(maxUploadBytes)}`,
        }, { status: 413 });
      }
      try {
        const id = context.logical.id;
        const server = context.container;
        const source = ctx.request.body ?? new ReadableStream<Uint8Array>({ start(controller) { controller.close(); } });
        try {
          await uploadFile(server, parsed.data.root, parsed.data.path, parsed.data.name, size, source, context.assertAccess, context.signal);
        } finally {
          await source.cancel().catch(() => {});
        }
        fileAudit(ctx, "uploaded", id, {
          root: parsed.data.root,
          path: parsed.data.path,
          name: parsed.data.name,
          size,
        });
        return respond(okResponseSchema, { ok: true }, 201);
      }
      catch (error) {
        return sendFileError(error);
      }
    })
  },
  "/api/v1/servers/:id/files/directory": {
    POST: serverAction("files.write", async (ctx, context) => {
      const parsed = createDirectoryRequestSchema.safeParse(ctx.body);
      if (!parsed.success) {
        return Response.json({ error: "Invalid folder request" }, { status: 400 });
      }
      try {
        const id = context.logical.id;
        const server = context.container;
        await createDirectory(server, parsed.data.root, parsed.data.path, parsed.data.name, context.assertAccess);
        fileAudit(ctx, "directory.created", id, parsed.data);
        return respond(okResponseSchema, { ok: true }, 201);
      }
      catch (error) {
        return sendFileError(error);
      }
    })
  },
  "/api/v1/servers/:id/files/rename": {
    PATCH: serverAction("files.write", async (ctx, context) => {
      const parsed = renameFileRequestSchema.safeParse(ctx.body);
      if (!parsed.success) {
        return Response.json({ error: "Invalid rename request" }, { status: 400 });
      }
      try {
        const id = context.logical.id;
        const server = context.container;
        await renameFileEntry(server, parsed.data.root, parsed.data.path, parsed.data.newName, context.assertAccess);
        fileAudit(ctx, "renamed", id, parsed.data);
        return respond(okResponseSchema, { ok: true });
      }
      catch (error) {
        return sendFileError(error);
      }
    })
  }
};
