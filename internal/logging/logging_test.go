package logging

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestRedactsCredentials(t *testing.T) {
	redacted := Redact(strings.Join([]string{"Authorization: Bearer header-secret", "ludock_session=cookie-secret; Path=/",
		`payload={"password":"json-secret"}`, "https://example.test/path?api_key=query-secret&safe=value"}, "\n"))
	for _, secret := range []string{"header-secret", "cookie-secret", "json-secret", "query-secret"} {
		if strings.Contains(redacted, secret) {
			t.Errorf("leaked %s: %s", secret, redacted)
		}
	}
	if !strings.Contains(redacted, "safe=value") {
		t.Fatal(redacted)
	}
}

func TestRedactsQuotedCredentials(t *testing.T) {
	leak := regexp.MustCompile(`synthetic|secret words|credential-tail|unfinished secret`)
	for _, input := range []string{
		`password="synthetic secret words" action=connect`,
		`token='synthetic secret words' action=connect`,
		`password="synthetic\"credential-tail" action=connect`,
		`secret='synthetic\'credential-tail' action=connect`,
		`authorization="Bearer synthetic secret words" action=connect`,
		`password="synthetic unfinished secret`,
		`password="synthetic unfinished secret\`,
	} {
		redacted := Redact(input)
		if leak.MatchString(redacted) || !strings.Contains(redacted, "[REDACTED]") ||
			(strings.HasSuffix(input, "action=connect") && !strings.Contains(redacted, "action=connect")) {
			t.Errorf("%q -> %q", input, redacted)
		}
	}
	encoded, _ := json.Marshal(map[string]any{"password": "synthetic\"credential-tail\\", "safe": true})
	var decoded map[string]any
	if json.Unmarshal([]byte(Redact(string(encoded))), &decoded) != nil || decoded["password"] != "[REDACTED]" || decoded["safe"] != true {
		t.Fatal(Redact(string(encoded)))
	}
}

func TestConsoleIsRedacted(t *testing.T) {
	var stdout, stderr bytes.Buffer
	SetOutput(&stdout, &stderr)
	defer SetOutput(os.Stdout, os.Stderr)
	New("test").Warn("ludock_session=development-secret; Path=/; HttpOnly",
		F("detail", "theme=dark; ludock_session=context-secret"), F("apiKey", "unlabelled-key-secret"), F("count", 3))
	line := stderr.String()
	if regexp.MustCompile(`development-secret|context-secret|unlabelled-key-secret`).MatchString(line) {
		t.Fatalf("leaked: %s", line)
	}
	if !strings.Contains(line, "ludock_session=[REDACTED]; Path=/; HttpOnly") ||
		!strings.Contains(line, `{"detail":"theme=dark; ludock_session=[REDACTED]","apiKey":"[REDACTED]","count":3}`) ||
		!strings.Contains(line, " WARN [test] ") {
		t.Fatalf("%q", line)
	}
}

func TestDebugIsFilteredByLevel(t *testing.T) {
	var stdout bytes.Buffer
	SetOutput(&stdout, &stdout)
	defer SetOutput(os.Stdout, os.Stderr)
	SetLevel(LevelInfo)
	New("test").Debug("hidden")
	if stdout.Len() != 0 {
		t.Fatal(stdout.String())
	}
	if level, invalid := ParseLevel(" DEBUG "); level != LevelDebug || invalid != "" {
		t.Fatal(level, invalid)
	}
	if level, invalid := ParseLevel("verbose"); level != LevelInfo || invalid != "verbose" {
		t.Fatal(level, invalid)
	}
}
