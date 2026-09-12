import { z } from "zod";
import { operationSchema } from "./operations.js";

export const scheduleSchema = z
  .object({
    action: z.enum(["start", "stop", "restart", "backup"]),
    enabled: z.boolean().default(true),
    time: z.string().regex(/^(?:[01]\d|2[0-3]):[0-5]\d$/),
    days: z.array(z.number().int().min(0).max(6)).min(1).max(7),
    timezone: z
      .string()
      .min(1)
      .max(100)
      .refine((value) => {
        try {
          new Intl.DateTimeFormat("en", { timeZone: value });
          return true;
        } catch {
          return false;
        }
      }, "Unknown timezone"),
  })
  .strict();
export type ScheduleInput = z.infer<typeof scheduleSchema>;

export const updateScheduleRequestSchema = scheduleSchema
  .extend({
    enabled: scheduleSchema.shape.enabled.removeDefault(),
    revision: z.number().int().positive(),
  })
  .strict();
export type UpdateScheduleRequest = z.infer<typeof updateScheduleRequestSchema>;

export const scheduleEnabledRequestSchema = z
  .object({
    enabled: z.boolean(),
    revision: z.number().int().positive(),
  })
  .strict();
export type ScheduleEnabledRequest = z.infer<typeof scheduleEnabledRequestSchema>;

export const nextRunUnavailableReasonSchema = z.enum([
  "owner_missing",
  "owner_disabled",
  "owner_access_removed",
  "action_access_removed",
  "binding_changed",
  "binding_unavailable",
  "unavailable",
]);
export type NextRunUnavailableReason = z.infer<typeof nextRunUnavailableReasonSchema>;

export const savedScheduleSchema = scheduleSchema.extend({
  enabled: scheduleSchema.shape.enabled.removeDefault(),
  id: z.string().uuid(),
  serverId: z.string().uuid(),
  ownerId: z.string().uuid(),
  lastResult: z.string().nullable(),
  lastOperation: operationSchema.nullable(),
  lastRunAt: z.number().int().nonnegative().nullable(),
  // Execution slot for preview deduplication, not user-facing display text.
  lastSlot: z.string().nullable(),
  revision: z.number().int().positive(),
  nextRunAt: z.number().int().nonnegative().nullable(),
  nextRunUnavailableReason: nextRunUnavailableReasonSchema.nullable(),
});
export type Schedule = z.infer<typeof savedScheduleSchema>;
export const schedulesResponseSchema = z.object({
  schedules: z.array(savedScheduleSchema),
});
export const scheduleResponseSchema = z.object({
  schedule: savedScheduleSchema,
});
