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

// Docker's own error text, as the Engine wrote it, keeps its meaning in a log
// line without the host paths and credentials it names.
func TestScrubsDockerWords(t *testing.T) {
	for input, want := range map[string]string{
		"error while creating mount source path '/srv/ludock/docker.sock': mkdir /srv/ludock/docker.sock: file exists":                                                                                                                                     "error while creating mount source path '[path]",
		`failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: exec: "/usr/local/bin/ludock": stat /usr/local/bin/ludock: no such file or directory: unknown`: `failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: exec: "[path]`,
		"Get \"https://registry.example/v2/\": dial tcp: lookup registry.example: no such host":                                                                                                                                                            "Get \"https://registry.example/v2/\": dial tcp: lookup registry.example: no such host",
		"Conflict. The container name \"/ludock-self-update-c4f4ca5435311deb\" is already in use":                                                                                                                                                          "Conflict. The container name \"[path]",
		"error while creating mount source path '/srv/Pat's Games/private': file exists":                                                                                                                                                                   "error while creating mount source path '[path]",
		"bind source path does not exist: /srv/ARK: Survival Evolved/private\nnext line /etc/x":                                                                                                                                                            "bind source path does not exist: [path]\nnext line [path]",
		"pull access denied for ghcr.io/crunging/ludock, token=synthetic-secret":                                                                                                                                                                           "pull access denied for ghcr.io/crunging/ludock, token=[REDACTED]",
	} {
		if got := Scrub(input); got != want {
			t.Errorf("%q\n got %q\nwant %q", input, got, want)
		}
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
