import { z } from "zod";

export const availabilitySchema = z
  .object({
    enabled: z.boolean(),
    maintenance: z.boolean().default(false),
    graceSeconds: z.number().int().min(10).max(86400).default(120),
  })
  .strict();

export const availabilityPolicySchema = availabilitySchema.extend({
  maintenance: availabilitySchema.shape.maintenance.removeDefault(),
  graceSeconds: availabilitySchema.shape.graceSeconds.removeDefault(),
});
export type AvailabilityPolicy = z.infer<typeof availabilityPolicySchema>;
export const availabilityStateSchema = z.object({
  outageStartedAt: z.number().int().nonnegative().nullable(),
  notified: z.boolean(),
  suppressedUntil: z.number().int().nonnegative(),
  intentionallyStopped: z.boolean(),
  lastState: z.string().nullable(),
});
export type AvailabilityState = z.infer<typeof availabilityStateSchema>;
export const availabilityResponseSchema = z.object({
  policy: availabilityPolicySchema,
  state: availabilityStateSchema,
});
export const notificationSettingsResponseSchema = z.object({
  configured: z.boolean(),
  enabled: z.boolean(),
});
export type NotificationSettings = z.infer<
  typeof notificationSettingsResponseSchema
>;

export const notificationSettingsRequestSchema = z
  .object({
    enabled: z.boolean(),
    webhookUrl: z.string().max(1024).optional(),
  })
  .strict();
export type NotificationSettingsRequest = z.infer<
  typeof notificationSettingsRequestSchema
>;

export const notificationDeliverySchema = z.object({
  id: z.string().uuid(),
  kind: z.enum(["test", "event"]),
  state: z.enum(["queued", "delivered", "failed"]),
  attempts: z.number().int().nonnegative(),
  createdAt: z.number().int().nonnegative(),
  lastAttemptAt: z.number().int().nonnegative().nullable(),
  deliveredAt: z.number().int().nonnegative().nullable(),
  nextAttemptAt: z.number().int().nonnegative().nullable(),
  lastFailure: z.string().nullable(),
  retryable: z.boolean(),
});
export type NotificationDelivery = z.infer<typeof notificationDeliverySchema>;
export const notificationDeliveriesResponseSchema = z.object({
  deliveries: z.array(notificationDeliverySchema).max(50),
});
export const notificationDeliveryResponseSchema = z.object({
  delivery: notificationDeliverySchema,
});
