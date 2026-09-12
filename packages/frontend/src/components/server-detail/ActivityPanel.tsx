import type { Operation, OperationStatus } from "@ludock/shared";
import { useRef } from "react";
import OperationList from "../OperationList";
import ScheduledOperationPanel from "./ScheduledOperationPanel";
import { operationActive, operationStatusLabels } from "../../operations";

export interface ActivityFilters {
  status: OperationStatus | "all";
  kind: string;
}

interface Props {
  operations: Operation[];
  admin: boolean;
  onRefresh: () => void;
  onRecreate: () => void;
  serverId: string;
  selectedOperationId: string | null;
  onCloseOperation: () => void;
  filters: ActivityFilters;
  onFiltersChange: (filters: ActivityFilters) => void;
}

export default function ActivityPanel(props: Props) {
  const { operations, admin, onRefresh, onRecreate, serverId, selectedOperationId, onCloseOperation, filters, onFiltersChange } = props;
  const statusInput = useRef<HTMLSelectElement>(null);
  const visible = operations.filter((operation) =>
    (filters.status === "all" || operation.status === filters.status) &&
    (filters.kind === "all" || operation.kind === filters.kind));
  const kinds = [...new Set([
    ...operations.map((operation) => operation.kind),
    ...(filters.kind === "all" ? [] : [filters.kind]),
  ])].sort();
  const filtered = filters.status !== "all" || filters.kind !== "all";
  const hiddenActive = operations.some((operation) => operationActive(operation) && !visible.includes(operation));
  const clearFilters = () => {
    onFiltersChange({ status: "all", kind: "all" });
    statusInput.current?.focus();
  };
  return (
    <>
      {selectedOperationId && (
        <ScheduledOperationPanel
          key={selectedOperationId}
          operationId={selectedOperationId}
          serverId={serverId}
          onClose={onCloseOperation}
        />
      )}
      <div className="section-heading">
        <h2 id="recent-operations-title" tabIndex={-1}>Recent operations</h2>
        <button className="secondary-btn" onClick={onRefresh}>
          Refresh
        </button>
      </div>
      <div className="activity-filters stack-form" role="group" aria-label="Filter recent operations">
        <label>
          <span>Status</span>
          <select ref={statusInput} value={filters.status} onChange={(event) => onFiltersChange({ ...filters, status: event.target.value as ActivityFilters["status"] })}>
            <option value="all">All statuses</option>
            {Object.entries(operationStatusLabels).map(([status, label]) => <option key={status} value={status}>{label}</option>)}
          </select>
        </label>
        <label>
          <span>Operation</span>
          <select value={filters.kind} onChange={(event) => onFiltersChange({ ...filters, kind: event.target.value })}>
            <option value="all">All operations</option>
            {kinds.map((kind) => <option key={kind} value={kind}>{kind.replaceAll("_", " ")}</option>)}
          </select>
        </label>
        {filtered && <button type="button" className="secondary-btn" onClick={clearFilters}>Clear filters</button>}
      </div>
      <p className="section-note activity-filter-count" aria-live="polite">Showing {visible.length} of {operations.length} recent operations.</p>
      {hiddenActive && (
        <p className="section-note activity-filter-notice">
          An active operation is hidden by these filters. Server controls remain paused.{" "}
          <button type="button" className="text-link" onClick={clearFilters}>Show active work</button>
        </p>
      )}
      <OperationList operations={visible} emptyMessage={operations.length > 0 ? "No recent operations match these filters." : undefined} />
      {admin &&
        operations.some(
          (operation) =>
            operation.kind === "update" &&
            operation.status === "already_current",
        ) && (
          <p className="section-note">
            The configured image is current. Game software may update during
            startup.{" "}
            <button className="text-link" onClick={onRecreate}>
              Recreate anyway
            </button>
          </p>
        )}
    </>
  );
}
