import { createDirectoryRequestSchema, fileListingSchema, fileLocationSchema, formatByteSize, okResponseSchema, renameFileRequestSchema, uploadFileQuerySchema } from "@ludock/shared";
import { createDirectory, deleteFileEntry, listFiles, openDownload, renameFileEntry, uploadFile } from "../file-storage.js";
import { getMaxUploadBytes } from "../upload-limit.js";
import { audit, respond, type ApiRoutes, type RequestContext } from "./request.js";
import { serverAction } from "./server-action.js";

const maxUploadBytes = getMaxUploadBytes();

function fileLocation(ctx: RequestContext) {
  return fileLocationSchema.safeParse({
    root: ctx.url.searchParams.get("root") ?? undefined,
    path: ctx.url.searchParams.get("path") ?? "",
  });
}
function invalid(error: string): Response {
  return Response.json({ error }, { status: 400 });
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
      const location = fileLocation(ctx);
      if (!location.success) return invalid("Invalid file location");
      const { root, path } = location.data;
      return respond(fileListingSchema, await listFiles(context.container, root, path, context.assertAccess));
    }),
    DELETE: serverAction("files.write", async (ctx, context) => {
      const location = fileLocation(ctx);
      if (!location.success) return invalid("Invalid file location");
      const { root, path } = location.data;
      await deleteFileEntry(context.container, root, path, context.assertAccess);
      audit(ctx, "server.file.deleted", context.logical.id, location.data);
      return respond(okResponseSchema, { ok: true });
    }),
  },
  "/api/v1/servers/:id/files/download": {
    GET: serverAction("files.read", async (ctx, context) => {
      const location = fileLocation(ctx);
      if (!location.success) return invalid("Invalid file location");
      const { root, path } = location.data;
      const download = await openDownload(context.container, root, path, context.assertAccess);
      ctx.headers.set("Content-Disposition", contentDisposition(download.name));
      ctx.headers.set("Content-Type", download.type === "directory"
        ? "application/x-tar"
        : "application/octet-stream");
      audit(ctx, "server.file.downloaded", context.logical.id, location.data);
      context.waitForCleanup(download.completed);
      return new Response(download.stream);
    }),
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
      if (!parsed.success || !Number.isSafeInteger(size) || size < 0) return invalid("Invalid upload request");
      if (size > maxUploadBytes) {
        return Response.json({
          error: `File exceeds the upload size limit of ${formatByteSize(maxUploadBytes)}`,
        }, { status: 413 });
      }
      const { root, path, name } = parsed.data;
      const source = ctx.request.body ?? new ReadableStream<Uint8Array>({ start(controller) { controller.close(); } });
      try {
        await uploadFile(context.container, root, path, name, size, source, context.assertAccess, context.signal);
      } finally {
        await source.cancel().catch(() => {});
      }
      audit(ctx, "server.file.uploaded", context.logical.id, { root, path, name, size });
      return respond(okResponseSchema, { ok: true }, 201);
    }),
  },
  "/api/v1/servers/:id/files/directory": {
    POST: serverAction("files.write", async (ctx, context) => {
      const parsed = createDirectoryRequestSchema.safeParse(ctx.body);
      if (!parsed.success) return invalid("Invalid folder request");
      const { root, path, name } = parsed.data;
      await createDirectory(context.container, root, path, name, context.assertAccess);
      audit(ctx, "server.file.directory.created", context.logical.id, parsed.data);
      return respond(okResponseSchema, { ok: true }, 201);
    }),
  },
  "/api/v1/servers/:id/files/rename": {
    PATCH: serverAction("files.write", async (ctx, context) => {
      const parsed = renameFileRequestSchema.safeParse(ctx.body);
      if (!parsed.success) return invalid("Invalid rename request");
      const { root, path, newName } = parsed.data;
      await renameFileEntry(context.container, root, path, newName, context.assertAccess);
      audit(ctx, "server.file.renamed", context.logical.id, parsed.data);
      return respond(okResponseSchema, { ok: true });
    }),
  },
};
