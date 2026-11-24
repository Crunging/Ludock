// Package mountinfo reads the mount table of the process's own container.
package mountinfo

import (
	"os"
	"strconv"
	"strings"
)

// Mount is one line of the table, with escapes undone.
type Mount struct {
	// Root is the mounted path within its source filesystem.
	Root  string
	Point string
}

// Table returns the raw table; tests replace it, since they can't mount.
var Table = func() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") }

func Read() ([]Mount, error) {
	table, err := Table()
	if err != nil {
		return nil, err
	}
	return Parse(string(table)), nil
}

// Parse reads mounts in table order, so a later mount at a point hides an
// earlier one. Root and Point are the fourth and fifth fields, before the
// optional fields and their " - " separator.
func Parse(table string) []Mount {
	var mounts []Mount
	for line := range strings.Lines(table) {
		// Only ASCII spaces separate fields; a path may hold other whitespace.
		fields := strings.Split(strings.TrimSuffix(line, "\n"), " ")
		if len(fields) < 5 {
			continue
		}
		mounts = append(mounts, Mount{Root: unescape(fields[3]), Point: unescape(fields[4])})
	}
	return mounts
}

// unescape undoes the kernel's octal escapes of space, tab, newline, and
// backslash.
func unescape(value string) string {
	var out strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '\\' && index+4 <= len(value) {
			if code, err := strconv.ParseUint(value[index+1:index+4], 8, 8); err == nil {
				out.WriteByte(byte(code))
				index += 3
				continue
			}
		}
		out.WriteByte(value[index])
	}
	return out.String()
}
