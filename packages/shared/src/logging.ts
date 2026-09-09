import { z } from "zod";

export const auditEntrySchema = z.object({
  id: z.number().int().positive(),
  username: z.string().nullable(),
  action: z.string(),
  targetType: z.string().nullable(),
  targetId: z.string().nullable(),
  details: z.unknown(),
  ipAddress: z.string().nullable(),
  createdAt: z.number().int().nonnegative(),
});
export type AuditEntry = z.infer<typeof auditEntrySchema>;
export const auditResponseSchema = z.object({
  entries: z.array(auditEntrySchema),
});
export const applicationLogLevelSchema = z.enum([
  "debug",
  "info",
  "warn",
  "error",
]);
export type ApplicationLogLevel = z.infer<typeof applicationLogLevelSchema>;
export const applicationLogContextSchema = z.record(
  z.union([z.string(), z.number(), z.boolean(), z.null()]),
);
export type ApplicationLogContext = z.infer<typeof applicationLogContextSchema>;
export const applicationLogEntrySchema = z.object({
  id: z.number().int().positive(),
  timestamp: z.number().int().nonnegative(),
  level: applicationLogLevelSchema,
  component: z.string(),
  message: z.string(),
  context: applicationLogContextSchema.optional(),
});
export type ApplicationLogEntry = z.infer<typeof applicationLogEntrySchema>;
export const applicationLogsResponseSchema = z.object({
  generation: z.string().uuid(),
  entries: z.array(applicationLogEntrySchema),
});
