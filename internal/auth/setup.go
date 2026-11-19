package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"sync"

	"github.com/Crunging/Ludock/internal/apperr"
)

const (
	SetupCodeMin = 32
	SetupCodeMax = 128
	PasswordMin  = 15
	PasswordMax  = 128
)

type SetupState struct {
	Required bool
}

// Setup authorizes first-administrator setup with a one-time code until the
// first administrator exists. The code is long and random; attempts are
// limited per source.
type Setup struct {
	mu            sync.Mutex
	authorization [32]byte
	generated     string
	completed     bool
}

// NewSetup takes the configured code. An empty code generates one that is
// printed to the console once.
func NewSetup(code string) (*Setup, error) {
	setup := &Setup{}
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
	errSetupComplete = apperr.New("SETUP_COMPLETE", 409, "Initial setup has already been completed")
	errSetupAuth     = apperr.New("SETUP_AUTHORIZATION_REQUIRED", 403, "Initial setup authorization failed")
)

func (w *Setup) assertOpen(required bool) error {
	if !required {
		return errSetupComplete
	}
	return nil
}

func (w *Setup) assertAuthorized(required bool, candidate string) error {
	if err := w.assertOpen(required); err != nil {
		return err
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
	clear(w.authorization[:])
}
