package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"sync"

	"github.com/Crunging/Ludock/internal/apperr"
)

const (
	// SetupWindowMs is how long first-administrator setup stays open after
	// startup.
	SetupWindowMs = 5 * 60 * 1000
	SetupCodeMin  = 32
	SetupCodeMax  = 128
	PasswordMin   = 15
	PasswordMax   = 128
)

// SetupState describes first-administrator setup.
type SetupState struct {
	Required    bool
	Locked      bool
	ExpiresAt   int64
	RemainingMs int64
}

// SetupWindow authorizes first-administrator setup with a one-time code
// for a limited time after startup.
type SetupWindow struct {
	ExpiresAt int64
	now       func() int64

	mu            sync.Mutex
	authorization [32]byte
	generated     string
	completed     bool
}

// NewSetupWindow opens a window of durationMs. An empty code generates one
// that is printed to the console once.
func NewSetupWindow(now func() int64, durationMs int64, code string) (*SetupWindow, error) {
	window := &SetupWindow{ExpiresAt: now() + durationMs, now: now}
	if code == "" {
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		code = base64.RawURLEncoding.EncodeToString(random[:])
		window.generated = code
	}
	if len(code) < SetupCodeMin || len(code) > SetupCodeMax {
		return nil, fmt.Errorf("LUDOCK_SETUP_CODE must be between %d and %d characters", SetupCodeMin, SetupCodeMax)
	}
	window.authorization = sha256.Sum256([]byte(code))
	return window, nil
}

// State reports setup state; required means no account exists yet.
func (w *SetupWindow) State(required bool) SetupState {
	if !required {
		return SetupState{}
	}
	now := w.now()
	return SetupState{Required: true, Locked: now >= w.ExpiresAt, ExpiresAt: w.ExpiresAt,
		RemainingMs: max(0, w.ExpiresAt-now)}
}

var (
	errSetupComplete = apperr.New("SETUP_COMPLETE", 409, "Initial setup has already been completed")
	errSetupLocked   = apperr.New("SETUP_LOCKED", 403, "Initial setup has expired. Restart the panel to reopen setup.")
	errSetupAuth     = apperr.New("SETUP_AUTHORIZATION_REQUIRED", 403, "Initial setup authorization failed")
)

func (w *SetupWindow) assertOpen(required bool) error {
	state := w.State(required)
	if !state.Required {
		return errSetupComplete
	}
	if state.Locked {
		return errSetupLocked
	}
	return nil
}

func (w *SetupWindow) assertAuthorized(required bool, candidate string) error {
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
func (w *SetupWindow) TakeGeneratedCode() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	code := w.generated
	w.generated = ""
	return code
}

func (w *SetupWindow) markCompleted() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.generated = ""
	w.completed = true
	clear(w.authorization[:])
}
