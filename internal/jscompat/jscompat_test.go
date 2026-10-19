package jscompat

import (
	"slices"
	"sort"
	"testing"
)

// Expected values were produced by Bun 1.4.3.
func TestMatchesJavaScript(t *testing.T) {
	inputs := []string{"/srv/mc//data/", "a/../..", "/../a", "", "./", "a/./b/../c/", "//x"}
	want := []string{"/srv/mc/data/", "..", "/a", ".", "./", "a/c/", "/x"}
	for index, input := range inputs {
		if got := NormalizePosix(input); got != want[index] {
			t.Errorf("NormalizePosix(%q) = %q, want %q", input, got, want[index])
		}
	}
	if got := EncodeURIComponent("games stack/é!*()~'"); got != "games%20stack%2F%C3%A9!*()~'" {
		t.Error(got)
	}
	if got := Quote("a <\x01\x7f\"\\\n"); got != "\"a <\\u0001\x7f\\\"\\\\\\n\"" {
		t.Errorf("%q", got)
	}
	words := []string{"b", "A", "a", "_x", "1", "é", "e", "Z"}
	sort.SliceStable(words, func(i, j int) bool { return LocaleCompare(words[i], words[j]) < 0 })
	if !slices.Equal(words, []string{"_x", "1", "a", "A", "b", "e", "é", "Z"}) {
		t.Error(words)
	}
	if Length("🙂é") != 3 {
		t.Error("UTF-16 length")
	}
}
