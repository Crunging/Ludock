import { z } from "zod";
import { historyActorSchema, historyQueryShape, validHistoryRange } from "./history.js";
import { operationStatusSchema } from "./operations.js";

export const auditHistoryQuerySchema = z.object({
  ...historyQueryShape,
  operationId: z.string().uuid().optional(),
  status: operationStatusSchema.optional(),
}).refine(validHistoryRange, { message: "From must be before or equal to to", path: ["to"] });
export type AuditHistoryQuery = z.infer<typeof auditHistoryQuerySchema>;

export const auditEntrySchema = z.object({
  id: z.number().int().positive(),
  username: z.string().nullable(),
  actor: historyActorSchema.nullable().optional(),
  action: z.string(),
  // The event's recorded action suffix, independent of an operation's current status.
  status: operationStatusSchema.nullable().optional(),
  operationId: z.string().uuid().nullable().optional(),
  targetType: z.string().nullable(),
  targetId: z.string().nullable(),
  details: z.unknown(),
  ipAddress: z.string().nullable(),
  createdAt: z.number().int().nonnegative(),
});
export type AuditEntry = z.infer<typeof auditEntrySchema>;
export const auditResponseSchema = z.object({
  entries: z.array(auditEntrySchema),
  nextCursor: z.string().nullable().default(null),
});
export const applicationLogLevelSchema = z.enum([
  "debug",
  "info",
  "warn",
  "error",
]);
export type ApplicationLogLevel = z.infer<typeof applicationLogLevelSchema>;
export const applicationLogContextSchema = z.record(
  z.string(),
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
