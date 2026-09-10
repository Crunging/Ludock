// Keep the helper on the same Bun major as the application runtime in Dockerfile.
export const DEFAULT_HELPER_IMAGE = "oven/bun:1-alpine@sha256:d888c0ae6c86d7866ff10c5aafdd9077b36aee6455b33dd270fb93c0dd5cef6f";

export function getHelperImage(image = process.env.FILE_HELPER_IMAGE || DEFAULT_HELPER_IMAGE): string {
  if (image.trim() !== image || !/^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$/.test(image)) {
    throw new Error("FILE_HELPER_IMAGE must identify a trusted Bun helper by its immutable sha256 digest.");
  }
  return image;
}
