package logging

import "regexp"

// SensitiveKeySource names credentials in container environments and
// application logs.
const SensitiveKeySource = "password|passwd|secret|token|credential|rconpw|authorization|cookie|api[-_]?key|session"

var (
	sensitiveKey        = regexp.MustCompile("(?i)" + SensitiveKeySource)
	secretKeyPattern    = regexp.MustCompile(`(?i)(` + SensitiveKeySource + `)(\s*[=:]\s*)[^\s,"';&}]+`)
	bearerPattern       = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;]+`)
	cookiePattern       = regexp.MustCompile(`(?i)\b(ludock_session)=([^;\s]+)`)
	sensitiveQuery      = regexp.MustCompile(`(?i)([?&](?:` + SensitiveKeySource + `)=)[^&#\s]+`)
	quotedSecretPattern = regexp.MustCompile(`(?i)(` + SensitiveKeySource + `)(["']?\s*[=:]\s*)("(?:\\[\s\S]|[^"\\])*(?:"|\\?$)|'(?:\\[\s\S]|[^'\\])*(?:'|\\?$))`)
)

func IsSensitiveKey(key string) bool { return sensitiveKey.MatchString(key) }

func Redact(value string) string {
	// Consume quoted values before token-oriented passes can remove a closing
	// quote. Escaped quotes are credential content, not the end of the value.
	value = quotedSecretPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := quotedSecretPattern.FindStringSubmatch(match)
		quote := parts[3][:1]
		return parts[1] + parts[2] + quote + "[REDACTED]" + quote
	})
	value = bearerPattern.ReplaceAllString(value, "Bearer [REDACTED]")
	value = cookiePattern.ReplaceAllString(value, "${1}=[REDACTED]")
	value = sensitiveQuery.ReplaceAllString(value, "${1}[REDACTED]")
	return secretKeyPattern.ReplaceAllString(value, "${1}${2}[REDACTED]")
}

// truncate shortens text to at most limit bytes without splitting a rune.
func truncate(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	cut := limit
	for cut > 0 && !utf8Start(value[cut]) {
		cut--
	}
	return value[:cut], true
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
