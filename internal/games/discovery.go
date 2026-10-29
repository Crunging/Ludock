package games

import (
	"regexp"

	"github.com/Crunging/Ludock/internal/textutil"
	"strconv"
	"strings"
)

const (
	LabelEnable        = "ludock.enable"
	LabelName          = "ludock.name"
	LabelGame          = "ludock.game"
	LabelAddress       = "ludock.address"
	LabelFiles         = "ludock.files"
	labelComposeOneoff = "com.docker.compose.oneoff"
)

const (
	ReasonInvalidEnableLabel = "invalid-enable-label"
	ReasonOptedOut           = "opted-out"
	ReasonExplicitlyEnabled  = "explicitly-enabled"
	ReasonComposeOneoff      = "compose-oneoff"
	ReasonRecognizedImage    = "recognized-image"
	ReasonUnrecognizedImage  = "unrecognized-image"
)

// Eligibility decides whether Ludock manages a container.
type Eligibility struct {
	Eligible           bool
	Reason             string
	RecognizedGameType string
}

// ParseBool reads a true/false label case-insensitively; ok is false for
// any other value.
func ParseBool(value string, present bool) (result, ok bool) {
	if !present {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// Evaluate applies the same precedence to discovery, inspection, and every
// mutation.
func Evaluate(image string, labels map[string]string) Eligibility {
	recognized := InferGameType(image)
	if value, present := labels[LabelEnable]; present {
		enable, ok := ParseBool(value, true)
		switch {
		case !ok:
			return Eligibility{false, ReasonInvalidEnableLabel, recognized}
		case !enable:
			return Eligibility{false, ReasonOptedOut, recognized}
		}
		return Eligibility{true, ReasonExplicitlyEnabled, recognized}
	}
	if oneoff, ok := ParseBool(labels[labelComposeOneoff], hasKey(labels, labelComposeOneoff)); ok && oneoff {
		return Eligibility{false, ReasonComposeOneoff, recognized}
	}
	if recognized == "unknown" {
		return Eligibility{false, ReasonUnrecognizedImage, recognized}
	}
	return Eligibility{true, ReasonRecognizedImage, recognized}
}

func hasKey(labels map[string]string, key string) bool {
	_, ok := labels[key]
	return ok
}

var (
	hostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[-0-9a-zA-Z]{0,61}[0-9a-zA-Z])?)*\.?$`)
	ipv4Pattern     = regexp.MustCompile(`^(?:(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])$`)
	ipv6Pattern     = regexp.MustCompile(`^(([0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,7}:|([0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,5}(:[0-9a-fA-F]{1,4}){1,2}|([0-9a-fA-F]{1,4}:){1,4}(:[0-9a-fA-F]{1,4}){1,3}|([0-9a-fA-F]{1,4}:){1,3}(:[0-9a-fA-F]{1,4}){1,4}|([0-9a-fA-F]{1,4}:){1,2}(:[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}:((:[0-9a-fA-F]{1,4}){1,6})|:((:[0-9a-fA-F]{1,4}){1,7}|:))$`)
)

// ConnectionHost validates the public name or IP address players use and
// returns it trimmed and lowercased.
func ConnectionHost(value string) (string, bool) {
	host := strings.ToLower(strings.TrimSpace(value))
	if len(host) > 253 || host == "" {
		return "", false
	}
	if ipv4Pattern.MatchString(host) || ipv6Pattern.MatchString(host) {
		return host, true
	}
	if length := len(strings.TrimSuffix(host, ".")); length >= 1 && length <= 253 && hostnamePattern.MatchString(host) {
		return host, true
	}
	return "", false
}

// Address is the exact address players type.
type Address struct {
	Host string
	Port int // 0 when absent
}

var (
	bracketedAddress = regexp.MustCompile(`^\[([^\]]+)\](?::([0-9]+))?$`)
	hostAndPort      = regexp.MustCompile(`^([^:\[\]]+):([0-9]+)$`)
)

// ParseAddress reads ludock.address: a host name or IP address with an
// optional port, such as mc.example.com, 203.0.113.10:30000, or
// [2001:db8::1]:2456. It returns nil for a missing or invalid value, which
// falls back to detection.
func ParseAddress(value string) *Address {
	text := strings.TrimSpace(value)
	if text == "" || textutil.Length(text) > 300 {
		return nil
	}
	host, port := text, ""
	bracketed := false
	if match := bracketedAddress.FindStringSubmatch(text); match != nil {
		host, port, bracketed = match[1], match[2], true
	} else if match := hostAndPort.FindStringSubmatch(text); match != nil {
		host, port = match[1], match[2]
	}
	// Brackets only ever wrap IPv6 addresses.
	if bracketed && !strings.Contains(host, ":") {
		return nil
	}
	parsed, ok := ConnectionHost(host)
	if !ok {
		return nil
	}
	if port == "" {
		return &Address{Host: parsed}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return nil
	}
	return &Address{Host: parsed, Port: number}
}

// Approved configuration labels. Arbitrary ludock.* labels often store
// third-party integration secrets and are never passed on.
var configurationLabels = map[string]bool{
	LabelEnable: true, LabelName: true, LabelGame: true, LabelAddress: true, LabelFiles: true,
	LabelConsole: true, LabelConsolePort: true, LabelConsoleHost: true, LabelConsolePasswordEnv: true, LabelSecretEnv: true,
}

// ApprovedLabels keeps only the configuration labels Ludock reads.
func ApprovedLabels(labels map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range labels {
		if configurationLabels[key] {
			result[key] = value
		}
	}
	return result
}

// ComposeLabels is the Compose identity from labels, or nil for a
// standalone container or an explicit one-off.
func ComposeLabels(labels map[string]string) (project, service, number string, ok bool) {
	if oneoff, valid := ParseBool(labels[labelComposeOneoff], hasKey(labels, labelComposeOneoff)); valid && oneoff {
		return "", "", "", false
	}
	project, hasProject := labels["com.docker.compose.project"]
	service, hasService := labels["com.docker.compose.service"]
	number, hasNumber := labels["com.docker.compose.container-number"]
	if !hasProject && !hasService && !hasNumber {
		return "", "", "", false
	}
	return project, service, number, true
}

var replica = regexp.MustCompile(`^[1-9][0-9]*$`)

// InvalidComposeIdentity reports partial or control-character Compose
// identity labels, which must fail closed.
func InvalidComposeIdentity(labels map[string]string) bool {
	project, service, number, ok := ComposeLabels(labels)
	if !ok {
		return false
	}
	for _, value := range []string{project, service} {
		if value == "" || textutil.Length(value) > 512 || strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return true
		}
	}
	return !replica.MatchString(number)
}
