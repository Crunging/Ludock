package auth

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// IsExternalHTTPS reports whether the browser reached Ludock over HTTPS,
// directly or through a proxy that says so.
func IsExternalHTTPS(r *http.Request) bool {
	return slices.Contains(externalProtocols(r), "https:")
}

// IsSameOrigin reports whether the request's Origin is this panel. Fetch
// metadata decides when the browser sends it; otherwise the origin must match
// a host and protocol the request (or its proxy) names.
func IsSameOrigin(r *http.Request) bool {
	origin, ok := parseOrigin(r)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "same-origin":
		return true
	case "same-site", "cross-site":
		return false
	}
	return slices.Contains(externalProtocols(r), origin.protocol) &&
		slices.Contains(externalHosts(r), strings.ToLower(origin.host))
}

// OriginDiagnostic describes origin metadata without credentials, for debug
// logs about rejected requests.
func OriginDiagnostic(r *http.Request) map[string]string {
	origin, ok := parseOrigin(r)
	missing := func(value, fallback string) string {
		if value == "" {
			return fallback
		}
		return value
	}
	originHost, originProtocol := "missing-or-invalid", "missing-or-invalid"
	if ok {
		originHost, originProtocol = origin.host, origin.protocol
	}
	return map[string]string{
		"originHost":        originHost,
		"originProtocol":    originProtocol,
		"host":              missing(r.Host, "missing"),
		"forwardedHost":     missing(r.Header.Get("X-Forwarded-Host"), "missing"),
		"forwardedProtocol": missing(r.Header.Get("X-Forwarded-Proto"), "missing"),
		"fetchSite":         missing(r.Header.Get("Sec-Fetch-Site"), "missing"),
		"resolvedHosts":     missing(strings.Join(externalHosts(r), ","), "none"),
		"resolvedProtocols": missing(strings.Join(externalProtocols(r), ","), "none"),
	}
}

func externalProtocols(r *http.Request) []string {
	if r.TLS != nil {
		return []string{"https:"}
	}
	var result []string
	for _, value := range forwardedValues(r.Header.Get("X-Forwarded-Proto")) {
		protocol := ""
		switch strings.ToLower(value) {
		case "https", "wss":
			protocol = "https:"
		case "http", "ws":
			protocol = "http:"
		}
		if protocol != "" && !slices.Contains(result, protocol) {
			result = append(result, protocol)
		}
	}
	if len(result) == 0 {
		return []string{"http:"}
	}
	return result
}

type requestOrigin struct{ protocol, host string }

func parseOrigin(r *http.Request) (requestOrigin, bool) {
	value := r.Header.Get("Origin")
	if value == "" {
		return requestOrigin{}, false
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.Contains(value, "#") {
		return requestOrigin{}, false
	}
	host, ok := normalizeHost(u.Scheme+":", u.Host)
	if !ok {
		return requestOrigin{}, false
	}
	return requestOrigin{u.Scheme + ":", host}, true
}

// normalizeHost serializes host the way a browser URL does: lowercase, with
// the protocol's default port removed.
func normalizeHost(protocol, host string) (string, bool) {
	if host == "" || strings.ContainsAny(host, "/?#@\\ \t\r\n") {
		return "", false
	}
	name, port := host, ""
	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end < 0 {
			return "", false
		}
		name, port = host[:end+1], strings.TrimPrefix(host[end+1:], ":")
		if host[end+1:] != "" && !strings.HasPrefix(host[end+1:], ":") {
			return "", false
		}
	} else if index := strings.LastIndex(host, ":"); index >= 0 {
		name, port = host[:index], host[index+1:]
		if strings.Contains(name, ":") {
			return "", false
		}
	}
	if name == "" {
		return "", false
	}
	name = strings.ToLower(name)
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 0 || number > 65535 || strings.HasPrefix(port, "+") {
			return "", false
		}
		if (protocol == "http:" && number == 80) || (protocol == "https:" && number == 443) {
			port = ""
		} else {
			port = strconv.Itoa(number)
		}
	}
	if port == "" {
		return name, true
	}
	return name + ":" + port, true
}

func externalHosts(r *http.Request) []string {
	protocols := externalProtocols(r)
	forwardedHosts := forwardedValues(r.Header.Get("X-Forwarded-Host"))
	var ports []string
	for _, port := range forwardedValues(r.Header.Get("X-Forwarded-Port")) {
		if number, err := strconv.Atoi(port); err == nil && number >= 1 && number <= 65535 {
			ports = append(ports, port)
		}
	}
	raw := []string{r.Host}
	raw = append(raw, forwardedHosts...)
	for _, host := range forwardedHosts {
		if hostHasPort(host) {
			continue
		}
		for _, port := range ports {
			raw = append(raw, host+":"+port)
		}
	}
	var hosts []string
	add := func(value string) {
		if !slices.Contains(hosts, value) {
			hosts = append(hosts, value)
		}
	}
	for _, host := range raw {
		if host == "" {
			continue
		}
		add(strings.ToLower(host))
		for _, protocol := range protocols {
			if normalized, ok := normalizeHost(protocol, host); ok {
				add(normalized)
			}
		}
	}
	return hosts
}

func forwardedValues(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func hostHasPort(host string) bool {
	normalized, ok := normalizeHost("http:", host)
	if !ok {
		return false
	}
	if end := strings.LastIndex(normalized, "]"); end >= 0 {
		normalized = normalized[end+1:]
	}
	return strings.Contains(normalized, ":")
}
