import { parseByteSize } from "@ludock/shared";

export function getMaxUploadBytes(configured = process.env.MAX_UPLOAD_SIZE): number {
  const size = configured?.trim();
  if (!size) return 2 * 1024 ** 3;
  const bytes = parseByteSize(size);
  if (bytes !== null && bytes > 0) return bytes;
  throw new Error(
    "Invalid MAX_UPLOAD_SIZE: use a positive size such as 500 MB, 2 GiB, or 1.5 GiB",
  );
}
