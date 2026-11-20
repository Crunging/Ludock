package db

import (
	"context"
	"fmt"
	"testing"
)

// Listing by prefix returns exactly the keys that start with it, in key
// order and without the prefix, and none that only sort beside it.
func TestSettingsWithPrefixBounds(t *testing.T) {
	d, _ := openTemp(t)
	ctx := context.Background()
	for _, key := range []string{"stage;", "stage:b", "stage", "stage:a", "stage:", "stag", "stage.x"} {
		if err := d.SetSetting(ctx, nil, key, key); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := d.SettingsWithPrefix(ctx, "stage:")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(listed); got != `[{ "stage:"} {a "stage:a"} {b "stage:b"}]` {
		t.Fatal(got)
	}
}
