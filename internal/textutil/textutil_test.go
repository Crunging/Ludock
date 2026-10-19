package textutil

import "testing"

func TestLengthCountsUTF16Units(t *testing.T) {
	if Length("abc") != 3 || Length("ü") != 1 || Length("🙂") != 2 {
		t.Fatal(Length("abc"), Length("ü"), Length("🙂"))
	}
}

func TestCompare(t *testing.T) {
	if Compare("apple", "Banana") >= 0 || CompareFold("Éclair", "eclair") != 0 || Compare("a", "a") != 0 {
		t.Fatal("unexpected order")
	}
}
