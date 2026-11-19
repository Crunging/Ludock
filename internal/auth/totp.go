package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
)

// RFC 6238 with the parameters authenticator apps assume.
const (
	totpPeriodMs   = 30_000
	totpDigits     = 6
	totpSecretSize = 20
	// totpDrift is how many steps either side of now a code may come from.
	totpDrift = 1
)

var secretEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// hotp is RFC 4226's HOTP value for counter.
func hotp(secret []byte, counter int64) string {
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], uint64(counter))
	mac := hmac.New(sha1.New, secret)
	mac.Write(message[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1_000_000)
}

func totpStep(nowMs int64) int64 { return nowMs / totpPeriodMs }

// matchTOTP returns the latest step within the drift window whose code is
// code and which is later than lastStep, or 0.
func matchTOTP(secret []byte, code string, nowMs, lastStep int64) int64 {
	if !isTOTPCode(code) {
		return 0
	}
	current := totpStep(nowMs)
	for step := current + totpDrift; step >= current-totpDrift; step-- {
		if step <= lastStep || step <= 0 {
			break
		}
		if subtle.ConstantTimeCompare([]byte(hotp(secret, step)), []byte(code)) == 1 {
			return step
		}
	}
	return 0
}

func isTOTPCode(code string) bool {
	if len(code) != totpDigits {
		return false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

const (
	recoveryCodeCount  = 10
	recoveryCodeLength = 10
	recoveryAlphabet   = "abcdefghijklmnopqrstuvwxyz234567"
)

// newRecoveryCodes returns codes with 50 random bits each, formatted for
// display as "abcde-fghij".
func newRecoveryCodes() []string {
	codes := make([]string, recoveryCodeCount)
	for index := range codes {
		var raw [recoveryCodeLength]byte
		if _, err := rand.Read(raw[:]); err != nil {
			panic(err)
		}
		var b strings.Builder
		for position, value := range raw {
			if position == recoveryCodeLength/2 {
				b.WriteByte('-')
			}
			b.WriteByte(recoveryAlphabet[value&31])
		}
		codes[index] = b.String()
	}
	return codes
}

// normalizeCode strips the separators people type or paste and lowercases
// recovery codes.
func normalizeCode(code string) string {
	code = strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '\t' {
			return -1
		}
		return r
	}, code)
	return strings.ToLower(code)
}

func isRecoveryCode(code string) bool {
	if len(code) != recoveryCodeLength {
		return false
	}
	for _, c := range code {
		if !strings.ContainsRune(recoveryAlphabet, c) {
			return false
		}
	}
	return true
}

// Enrollment is a secret being set up in an authenticator app.
type Enrollment struct {
	// Secret is the base32 text people can type instead of scanning.
	Secret string
	// URI is the otpauth:// link the QR code encodes.
	URI string
}

func newEnrollment(secret []byte, username string) Enrollment {
	encoded := secretEncoding.EncodeToString(secret)
	query := url.Values{"secret": {encoded}, "issuer": {"Ludock"}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	label := url.PathEscape("Ludock:" + username)
	return Enrollment{Secret: encoded, URI: "otpauth://totp/" + label + "?" + query.Encode()}
}
