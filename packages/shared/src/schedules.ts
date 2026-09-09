import { z } from "zod";

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

export const savedScheduleSchema = scheduleSchema.extend({
  enabled: scheduleSchema.shape.enabled.removeDefault(),
  id: z.string().uuid(),
  serverId: z.string().uuid(),
  ownerId: z.string().uuid(),
  lastResult: z.string().nullable(),
});
export type Schedule = z.infer<typeof savedScheduleSchema>;
export const schedulesResponseSchema = z.object({
  schedules: z.array(savedScheduleSchema),
});
export const scheduleResponseSchema = z.object({
  schedule: savedScheduleSchema,
});
