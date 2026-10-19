package auth

import (
	"net/http"
	"net/http/httptest"
)

func httptestRequestWithCookie(cookie string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", cookie)
	return r
}
