// Programs run inside disposable helper containers with `bun -e`. Bun embeds
// each checked source as text without executing it in the backend.
// @ts-expect-error TypeScript models JS exports, not Bun's text import attribute.
import fileHelper from "./helpers/file-helper.js" with { type: "text" };
// @ts-expect-error See above.
import mountIdentities from "./helpers/mount-identities.js" with { type: "text" };
// @ts-expect-error See above.
import mountProof from "./helpers/mount-proof.js" with { type: "text" };
// @ts-expect-error See above.
import restoreExtract from "./helpers/restore-extract.js" with { type: "text" };
// @ts-expect-error See above.
import restoreHelper from "./helpers/restore-helper.js" with { type: "text" };

export const FILE_HELPER_SCRIPT = fileHelper as string;
export const MOUNT_IDENTITIES_SCRIPT = mountIdentities as string;
export const MOUNT_PROOF_SCRIPT = mountProof as string;
export const RESTORE_EXTRACT_SCRIPT = restoreExtract as string;
export const RESTORE_HELPER_SCRIPT = restoreHelper as string;
