package views

import (
	"context"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
)

// The only administrator who can sign in is offered nothing Ludock would
// refuse: no Disable, Make member, or Delete. A second administrator makes
// each of them possible again, except deleting your own account.
func TestUsersLastAdministratorOffersNoRefusal(t *testing.T) {
	render := func(users []db.User) string {
		var out strings.Builder
		if err := userRow(UsersView{Users: users, CurrentID: "a"}, users[0]).Render(context.Background(), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	admin := db.User{ID: "a", Username: "logan", Role: db.RoleAdmin, PasswordSet: true}
	invited := db.User{ID: "b", Username: "theo", Role: db.RoleAdmin}
	for _, text := range []string{">Disable", ">Make member", ">Delete"} {
		if row := render([]db.User{admin, invited}); strings.Contains(row, text) {
			t.Errorf("the only administrator who can sign in is offered %s", text)
		}
	}
	invited.PasswordSet = true
	row := render([]db.User{admin, invited})
	if !strings.Contains(row, ">Disable") || !strings.Contains(row, ">Make member") || strings.Contains(row, ">Delete") {
		t.Errorf("with a second administrator: %s", row)
	}
}
