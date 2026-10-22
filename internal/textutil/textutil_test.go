package textutil

import (
	"bytes"
	"sync"
	"testing"
)

func TestLengthCountsUTF16Units(t *testing.T) {
	if Length("abc") != 3 || Length("ü") != 1 || Length("🙂") != 2 {
		t.Fatal(Length("abc"), Length("ü"), Length("🙂"))
	}
}

func TestCompare(t *testing.T) {
	if CompareNatural("map2", "Map10") >= 0 || CompareNatural("é", "E") != 0 {
		t.Fatal("natural order")
	}
	if Compare("apple", "Banana") >= 0 || CompareFold("Éclair", "eclair") != 0 || Compare("a", "a") != 0 {
		t.Fatal("unexpected order")
	}
}

// Natural keys order every pair of file names as CompareNatural does.
func TestNaturalKeyMatchesCompareNatural(t *testing.T) {
	names := []string{"map2", "Map10", "map10", "é", "E", "e", "level.dat", "Level.DAT", "world_nether", "world",
		"10", "9", "a1b2", "a1b10", "", "Zürich", "zurich", "日本", "🙂", "file 1", "file  1", "file01", "file1"}
	for _, left := range names {
		for _, right := range names {
			if got, want := sign(bytes.Compare(NaturalKey(left), NaturalKey(right))), sign(CompareNatural(left, right)); got != want {
				t.Errorf("%q vs %q: key %d, compare %d", left, right, got, want)
			}
		}
	}
}

// Concurrent comparisons each use their own collator.
func TestCompareConcurrently(t *testing.T) {
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 1000 {
				if CompareNatural("map2", "map10") >= 0 || CompareFold("Éclair", "eclair") != 0 {
					t.Error("unexpected order")
					return
				}
			}
		}()
	}
	group.Wait()
}

func sign(value int) int {
	switch {
	case value < 0:
		return -1
	case value > 0:
		return 1
	}
	return 0
}
