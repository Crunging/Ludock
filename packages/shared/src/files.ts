import { z } from "zod";

export const fileRootSchema = z.object({
  id: z.string(),
  name: z.string(),
  path: z.string(),
});
export type FileRoot = z.infer<typeof fileRootSchema>;
export const fileEntrySchema = z.object({
  name: z.string(),
  type: z.enum(["file", "directory", "symlink"]),
  size: z.number().finite().nonnegative(),
  modifiedAt: z.number().finite(),
});
export type FileEntry = z.infer<typeof fileEntrySchema>;
export const fileListingSchema = z.object({
  root: fileRootSchema,
  path: z.string(),
  entries: z.array(fileEntrySchema),
});
export type FileListing = z.infer<typeof fileListingSchema>;

export const fileLocationSchema = z.object({
  root: z.string().min(1).max(64),
  path: z.string().max(2048).default(""),
});

export const createDirectoryRequestSchema = fileLocationSchema.extend({
  name: z.string().min(1).max(255),
});

export const renameFileRequestSchema = fileLocationSchema.extend({
  newName: z.string().min(1).max(255),
});

export const uploadFileQuerySchema = fileLocationSchema.extend({
  name: z.string().min(1).max(255),
});

export type FileLocationRequest = z.input<typeof fileLocationSchema>;
export type CreateDirectoryRequest = z.input<
  typeof createDirectoryRequestSchema
>;
export type RenameFileRequest = z.input<typeof renameFileRequestSchema>;
export type UploadFileQuery = z.input<typeof uploadFileQuerySchema>;
