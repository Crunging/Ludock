package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
)

const (
	SetupCodeMin = 32
	SetupCodeMax = 128
	PasswordMin  = 15
	PasswordMax  = 128
	// SetupOpenFor is how long after start an unowned Ludock can be set up
	// without the code, unless LUDOCK_SETUP_CODE is set.
	SetupOpenFor = 10 * time.Minute
)

type SetupState struct {
	Required bool
	// CodeNeeded is false while setup is open without the code.
	CodeNeeded bool
	// CodeSet says the owner chose the code with LUDOCK_SETUP_CODE.
	CodeSet bool
}

// Setup authorizes first-administrator setup until the first administrator
// exists: without a code for SetupOpenFor after start, then with a one-time
// code. The code is long and random; attempts are limited per source.
type Setup struct {
	mu            sync.Mutex
	authorization [32]byte
	generated     string
	configured    bool
	// openUntil ends the time without a code, in Unix milliseconds.
	openUntil int64
	completed bool
}

// NewSetup takes the configured code. An empty code generates one that is
// printed to the console once.
func NewSetup(code string) (*Setup, error) {
	setup := &Setup{configured: code != ""}
	if code == "" {
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		code = base64.RawURLEncoding.EncodeToString(random[:])
		setup.generated = code
	}
	if len(code) < SetupCodeMin || len(code) > SetupCodeMax {
		return nil, apperr.Newf("INVALID_SETUP_CODE", 400, "LUDOCK_SETUP_CODE must be between %d and %d characters", SetupCodeMin, SetupCodeMax)
	}
	setup.authorization = sha256.Sum256([]byte(code))
	return setup, nil
}

var (
	errSetupComplete    = apperr.New("SETUP_COMPLETE", 409, "Initial setup has already been completed")
	errSetupAuth        = apperr.New("SETUP_AUTHORIZATION_REQUIRED", 403, "That setup code didn’t work. Use the latest one in Ludock’s log, or the one you set as LUDOCK_SETUP_CODE.")
	errSetupCodeMissing = apperr.New("SETUP_AUTHORIZATION_REQUIRED", 403, "Enter the setup code to create your account.")
)

func (w *Setup) assertOpen(required bool) error {
	if !required {
		return errSetupComplete
	}
	return nil
}

// assertAuthorized accepts no code while setup is open without one; a code
// that is sent must always match.
func (w *Setup) assertAuthorized(required bool, candidate string, now int64) error {
	if err := w.assertOpen(required); err != nil {
		return err
	}
	if candidate == "" {
		if w.codeNeeded(now) {
			return errSetupCodeMissing
		}
		return nil
	}
	hash := sha256.Sum256([]byte(candidate))
	w.mu.Lock()
	matches := !w.completed && subtle.ConstantTimeCompare(hash[:], w.authorization[:]) == 1
	w.mu.Unlock()
	if !matches || len(candidate) < SetupCodeMin || len(candidate) > SetupCodeMax {
		return errSetupAuth
	}
	return nil
}

// open starts the time setup needs no code, unless the owner set one.
func (w *Setup) open(now int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.configured && !w.completed {
		w.openUntil = now + SetupOpenFor.Milliseconds()
	}
}

func (w *Setup) codeNeeded(now int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.completed || now >= w.openUntil
}

// TakeGeneratedCode returns a generated code once, for the console.
func (w *Setup) TakeGeneratedCode() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	code := w.generated
	w.generated = ""
	return code
}

func (w *Setup) markCompleted() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.generated = ""
	w.completed = true
	w.openUntil = 0
	clear(w.authorization[:])
}
