// The managed development runner assigns one identity to each checkout. The
// production image ignores this variable and keeps its ordinary cookie/API.
const configured = process.env.LUDOCK_DEV_INSTANCE || "";
export const developmentInstance =
  process.env.NODE_ENV === "development" && /^[a-f0-9]{12}$/.test(configured)
    ? configured
    : undefined;

export function matchesDevelopmentInstance(header: unknown): boolean {
  return (
    !developmentInstance ||
    header === undefined ||
    header === developmentInstance
  );
}
