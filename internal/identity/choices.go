package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/Crunging/Ludock/internal/db"
)

// Choice is an administrator's decision in the panel to add or remove the
// container with one external identity. It lives only in Ludock's database,
// so it survives the container being recreated and never touches Docker.
type Choice struct {
	Managed bool `json:"managed"`
	// Game is the game picked when adding, when it differs from the one the
	// image is recognized as.
	Game string `json:"game,omitempty"`
}

// Choices are stored as settings keyed by this prefix and the external
// identity. listServers hides removed servers by the same key.
const choicePrefix = "server.choice:"

// Choices returns every saved choice by external identity.
func (s *Store) Choices(ctx context.Context) (map[string]Choice, error) {
	// ';' follows ':', so this reads exactly the keys with the prefix.
	rows, err := s.DB.QueryContext(ctx, "SELECT key, value_json FROM settings WHERE key > 'server.choice:' AND key < 'server.choice;'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	choices := map[string]Choice{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		var choice Choice
		if json.Unmarshal([]byte(value), &choice) == nil {
			choices[strings.TrimPrefix(key, choicePrefix)] = choice
		}
	}
	return choices, rows.Err()
}

// SetChoice saves an administrator's choice. The administrator's session and
// role are checked again in the same transaction, and check runs there too
// before anything is written.
func (s *Store) SetChoice(ctx context.Context, adminID, tokenHash, external string, choice Choice, check func(tx *sql.Tx) error) error {
	return s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		if check != nil {
			if err := check(tx); err != nil {
				return err
			}
		}
		return s.DB.SetSetting(ctx, tx, choicePrefix+external, choice)
	})
}
