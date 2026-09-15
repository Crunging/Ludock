// Bun embeds this checked program as text without executing it in the backend.
// @ts-expect-error TypeScript models JS exports, not Bun's text import attribute.
import source from "./helpers/restore-helper.js" with { type: "text" };
export const RESTORE_HELPER_SCRIPT = source as string;
