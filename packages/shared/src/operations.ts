import { z } from "zod";
import { historyActorSchema, historyQueryShape, validHistoryRange } from "./history.js";

export const operationStatusSchema = z.enum([
  "queued",
  "running",
  "succeeded",
  "already_current",
  "failed",
  "interrupted",
]);
export type OperationStatus = z.infer<typeof operationStatusSchema>;
export const operationHistoryQuerySchema = z.object({
  ...historyQueryShape,
  kind: z.string().trim().min(1).max(200).optional(),
  status: operationStatusSchema.optional(),
}).refine(validHistoryRange, { message: "From must be before or equal to to", path: ["to"] });
export type OperationHistoryQuery = z.infer<typeof operationHistoryQuerySchema>;
export const operationSchema = z.object({
  id: z.string().uuid(),
  serverId: z.string().uuid(),
  kind: z.string(),
  actor: historyActorSchema.nullable().optional(),
  status: operationStatusSchema,
  phase: z.string(),
  createdAt: z.number().int().nonnegative(),
  updatedAt: z.number().int().nonnegative(),
  error: z.string().nullable(),
  result: z.record(z.string(), z.unknown()).nullable(),
});
export type Operation = z.infer<typeof operationSchema>;
export const operationsResponseSchema = z.object({
  operations: z.array(operationSchema),
  nextCursor: z.string().nullable().default(null),
});
export const operationResponseSchema = z.object({
  operation: operationSchema,
});
