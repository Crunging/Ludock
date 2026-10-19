package db_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
)

var ctx = context.Background()

type v050 struct {
	CredentialFingerprint    string `json:"credentialFingerprint"`
	ComposeSourceFingerprint string `json:"composeSourceFingerprint"`
}

func v050Vectors(t *testing.T) v050 {
	var vectors v050
	if err := json.Unmarshal(dbtest.V050Vectors(t), &vectors); err != nil {
		t.Fatal(err)
	}
	return vectors
}

func TestOpensV050Database(t *testing.T) {
	d := dbtest.OpenV050(t)
	users, err := d.ListUsers(ctx)
	if err != nil || len(users) != 2 || users[0].Username != "Admin" || users[0].Role != db.RoleAdmin {
		t.Fatalf("users = %+v, %v", users, err)
	}
	found, _ := d.FindUserByUsername(ctx, "admin")
	if found == nil || !strings.HasPrefix(found.PasswordHash, "$argon2id$v=19$m=65536,t=2,p=1$") {
		t.Fatalf("user = %+v", found)
	}
	var grants string
	d.QueryRow("SELECT capabilities_json FROM server_grants").Scan(&grants)
	if grants != `["server.view","logs.read"]` {
		t.Fatalf("grants = %s", grants)
	}
	if got := d.CredentialFingerprint(strings.Repeat("x", 40)); got != v050Vectors(t).CredentialFingerprint {
		t.Fatalf("credential fingerprint = %s", got)
	}
	if got, _ := d.ComposeSourceFingerprint(strings.Repeat("c", 64)); got != v050Vectors(t).ComposeSourceFingerprint {
		t.Fatalf("compose fingerprint = %s", got)
	}
}
