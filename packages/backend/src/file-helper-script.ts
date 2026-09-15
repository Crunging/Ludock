// Bun embeds this checked program as text without executing it in the backend.
// @ts-expect-error TypeScript models JS exports, not Bun's text import attribute.
import source from "./helpers/file-helper.js" with { type: "text" };
export const FILE_HELPER_SCRIPT = source as string;
