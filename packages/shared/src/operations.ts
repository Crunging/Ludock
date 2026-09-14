import { z } from "zod";

export const operationStatusSchema = z.enum([
  "queued",
  "running",
  "succeeded",
  "already_current",
  "failed",
  "interrupted",
]);
export type OperationStatus = z.infer<typeof operationStatusSchema>;
export const operationSchema = z.object({
  id: z.string().uuid(),
  serverId: z.string().uuid(),
  kind: z.string(),
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
});
export const operationResponseSchema = z.object({
  operation: operationSchema,
});
