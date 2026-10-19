// Package jscompat reproduces JavaScript encodings that persisted Ludock data
// depends on, so values written by v0.5.0 keep matching.
package jscompat

import (
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// Length returns a string's length in UTF-16 code units, like String.length.
func Length(value string) int {
	n := 0
	for _, r := range value {
		n += utf16.RuneLen(r)
	}
	return n
}

// Quote encodes a string like JSON.stringify.
func Quote(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				hex := strconv.FormatInt(int64(r), 16)
				if len(hex) < 2 {
					b.WriteByte('0')
				}
				b.WriteString(hex)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

const uriUnreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"

// EncodeURIComponent percent-encodes like encodeURIComponent.
func EncodeURIComponent(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if strings.IndexByte(uriUnreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteString(strings.ToUpper(strconv.FormatInt(int64(c)>>4, 16)))
		b.WriteString(strings.ToUpper(strconv.FormatInt(int64(c)&15, 16)))
	}
	return b.String()
}

var collator = collate.New(language.Und)

// LocaleCompare orders strings like String.prototype.localeCompare with the
// root collation.
func LocaleCompare(left, right string) int {
	return collator.CompareString(left, right)
}

// NormalizePosix normalizes a path like Node's path.posix.normalize, which
// keeps a trailing slash and never resolves above a relative start.
func NormalizePosix(path string) string {
	if path == "" {
		return "."
	}
	absolute := path[0] == '/'
	trailing := path[len(path)-1] == '/'
	path = normalizeString(path, !absolute)
	if path == "" {
		if absolute {
			return "/"
		}
		if trailing {
			return "./"
		}
		return "."
	}
	if trailing {
		path += "/"
	}
	if absolute {
		return "/" + path
	}
	return path
}

func normalizeString(path string, allowAboveRoot bool) string {
	var segments []string
	for _, segment := range strings.Split(path, "/") {
		switch segment {
		case "", ".":
		case "..":
			if len(segments) > 0 && segments[len(segments)-1] != ".." {
				segments = segments[:len(segments)-1]
			} else if allowAboveRoot {
				segments = append(segments, "..")
			}
		default:
			segments = append(segments, segment)
		}
	}
	return strings.Join(segments, "/")
}
