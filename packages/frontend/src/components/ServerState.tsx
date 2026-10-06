import type { Server } from "@ludock/shared";
import { serverStatus } from "../server-lifecycle";
import StatusPip from "./StatusPip";

/** The server's state as a pip and a plain word, colored by tone. */
export default function ServerState({ server }: {
  server: Pick<Server, "state" | "health"> & Partial<Pick<Server, "exit">>;
}) {
  const { label, tone } = serverStatus(server);
  return (
    <span className={`server-state server-state--${tone}`}>
      <StatusPip tone={tone} />
      {label}
    </span>
  );
}
