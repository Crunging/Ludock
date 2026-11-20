package db

import (
	"context"
	"testing"
)

// Work recorded before it runs ends as one entry with its outcome, and an
// operation's queued entry becomes its outcome too. An entry pruned
// meanwhile is written again rather than lost.
func TestFinishAuditKeepsOneEntryPerWork(t *testing.T) {
	d, _ := openTemp(t)
	ctx := context.Background()
	actions := func() []string {
		rows, err := d.QueryContext(ctx, "SELECT action FROM audit_log ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []string
		for rows.Next() {
			var action string
			rows.Scan(&action)
			result = append(result, action)
		}
		return result
	}
	entry := AuditEntry{Action: "server.game-command.started", TargetType: "server", TargetID: "s1"}
	started, err := d.WriteAuditStart(ctx, entry)
	if err != nil {
		t.Fatal(err)
	}
	entry.Action = "server.game-command.succeeded"
	if err := d.FinishAudit(ctx, started, entry); err != nil {
		t.Fatal(err)
	}
	if got := actions(); len(got) != 1 || got[0] != "server.game-command.succeeded" {
		t.Fatal(got)
	}

	queued := AuditEntry{Action: "server.backup.queued", TargetType: "server", TargetID: "s1", Details: map[string]string{"operationId": "op-1"}}
	if err := d.WriteAudit(ctx, nil, queued); err != nil {
		t.Fatal(err)
	}
	done := AuditEntry{Action: "server.backup.succeeded", TargetType: "server", TargetID: "s1", Details: map[string]any{"operationId": "op-1"}}
	if err := d.FinishOperationAudit(ctx, d.DB, "op-1", "server.backup.queued", done); err != nil {
		t.Fatal(err)
	}
	if got := actions(); len(got) != 2 || got[1] != "server.backup.succeeded" {
		t.Fatal(got)
	}

	if _, err := d.ExecContext(ctx, "DELETE FROM audit_log"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishAudit(ctx, started, entry); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishOperationAudit(ctx, d.DB, "op-1", "server.backup.queued", done); err != nil {
		t.Fatal(err)
	}
	if got := actions(); len(got) != 2 || got[0] != "server.game-command.succeeded" || got[1] != "server.backup.succeeded" {
		t.Fatal(got)
	}
}
