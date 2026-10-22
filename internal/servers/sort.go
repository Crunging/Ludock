package servers

import (
	"sort"
	"strconv"
	"strings"
)

// sortBindings orders ports the way Docker's JSON lists them: by the
// "port/proto" key, then mapping order.
func sortBindings(bindings []binding) {
	key := func(b binding) string { return strconv.Itoa(b.Private) + "/" + b.Type }
	sort.SliceStable(bindings, func(i, j int) bool { return strings.Compare(key(bindings[i]), key(bindings[j])) < 0 })
}

func sortByLengthDesc(values []string) {
	sort.SliceStable(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
}
