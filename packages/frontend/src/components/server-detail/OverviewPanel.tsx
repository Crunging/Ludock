import { formatByteSize, type Schedule, type Server, type ServerStats } from "@ludock/shared";
import type { ReactNode } from "react";
import { formatDateTime, formatRelativeTime, formatRelativeTimeSentence } from "../../format";
import { connectAddress, exitDetail, stateSinceText } from "../../server-lifecycle";
import CopyAddress from "../CopyAddress";
import ServerState from "../ServerState";

const MIB = 1024 * 1024;
const healthNotes = {
  starting: "Health check is starting",
  healthy: "Health check passing",
  unhealthy: "Health check failing",
} as const;

interface Props {
  server: Server;
  stats: ServerStats | null;
  liveUnavailable: boolean;
  showBackup: boolean;
  onOpenBackups?: () => void;
  /** Undefined when the viewer cannot see schedules; null when none is due. */
  nextSchedule?: Schedule | null;
  onOpenSchedules?: () => void;
  children?: ReactNode;
}

function Timestamp({ value, standalone = false }: { value: number; standalone?: boolean }) {
  return (
    <time dateTime={new Date(value).toISOString()} title={formatDateTime(value)}>
      {standalone ? formatRelativeTimeSentence(value) : formatRelativeTime(value)}
    </time>
  );
}

export default function OverviewPanel(props: Props) {
  const { server, stats, liveUnavailable, showBackup, nextSchedule, children } = props;
  const ports = server.ports.filter((port) => port.public > 0);
  const address = connectAddress(server);
  const memoryShare = stats && stats.memLimitMB > 0
    ? Math.min(100, (stats.memUsageMB / stats.memLimitMB) * 100)
    : null;
  const resourcesUnavailable = liveUnavailable
    ? "Unavailable until Docker can be reached"
    : "Shown while the server is running";

  return (
    <>
      <h2 className="sr-only">Overview</h2>
      <dl className="overview-facts">
        <div>
          <dt>State</dt>
          <dd>
            <ServerState server={server} />
            {server.stateSince !== null && (
              <time
                className="overview-since"
                dateTime={new Date(server.stateSince).toISOString()}
                title={formatDateTime(server.stateSince)}
              >
                {stateSinceText(server)}
              </time>
            )}
            {server.health && <span className="muted">{healthNotes[server.health]}</span>}
            {exitDetail(server) && <span className="muted">{exitDetail(server)}</span>}
          </dd>
        </div>
        <div>
          <dt>Connect</dt>
          <dd>
            {address ? (
              <>
                <CopyAddress address={address} serverName={server.displayName} />
                {server.connection?.host === null && (
                  <span className="muted">Using this page’s address</span>
                )}
              </>
            ) : "No published port"}
          </dd>
        </div>
        <div>
          <dt>Ports</dt>
          <dd>
            {ports.length > 0 ? (
              <ul className="overview-ports">
                {ports.map((port) => (
                  <li key={`${port.public}/${port.type}/${port.private}`}>
                    <code>{port.public}/{port.type}</code>
                    {port.private !== port.public && (
                      <span className="muted">container port {port.private}</span>
                    )}
                  </li>
                ))}
              </ul>
            ) : "None published"}
          </dd>
        </div>
        <div>
          <dt>CPU</dt>
          <dd>
            {stats ? (
              <>
                <span className="overview-figure">{stats.cpuPercent.toFixed(1)}%</span>
                <span className="muted">100% is one core</span>
              </>
            ) : <span className="muted">{resourcesUnavailable}</span>}
          </dd>
        </div>
        <div>
          <dt>Memory</dt>
          <dd>
            {stats ? (
              <>
                <span className="overview-figure">
                  {formatByteSize(Math.round(stats.memUsageMB * MIB))}
                </span>
                {memoryShare !== null && (
                  <>
                    <span className="overview-meter" aria-hidden="true">
                      <span style={{ width: `${memoryShare}%` }} />
                    </span>
                    <span className="muted">of {formatByteSize(Math.round(stats.memLimitMB * MIB))}</span>
                  </>
                )}
              </>
            ) : <span className="muted">{resourcesUnavailable}</span>}
          </dd>
        </div>
        {showBackup && (
          <div>
            <dt>Last backup</dt>
            <dd>
              {server.latestBackup ? (
                <>
                  <Timestamp value={server.latestBackup.createdAt} standalone />
                  <span className="muted">{formatByteSize(server.latestBackup.size)}</span>
                </>
              ) : "Never"}
              {props.onOpenBackups && (
                <button className="text-link" onClick={props.onOpenBackups}>Backups</button>
              )}
            </dd>
          </div>
        )}
        {nextSchedule !== undefined && (
          <div>
            <dt>Next scheduled</dt>
            <dd>
              {nextSchedule?.nextRunAt ? (
                <>
                  <span className="capitalize">{nextSchedule.action}</span>
                  <Timestamp value={nextSchedule.nextRunAt} />
                </>
              ) : "Nothing scheduled"}
              {props.onOpenSchedules && (
                <button className="text-link" onClick={props.onOpenSchedules}>Schedules</button>
              )}
            </dd>
          </div>
        )}
        <div>
          <dt>Image</dt>
          <dd><code>{server.image || "Unknown"}</code></dd>
        </div>
      </dl>
      {children}
    </>
  );
}
