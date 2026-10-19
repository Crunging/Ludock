package datasize

import "testing"

func TestParseAndFormat(t *testing.T) {
	for input, want := range map[string]int64{"500 MB": 500_000_000, "2 GiB": 2 << 30, "1.5 GiB": 3 << 29, "10": 10, ".5 KB": 500} {
		if got, ok := Parse(input); !ok || got != want {
			t.Errorf("%q = %d %v", input, got, ok)
		}
	}
	for _, bad := range []string{"1.5 B", "-1", "GiB", "1 PB"} {
		if _, ok := Parse(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	for bytes, want := range map[int64]string{0: "0 B", 1536: "1.5 KiB", 1 << 30: "1 GiB", 1048575: "1 MiB", 5 << 30: "5 GiB"} {
		if got := Format(bytes); got != want {
			t.Errorf("%d = %q, want %q", bytes, got, want)
		}
	}
}
