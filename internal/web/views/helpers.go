// Package views holds Ludock's templ pages and fragments.
package views

import (
	"encoding/json"
	"net/url"
	"strconv"
)

// Asset maps an asset name to its versioned URL; set by the web package.
var Asset = func(name string) string { return "/assets/" + name }

func csrfHeaders(token string) string {
	if token == "" {
		return ""
	}
	encoded, _ := json.Marshal(map[string]string{"X-CSRF-Token": token})
	return string(encoded)
}

func urlQuery(value string) string { return url.QueryEscape(value) }

func intText(value int) string { return strconv.Itoa(value) }

func int64Text(value int64) string { return strconv.FormatInt(value, 10) }
