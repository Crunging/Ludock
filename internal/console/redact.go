package console

import (
	"strings"
	"unicode/utf8"
)

// Redactor removes credential values from streamed output. It keeps enough
// trailing text to catch a credential split across Docker frames, so no
// fragment of a credential reaches the browser.
type Redactor struct {
	secrets []string
	output  func(string)
	pending string
	tail    int
}

// NewRedactor redacts secrets (longest first wins at the same position).
func NewRedactor(secrets []string, output func(string)) *Redactor {
	tail := 0
	var kept []string
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		kept = append(kept, secret)
		tail = max(tail, len(secret)-1)
	}
	return &Redactor{secrets: kept, output: output, tail: tail}
}

// Push adds output.
func (r *Redactor) Push(value string) {
	r.pending += value
	r.emit(false)
}

// End flushes everything still held.
func (r *Redactor) End() { r.emit(true) }

func (r *Redactor) emit(final bool) {
	limit := len(r.pending)
	if !final {
		limit = max(0, len(r.pending)-r.tail)
		// Never split a rune: a partial one would not survive encoding.
		for limit > 0 && limit < len(r.pending) && !utf8.RuneStart(r.pending[limit]) {
			limit--
		}
	}
	if limit == 0 {
		return
	}
	positions := make([]int, len(r.secrets))
	for index, secret := range r.secrets {
		positions[index] = strings.Index(r.pending, secret)
	}
	var safe strings.Builder
	offset := 0
	for offset < limit {
		next, matched := limit, ""
		for index, secret := range r.secrets {
			found := positions[index]
			if found != -1 && found < offset {
				if relative := strings.Index(r.pending[offset:], secret); relative >= 0 {
					found = offset + relative
				} else {
					found = -1
				}
				positions[index] = found
			}
			if found != -1 && found < next {
				next, matched = found, secret
				if next == offset {
					break
				}
			}
		}
		safe.WriteString(r.pending[offset:next])
		offset = next
		if matched == "" {
			break
		}
		safe.WriteString("[redacted]")
		offset += len(matched)
	}
	r.pending = r.pending[offset:]
	if safe.Len() > 0 {
		r.output(safe.String())
	}
}
