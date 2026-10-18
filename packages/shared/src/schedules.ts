import { z } from "zod";
import type { ServerCapability } from "./access.js";
import { operationSchema } from "./operations.js";

export const scheduleActionSchema = z.enum(["start", "stop", "restart", "backup"]);
export type ScheduleAction = z.infer<typeof scheduleActionSchema>;

export const scheduleActionCapability = (action: ScheduleAction): ServerCapability =>
  action === "backup" ? "backups.create" : `server.${action}`;

/** Every capability a schedule needs, for its owner and for whoever edits or resumes it. */
export const scheduleCapabilities = (
  schedule: { action: ScheduleAction; createBackup: boolean },
): ServerCapability[] => [
  scheduleActionCapability(schedule.action),
  ...(schedule.createBackup ? ["backups.create" as const] : []),
];

export const scheduleAnnouncementsSchema = z.object({
  leadMinutes: z.array(z.number().int().min(1).max(60)).min(1).max(10)
    .refine((values) => new Set(values).size === values.length, "Lead times must be unique")
    .default([10, 5, 1]),
}).strict();

const scheduleFields = {
  action: scheduleActionSchema,
  announcements: scheduleAnnouncementsSchema.optional(),
  createBackup: z.boolean().default(false),
  enabled: z.boolean().default(true),
  time: z.string().regex(/^(?:[01]\d|2[0-3]):[0-5]\d$/),
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
};

export const recurringScheduleSchema = z
  .object({
    kind: z.literal("recurring"),
    ...scheduleFields,
    days: z.array(z.number().int().min(0).max(6)).min(1).max(7),
  })
  .strict();

/** Runs once at a local date and time in its time zone. */
export const oneTimeScheduleSchema = z
  .object({
    kind: z.literal("once"),
    ...scheduleFields,
    date: z
      .string()
      .regex(/^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])$/)
      .refine((value) => {
        const date = new Date(`${value}T00:00:00Z`);
        return Number.isFinite(date.getTime()) && date.toISOString().startsWith(value);
      }, "Unknown date"),
  })
  .strict();

function restartOnlyOptions<
  T extends z.ZodType<{ action: string; announcements?: unknown; createBackup: boolean }>,
>(schema: T): T {
  return schema
    .refine((value) => !value.createBackup || value.action === "restart", {
      message: "Backup before restart is only available for restart schedules",
      path: ["createBackup"],
    })
    .refine((value) => !value.announcements || value.action === "restart", {
      message: "Announcements are only available for restarts",
      path: ["announcements"],
    });
}

export const scheduleSchema = restartOnlyOptions(
  z.discriminatedUnion("kind", [recurringScheduleSchema, oneTimeScheduleSchema]),
);
export type ScheduleInput = z.infer<typeof scheduleSchema>;
export type RecurringScheduleInput = z.infer<typeof recurringScheduleSchema>;
export type OneTimeScheduleInput = z.infer<typeof oneTimeScheduleSchema>;

const updateFields = {
  enabled: scheduleFields.enabled.removeDefault(),
  revision: z.number().int().positive(),
};
export const updateScheduleRequestSchema = restartOnlyOptions(
  z.discriminatedUnion("kind", [
    recurringScheduleSchema.extend(updateFields).strict(),
    oneTimeScheduleSchema.extend(updateFields).strict(),
  ]),
);
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

const savedFields = {
  enabled: scheduleFields.enabled.removeDefault(),
  createBackup: scheduleFields.createBackup.removeDefault(),
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
  // Set once a one-time schedule's run was queued, skipped, or missed.
  completedAt: z.number().int().nonnegative().nullable(),
};
export const savedScheduleSchema = restartOnlyOptions(
  z.discriminatedUnion("kind", [
    recurringScheduleSchema.extend(savedFields),
    oneTimeScheduleSchema.extend(savedFields),
  ]),
);
export type Schedule = z.infer<typeof savedScheduleSchema>;
export const schedulesResponseSchema = z.object({
  schedules: z.array(savedScheduleSchema),
});
export const scheduleResponseSchema = z.object({
  schedule: savedScheduleSchema,
});
