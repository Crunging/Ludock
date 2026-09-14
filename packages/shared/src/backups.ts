import { z } from "zod";

export const backupSettingsSchema = z
  .object({
    destination: z.string().min(1).max(1024),
    retentionCount: z.number().int().min(1).max(1000),
    maxBytes: z.number().int().positive().safe(),
    reserveBytes: z.number().int().nonnegative().safe(),
  })
  .strict();
export type BackupSettings = z.infer<typeof backupSettingsSchema>;

export const backupSchema = z.object({
  id: z.string().uuid(),
  serverId: z.string().uuid(),
  createdAt: z.number().int().nonnegative(),
  size: z.number().int().nonnegative().safe(),
  checksum: z.string(),
  roots: z.array(z.object({ id: z.string(), path: z.string() })),
  state: z.enum(["complete", "failed"]),
});
export type Backup = z.infer<typeof backupSchema>;
export const backupsResponseSchema = z.object({
  backups: z.array(backupSchema),
});
export const backupSettingsResponseSchema = z.object({
  settings: backupSettingsSchema.nullable(),
});

export const backupReadinessIssueSchema = z.object({
  code: z.string(),
  message: z.string(),
});
export type BackupReadinessIssue = z.infer<typeof backupReadinessIssueSchema>;

export const backupStorageStatusSchema = z.object({
  configured: z.boolean(),
  archiveBytes: z.number().int().nonnegative().safe(),
  maxBytes: z.number().int().positive().safe().nullable(),
  reserveBytes: z.number().int().nonnegative().safe().nullable(),
  availableBytes: z.number().int().nonnegative().safe().nullable(),
  issues: z.array(backupReadinessIssueSchema),
});
export type BackupStorageStatus = z.infer<typeof backupStorageStatusSchema>;
export const backupStorageResponseSchema = z.object({
  storage: backupStorageStatusSchema,
});

export const backupPreflightSchema = z.object({
  ready: z.boolean(),
  checkedAt: z.number().int().nonnegative(),
  issues: z.array(backupReadinessIssueSchema),
});
export type BackupPreflight = z.infer<typeof backupPreflightSchema>;
export const backupPreflightResponseSchema = z.object({
  preflight: backupPreflightSchema,
});

export const restoreRequestSchema = z
  .object({
    backupId: z.string().uuid(),
    confirmation: z.string().max(200),
  })
  .strict();
export type RestoreRequest = z.infer<typeof restoreRequestSchema>;
