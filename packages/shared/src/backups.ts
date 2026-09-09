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

export const restoreRequestSchema = z
  .object({
    backupId: z.string().uuid(),
    confirmation: z.string().max(200),
  })
  .strict();
export type RestoreRequest = z.infer<typeof restoreRequestSchema>;
