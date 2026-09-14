import { z } from "zod";
import { serverCapabilitySchema } from "./access.js";
import { fileRootSchema } from "./files.js";

export const bindingStatusSchema = z.enum([
  "active",
  "missing",
  "ambiguous",
  "review_required",
]);

export const serverSchema = z.object({
  id: z.string().uuid(),
  shortId: z.string(),
  name: z.string(),
  displayName: z.string(),
  image: z.string(),
  state: z.string(),
  status: z.string(),
  gameType: z.string(),
  gameConsole: z
    .object({
      id: z.enum([
        "minecraft-rcon",
        "source-rcon",
        "rust-webrcon",
        "telnet-console",
        "stdin-console",
      ]),
      name: z.string(),
      commandPlaceholder: z.string(),
    })
    .nullable(),
  fileRoots: z.array(fileRootSchema),
  ports: z.array(
    z.object({ private: z.number(), public: z.number(), type: z.string() }),
  ),
  created: z.number(),
  labels: z.record(z.string(), z.string()),
  permissions: z.array(serverCapabilitySchema),
  bindingStatus: bindingStatusSchema,
});
export type Server = z.infer<typeof serverSchema>;

export const serverStatsSchema = z.object({
  cpuPercent: z.number().finite(),
  memUsageMB: z.number().finite().nonnegative(),
  memLimitMB: z.number().finite().nonnegative(),
});
export type ServerStats = z.infer<typeof serverStatsSchema>;
export const serversResponseSchema = z.object({
  servers: z.array(serverSchema),
});
export const serverResponseSchema = z.object({
  server: serverSchema,
  stats: serverStatsSchema.nullable(),
});
export const bindingReviewResponseSchema = z.object({ server: serverSchema });
export const SERVER_STATE_ACTIONS = [
  "create",
  "start",
  "stop",
  "die",
  "kill",
  "restart",
  "destroy",
  "rename",
  "pause",
  "unpause",
  "update",
  "oom",
  "health_status: healthy",
  "health_status: unhealthy",
  "health_status: starting",
] as const;
const serverEventBaseSchema = z.object({
  type: z.literal("container_event"),
  time: z.number().finite(),
});
// Invalidation for a disappeared server deliberately carries no identifier.
export const serverEventSchema = z.union([
  serverEventBaseSchema.extend({ action: z.literal("refresh") }),
  serverEventBaseSchema.extend({
    action: z.enum(SERVER_STATE_ACTIONS),
    serverId: z.string().uuid(),
  }),
]);
export type ServerEvent = z.infer<typeof serverEventSchema>;
export const consoleMessageSchema = z.object({
  type: z.enum(["stdout", "stderr", "system", "error"]),
  data: z.string(),
});
export type ConsoleMessage = z.infer<typeof consoleMessageSchema>;

export const bindingReviewRequestSchema = z
  .object({ confirmation: z.string().max(200) })
  .strict();
export type BindingReviewRequest = z.infer<typeof bindingReviewRequestSchema>;
