package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
)

func TestSettingsNeedAnAdministrator(t *testing.T) {
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleOperator)
	for _, target := range []string{"/settings", "/diagnostics"} {
		if response := p.do("GET", target, operator, nil, nil); response.Code != http.StatusSeeOther {
			t.Fatal(target, response.Code)
		}
	}
	form := url.Values{"_csrf": {CSRFToken(operator.Token)}, "host": {"evil.example"}}
	if response := p.do("POST", "/settings/connection", operator, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden {
		t.Fatal(response.Code)
	}
}
