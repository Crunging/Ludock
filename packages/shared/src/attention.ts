import { z } from "zod";
import { nextRunUnavailableReasonSchema, scheduleSchema } from "./schedules.js";

const attentionItemBaseSchema = z.object({
  id: z.string(),
  serverId: z.string().uuid(),
  serverName: z.string(),
});

export const attentionItemSchema = z.discriminatedUnion("kind", [
  attentionItemBaseSchema.extend({
    kind: z.literal("binding"),
    bindingStatus: z.enum(["missing", "ambiguous", "review_required"]),
  }),
  attentionItemBaseSchema.extend({
    kind: z.literal("availability"),
    state: z.string(),
    outageStartedAt: z.number().int().nonnegative(),
  }),
  attentionItemBaseSchema.extend({
    kind: z.literal("schedule"),
    scheduleId: z.string().uuid(),
    action: scheduleSchema.shape.action,
    reason: nextRunUnavailableReasonSchema,
  }),
  attentionItemBaseSchema.extend({
    kind: z.literal("operation"),
    operationId: z.string().uuid(),
    operationKind: z.string(),
    status: z.enum(["failed", "interrupted"]),
    updatedAt: z.number().int().nonnegative(),
  }),
]);
export type AttentionItem = z.infer<typeof attentionItemSchema>;

export const attentionResponseSchema = z.object({
  items: z.array(attentionItemSchema),
  // Persisted issues remain available when live discovery cannot be refreshed.
  discoveryUnavailable: z.boolean(),
});
export type AttentionResponse = z.infer<typeof attentionResponseSchema>;
