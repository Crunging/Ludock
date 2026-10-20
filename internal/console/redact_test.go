package console

import (
	"strings"
	"testing"
)

func TestRedactsAcrossEverySplit(t *testing.T) {
	for _, c := range []struct {
		secrets  []string
		input    string
		expected string
	}{
		{[]string{"aba", "bab"}, "ababab", "[redacted][redacted]"},
		{[]string{"a.*[$]", "päss🔑word"}, "before a.*[$] päss🔑word after", "before [redacted] [redacted] after"},
	} {
		for split := 0; split <= len(c.input); split++ {
			for _, chunks := range [][]string{{c.input[:split], c.input[split:]}, strings.Split(c.input, "")} {
				var output strings.Builder
				redactor := NewRedactor(c.secrets, func(value string) { output.WriteString(value) })
				for _, chunk := range chunks {
					redactor.Push(chunk)
				}
				redactor.End()
				if output.String() != c.expected {
					t.Fatalf("%q split %v -> %q", c.input, chunks, output.String())
				}
			}
		}
	}
}

func TestRedactsDenseMatches(t *testing.T) {
	var output strings.Builder
	redactor := NewRedactor([]string{"absent-long-credential", "xy", "x"}, func(value string) { output.WriteString(value) })
	redactor.Push(strings.Repeat("xy", 4096) + "x")
	redactor.Push("y")
	redactor.End()
	if output.String() != strings.Repeat("[redacted]", 4097) {
		t.Fatal(len(output.String()))
	}
}

func TestHoldsBackOnlyTextThatCouldStartASecret(t *testing.T) {
	var output strings.Builder
	redactor := NewRedactor([]string{"rcon-secret"}, func(value string) { output.WriteString(value) })
	redactor.Push("Theo joined the game\n")
	if output.String() != "Theo joined the game\n" {
		t.Fatalf("held ordinary output: %q", output.String())
	}
	redactor.Push("password rcon-se")
	if output.String() != "Theo joined the game\npassword " {
		t.Fatalf("leaked a credential fragment: %q", output.String())
	}
	redactor.Push("cret\n")
	if output.String() != "Theo joined the game\npassword [redacted]\n" {
		t.Fatalf("got %q", output.String())
	}
}
