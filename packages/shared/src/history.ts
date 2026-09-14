import { z } from "zod";

export const historyActorSchema = z.object({
  id: z.string().nullable(),
  name: z.string().nullable(),
});
export type HistoryActor = z.infer<typeof historyActorSchema>;

export const historyQueryShape = {
  limit: z.coerce.number().int().min(1).max(250).default(50),
  cursor: z.string().min(1).max(2048).optional(),
  serverId: z.string().uuid().optional(),
  actor: z.string().trim().min(1).max(200).optional(),
  action: z.string().trim().min(1).max(200).optional(),
  from: z.coerce.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER).optional(),
  to: z.coerce.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER).optional(),
};

export const validHistoryRange = (query: { from?: number; to?: number }): boolean =>
  query.from === undefined || query.to === undefined || query.from <= query.to;
