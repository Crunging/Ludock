// Package auth implements accounts, sessions, setup, and request security.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Current Argon2id parameters. Tests lower the memory cost.
var (
	argonMemoryKiB uint32 = 65536
	argonTime      uint32 = 2
)

const (
	argonThreads   = 1
	argonSaltBytes = 32
	argonKeyBytes  = 32
)

// Hasher hashes and verifies passwords. Tests wrap it to pause work at exact
// boundaries.
type Hasher interface {
	Hash(password string) (string, error)
	Verify(password, encoded string) bool
}

// Argon2 is the production hasher. It writes argon2id PHC strings.
type Argon2 struct{}

var b64 = base64.RawStdEncoding.Strict()

func (Argon2) Hash(password string) (string, error) {
	return hashWith(password, argonMemoryKiB, argonTime, argonThreads)
}

func hashWith(password string, memory, time uint32, threads uint8) (string, error) {
	salt := make([]byte, argonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, time, memory, threads, argonKeyBytes)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, time, threads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

type argonParams struct {
	memory, time uint32
	threads      uint8
	salt, key    []byte
}

var phcPattern = regexp.MustCompile(`^\$argon2id\$v=19\$m=([1-9]\d*),t=([1-9]\d*),p=([1-9]\d*)\$([A-Za-z0-9+/]+)\$([A-Za-z0-9+/]+)$`)

func parseArgon(encoded string) (*argonParams, bool) {
	if len(encoded) > 256 {
		return nil, false
	}
	match := phcPattern.FindStringSubmatch(encoded)
	if match == nil {
		return nil, false
	}
	memory, err1 := strconv.ParseUint(match[1], 10, 32)
	time, err2 := strconv.ParseUint(match[2], 10, 32)
	threads, err3 := strconv.ParseUint(match[3], 10, 8)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, false
	}
	// Stored parameters must not turn a login into unbounded password work.
	if memory < 8*threads || memory > 262144 || time > 10 || threads > 16 {
		return nil, false
	}
	salt, err := b64.DecodeString(match[4])
	if err != nil || len(salt) < 16 || len(salt) > 64 || b64.EncodeToString(salt) != match[4] {
		return nil, false
	}
	key, err := b64.DecodeString(match[5])
	if err != nil || len(key) != argonKeyBytes || b64.EncodeToString(key) != match[5] {
		return nil, false
	}
	return &argonParams{uint32(memory), uint32(time), uint8(threads), salt, key}, true
}

// Verify checks a password against a PHC string in constant time.
func (Argon2) Verify(password, encoded string) bool {
	params, ok := parseArgon(encoded)
	if !ok {
		return false
	}
	key := argon2.IDKey([]byte(password), params.salt, params.time, params.memory, params.threads, argonKeyBytes)
	return subtle.ConstantTimeCompare(key, params.key) == 1
}

// NeedsUpgrade reports a verifier weaker than the current parameters.
func NeedsUpgrade(encoded string) bool {
	params, ok := parseArgon(encoded)
	return !ok || params.memory < argonMemoryKiB || params.time < argonTime
}

// ErrPasswordWorkBusy rejects password work instead of queueing it.
var ErrPasswordWorkBusy = errors.New("password verification is busy")

const (
	maxPasswordWork           = 4
	maxPasswordWorkPerSource  = 2
	maxPasswordWorkPerSubject = 1
)

// PasswordWork bounds concurrent memory-hard KDF work. It rejects instead of
// queueing so an attacker cannot build a backlog. Its limits are deliberately
// independent of the persistent login throttles.
type PasswordWork struct {
	mu        sync.Mutex
	active    int
	bySource  map[string]int
	bySubject map[string]int
}

func NewPasswordWork() *PasswordWork {
	return &PasswordWork{bySource: map[string]int{}, bySubject: map[string]int{}}
}

// Do reserves a slot for source and subject and runs work, or returns
// ErrPasswordWorkBusy without running it.
func (p *PasswordWork) Do(source, subject string, work func() error) error {
	p.mu.Lock()
	if p.active >= maxPasswordWork || p.bySource[source] >= maxPasswordWorkPerSource ||
		p.bySubject[subject] >= maxPasswordWorkPerSubject {
		p.mu.Unlock()
		return ErrPasswordWorkBusy
	}
	p.active++
	p.bySource[source]++
	p.bySubject[subject]++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.active--
		release(p.bySource, source)
		release(p.bySubject, subject)
		p.mu.Unlock()
	}()
	return work()
}

func release(counter map[string]int, key string) {
	if counter[key] <= 1 {
		delete(counter, key)
	} else {
		counter[key]--
	}
}
