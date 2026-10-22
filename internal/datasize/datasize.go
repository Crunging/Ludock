// Package datasize parses and formats byte sizes.
package datasize

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

var (
	pattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d*)?|\.\d+)\s*(B|[KMGT]i?B)?$`)
	units   = map[string]int64{"b": 1, "kb": 1000, "mb": 1000 * 1000, "gb": 1000 * 1000 * 1000, "tb": 1000 * 1000 * 1000 * 1000,
		"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40}
	binary = []string{"B", "KiB", "MiB", "GiB", "TiB"}
)

// Parse reads sizes such as "500 MB", "2 GiB", or "1.5 GiB" without rounding
// away fractions of a byte.
func Parse(value string) (int64, bool) {
	match := pattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, false
	}
	whole, fraction, _ := strings.Cut(match[1], ".")
	unit := strings.ToLower(match[2])
	if unit == "" {
		unit = "b"
	}
	numerator, ok := new(big.Int).SetString(whole+fraction, 10)
	if !ok {
		return 0, false
	}
	numerator.Mul(numerator, big.NewInt(units[unit]))
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(fraction))), nil)
	quotient, remainder := new(big.Int).QuoRem(numerator, denominator, new(big.Int))
	if remainder.Sign() != 0 || quotient.Cmp(big.NewInt(1<<53-1)) > 0 {
		return 0, false
	}
	return quotient.Int64(), true
}

// Format is rounded display text in binary units.
func Format(bytes int64) string {
	if bytes < 0 {
		return "—"
	}
	value := float64(bytes)
	unit := 0
	for value >= 1024 && unit < len(binary)-1 {
		value /= 1024
		unit++
	}
	rounded := math.Round(value*100) / 100
	if rounded == 1024 && unit < len(binary)-1 {
		rounded, unit = 1, unit+1
	}
	return strconv.FormatFloat(rounded, 'f', -1, 64) + " " + binary[unit]
}
