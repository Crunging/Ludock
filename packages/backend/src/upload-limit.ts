import { parseByteSize } from "@ludock/shared";

export function getMaxUploadBytes(
  env: { MAX_UPLOAD_SIZE?: string; MAX_UPLOAD_BYTES?: string } = process.env,
): number {
  const size = env.MAX_UPLOAD_SIZE?.trim();
  if (size) {
    const bytes = parseByteSize(size);
    if (bytes !== null && bytes > 0) return bytes;
    throw new Error(
      "Invalid MAX_UPLOAD_SIZE: use a positive size such as 500 MB, 2 GiB, or 1.5 GiB",
    );
  }

  const legacy = env.MAX_UPLOAD_BYTES?.trim();
  if (legacy) {
    const bytes = Number(legacy);
    if (/^\d+$/.test(legacy) && Number.isSafeInteger(bytes) && bytes > 0)
      return bytes;
    throw new Error(
      "Invalid MAX_UPLOAD_BYTES: use a positive whole number of bytes, or set MAX_UPLOAD_SIZE=500 MB instead",
    );
  }

  return 2 * 1024 ** 3;
}
