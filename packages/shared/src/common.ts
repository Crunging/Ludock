import { z } from "zod";

/** Validate external input before passing these distinct identifiers internally. */
export const logicalServerIdSchema = z
  .string()
  .uuid()
  .brand<"LogicalServerId">();
export type LogicalServerId = z.infer<typeof logicalServerIdSchema>;
// Docker Engine accepts full IDs, short IDs, and names. The brand distinguishes
// an inspected Engine reference from the logical UUID used in public routes.
export const dockerContainerIdSchema = z
  .string()
  .regex(/^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/)
  .brand<"DockerContainerId">();
export type DockerContainerId = z.infer<typeof dockerContainerIdSchema>;

export const okResponseSchema = z.object({ ok: z.literal(true) });
export const apiErrorSchema = z.object({
  error: z.string().min(1),
  code: z.string().optional(),
  requestId: z.string().optional(),
});
export type ApiError = z.infer<typeof apiErrorSchema>;

/** Minimal parser interface keeps clients independent of a specific schema library. */
export interface ResponseSchema<T> {
  parse(value: unknown): T;
}
