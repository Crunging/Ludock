// Package textutil measures and orders user-facing text.
package textutil

import (
	"sync"
	"unicode/utf16"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// Length counts UTF-16 code units, as HTML minlength and maxlength do, so a
// server-side limit agrees with the form that enforces it in the browser.
func Length(value string) int {
	n := 0
	for _, r := range value {
		n += utf16.RuneLen(r)
	}
	return n
}

// A Collator keeps iteration buffers, so each comparison takes its own.
var (
	collator     = collators()
	foldCollator = collators(collate.IgnoreCase, collate.IgnoreDiacritics)
	// naturalCollator sorts "map2" before "map10".
	naturalCollator = collators(collate.IgnoreCase, collate.IgnoreDiacritics, collate.Numeric)
)

func collators(options ...collate.Option) *sync.Pool {
	return &sync.Pool{New: func() any { return collate.New(language.Und, options...) }}
}

func compare(pool *sync.Pool, left, right string) int {
	c := pool.Get().(*collate.Collator)
	defer pool.Put(c)
	return c.CompareString(left, right)
}

// Compare orders names for display with the root collation.
func Compare(left, right string) int { return compare(collator, left, right) }

// CompareFold orders names for display, ignoring case and accents.
func CompareFold(left, right string) int { return compare(foldCollator, left, right) }

// CompareNatural orders file names: case and accents ignored, digit runs
// compared as numbers.
func CompareNatural(left, right string) int { return compare(naturalCollator, left, right) }

// NaturalKeys returns sort keys that order bytewise like CompareNatural, for
// sorting many names without collating each pair again.
func NaturalKeys(values []string) [][]byte {
	c := naturalCollator.Get().(*collate.Collator)
	defer naturalCollator.Put(c)
	// One buffer holds every key: later keys are appended past earlier ones,
	// or into a new array that leaves them intact.
	var buffer collate.Buffer
	keys := make([][]byte, len(values))
	for index, value := range values {
		key := c.KeyFromString(&buffer, value)
		keys[index] = key[:len(key):len(key)]
	}
	return keys
}
