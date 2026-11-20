package db

import (
	"context"
	"time"
)

// operationsKeptPerServer covers the recent work Needs attention reads.
const operationsKeptPerServer = 100

// operationKeepFor keeps recent work regardless of its audit entries.
const operationKeepFor = 90 * 24 * time.Hour

// PruneOperations removes finished operations Activity no longer shows: past
// the age and per-server count kept, with no audit entry left that links to
// them, and not the last run of a schedule or a pending automatic restart.
func (d *DB) PruneOperations(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `DELETE FROM operations WHERE id IN (
      SELECT id FROM (SELECT id, status, updated_at,
        ROW_NUMBER() OVER (PARTITION BY server_id ORDER BY created_at DESC, id DESC) AS newer FROM operations) o
      WHERE newer > ? AND updated_at < ? AND status NOT IN ('queued','running')
        AND NOT EXISTS (SELECT 1 FROM schedules s WHERE s.last_operation_id = o.id)
        AND NOT EXISTS (SELECT 1 FROM settings WHERE key >= 'monitor.recovery:' AND key < 'monitor.recovery;'
          AND json_valid(value_json) AND json_extract(value_json, '$.operationId') = o.id)
        AND NOT EXISTS (SELECT 1 FROM audit_log a WHERE `+AuditOperationID("a.")+` = o.id))`,
		operationsKeptPerServer, Now()-operationKeepFor.Milliseconds())
	return err
}
