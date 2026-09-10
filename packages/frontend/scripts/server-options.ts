export function serverOptions(
  args: string[],
  defaultPort: number,
): { hostname: string; port: number } {
  let hostname = "127.0.0.1";
  let port = defaultPort;
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    if (argument === "--" || argument === "--strictPort") continue;
    if (argument === "--host") hostname = args[++index] || "";
    else if (argument === "--port") port = Number(args[++index]);
    else throw new Error(`Unknown frontend server option: ${argument}`);
  }
  if (!["127.0.0.1", "localhost", "::1"].includes(hostname)) {
    throw new Error("Frontend development and preview servers must use a loopback host.");
  }
  if (!Number.isInteger(port) || port < 1024 || port > 65535) {
    throw new Error("Frontend port must be an integer between 1024 and 65535.");
  }
  return { hostname, port };
}
