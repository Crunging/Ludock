package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A server has 130 old finished operations and a few recent ones. Pruning
// removes old finished work Activity no longer links to, beyond the newest
// 100, and keeps work still running, the last run of a schedule, a pending
// automatic restart, and any operation an audit entry still names.
func TestPruneOperationsKeepsWhatIsStillShown(t *testing.T) {
	d, _ := openTemp(t)
	ctx := context.Background()
	old := Now() - (100 * 24 * time.Hour).Milliseconds()
	for _, statement := range []string{
		`INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES('u','u','x','admin',1,1)`,
		`INSERT INTO docker_hosts(id,name,created_at) VALUES('h','local',1)`,
		`INSERT INTO logical_servers(id,host_id,external_identity,display_name,game_type,status,binding_revision,binding_fingerprint,first_seen_at,last_seen_at)
      VALUES('s','h','standalone:mc','mc','minecraft','active',1,'',1,1)`,
	} {
		if _, err := d.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(id, status string, at int64) {
		if _, err := d.Exec(`INSERT INTO operations(id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at)
      VALUES(?,'s','u','backup',?,?,'{}',1,?,?)`, id, status, status, at, at); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 130 {
		status := "succeeded"
		switch i {
		case 1:
			status = "running"
		case 2:
			status = "queued"
		}
		insert(fmt.Sprintf("old-%03d", i), status, old+int64(i))
	}
	for i := range 5 {
		insert(fmt.Sprintf("new-%d", i), "failed", Now()+int64(i))
	}
	for _, statement := range []string{
		`INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at,changed_at,last_operation_id) VALUES('a','s','u','{}',1,1,1,'old-003')`,
		`INSERT INTO availability(server_id,policy_json,recovery_json,updated_at) VALUES('s','{}','{"operationId":"old-004","pending":true}',1)`,
		`INSERT INTO audit_log(action,target_type,target_id,details_json,created_at) VALUES('server.backup.succeeded','server','s','{"operationId":"old-005"}',1)`,
	} {
		if _, err := d.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.PruneOperations(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query("SELECT id FROM operations WHERE id BETWEEN 'old-000' AND 'old-099' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kept []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		kept = append(kept, id)
	}
	if fmt.Sprint(kept) != "[old-001 old-002 old-003 old-004 old-005 old-035 old-036 old-037 old-038 old-039 old-040 old-041 old-042 old-043 old-044 old-045 old-046 old-047 old-048 old-049 old-050 old-051 old-052 old-053 old-054 old-055 old-056 old-057 old-058 old-059 old-060 old-061 old-062 old-063 old-064 old-065 old-066 old-067 old-068 old-069 old-070 old-071 old-072 old-073 old-074 old-075 old-076 old-077 old-078 old-079 old-080 old-081 old-082 old-083 old-084 old-085 old-086 old-087 old-088 old-089 old-090 old-091 old-092 old-093 old-094 old-095 old-096 old-097 old-098 old-099]" {
		t.Fatal(kept)
	}
	var total int
	d.QueryRow("SELECT COUNT(*) FROM operations").Scan(&total)
	if total != 100+5 {
		t.Fatal(total)
	}
}
