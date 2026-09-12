import type { Operation } from "@ludock/shared";

import { operationActive, operationLabel } from "../operations";

export default function OperationList({
  operations,
  emptyMessage = "No operations yet.",
}: {
  operations: Operation[];
  emptyMessage?: string;
}) {
  return (
    <div className="table-scroll">
      <table className="data-table" aria-label="Recent operations">
        <thead>
          <tr>
            <th>Operation</th>
            <th>Started</th>
            <th>Progress / result</th>
          </tr>
        </thead>
        <tbody>
          {operations.length === 0 && (
            <tr>
              <td colSpan={3} className="muted">
                {emptyMessage}
              </td>
            </tr>
          )}
          {operations.map((operation) => (
            <tr key={operation.id}>
              <td className="capitalize">
                {operation.kind.replaceAll("_", " ")}
              </td>
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
