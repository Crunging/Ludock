import type { Operation } from "@ludock/shared";

import { historyActorLabel, operationActive, operationLabel } from "../operations";
import { NavLink } from "../navigation";

export default function OperationList({
  operations,
  emptyMessage = "No operations yet.",
  showContext = false,
  serverNames = new Map<string, string>(),
}: {
  operations: Operation[];
  emptyMessage?: string;
  showContext?: boolean;
  serverNames?: ReadonlyMap<string, string>;
}) {
  return (
    <div className="table-scroll">
      <table className="data-table" aria-label={showContext ? "Operation history" : "Recent operations"}>
        <thead>
          <tr>
            <th>Operation</th>
            {showContext && <th>Server / actor</th>}
            <th>Started</th>
            <th>Progress / result</th>
          </tr>
        </thead>
        <tbody>
          {operations.length === 0 && (
            <tr>
              <td colSpan={showContext ? 4 : 3} className="muted">
                {emptyMessage}
              </td>
            </tr>
          )}
          {operations.map((operation) => (
            <tr key={operation.id}>
              <td className="capitalize">
                <NavLink className="text-link" to={`/operations/${encodeURIComponent(operation.id)}`}>
                  {operation.kind.replaceAll("_", " ")}
                </NavLink>
              </td>
              {showContext && <td className="operation-server">
                <NavLink className="text-link" to={`/servers/${encodeURIComponent(operation.serverId)}`}>
                  {serverNames.get(operation.serverId) ?? operation.serverId}
                </NavLink>
                <p className="muted">{historyActorLabel(operation.actor)}</p>
              </td>}
              <td>{new Date(operation.createdAt).toLocaleString()}</td>
              <td>
                <span role={operationActive(operation) ? "status" : undefined}>
                  {operationLabel(operation)}
                </span>
                {operation.error && (
                  <p className="operation-error">{operation.error}</p>
                )}
                {operation.result &&
                  typeof operation.result.guidance === "string" && (
                    <p className="muted">{operation.result.guidance}</p>
                  )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
