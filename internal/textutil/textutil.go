// Package textutil measures and orders user-facing text.
package textutil

import (
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

var (
	collator     = collate.New(language.Und)
	foldCollator = collate.New(language.Und, collate.IgnoreCase, collate.IgnoreDiacritics)
	// naturalCollator sorts "map2" before "map10".
	naturalCollator = collate.New(language.Und, collate.IgnoreCase, collate.IgnoreDiacritics, collate.Numeric)
)

// Compare orders names for display with the root collation.
func Compare(left, right string) int { return collator.CompareString(left, right) }

// CompareFold orders names for display, ignoring case and accents.
func CompareFold(left, right string) int { return foldCollator.CompareString(left, right) }

// CompareNatural orders file names: case and accents ignored, digit runs
// compared as numbers.
func CompareNatural(left, right string) int { return naturalCollator.CompareString(left, right) }
