import type { Operation } from "@ludock/shared";
import OperationList from "../OperationList";

interface Props {
  operations: Operation[];
  admin: boolean;
  onRefresh: () => void;
  onRecreate: () => void;
}

export default function ActivityPanel(props: Props) {
  const { operations, admin, onRefresh, onRecreate } = props;
  return (
    <>
      <div className="section-heading">
        <h2>Recent operations</h2>
        <button className="secondary-btn" onClick={onRefresh}>
          Refresh
        </button>
      </div>
      <OperationList operations={operations} />
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
