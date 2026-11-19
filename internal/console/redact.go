package console

import (
	"strings"
	"unicode/utf8"
)

// Redactor removes credential values from streamed output. It holds back
// trailing text that could start a credential split across Docker frames, so
// no fragment of a credential reaches the browser; other text is sent at once.
type Redactor struct {
	secrets []string
	output  func(string)
	pending string
}

// NewRedactor redacts secrets (longest first wins at the same position).
func NewRedactor(secrets []string, output func(string)) *Redactor {
	var kept []string
	for _, secret := range secrets {
		if secret != "" {
			kept = append(kept, secret)
		}
	}
	return &Redactor{secrets: kept, output: output}
}

func (r *Redactor) Push(value string) {
	r.pending += value
	r.emit(false)
}

// End flushes everything still held.
func (r *Redactor) End() { r.emit(true) }

func (r *Redactor) emit(final bool) {
	limit := len(r.pending)
	if !final {
		limit = len(r.pending) - r.partial()
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

// partial returns the length of the longest end of pending that begins a
// credential without completing it.
func (r *Redactor) partial() int {
	longest := 0
	for _, secret := range r.secrets {
		for length := min(len(secret)-1, len(r.pending)); length > longest; length-- {
			if strings.HasSuffix(r.pending, secret[:length]) {
				longest = length
				break
			}
		}
	}
	return longest
}
