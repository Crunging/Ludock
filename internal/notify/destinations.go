package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
)

type Destination string

const (
	Discord  Destination = "discord"
	Ntfy     Destination = "ntfy"
	Gotify   Destination = "gotify"
	Telegram Destination = "telegram"
	Email    Destination = "email"
	Webhook  Destination = "webhook"
)

// Destinations lists every destination in display order.
var Destinations = []Destination{Discord, Ntfy, Gotify, Telegram, Email, Webhook}

func Parse(name string) (Destination, bool) {
	for _, d := range Destinations {
		if string(d) == name {
			return d, true
		}
	}
	return "", false
}

func (d Destination) Label() string {
	return map[Destination]string{Discord: "Discord", Ntfy: "ntfy", Gotify: "Gotify", Telegram: "Telegram",
		Email: "Email", Webhook: "Webhook"}[d]
}

// name is the destination as named mid-sentence in failure messages.
func (d Destination) name() string {
	switch d {
	case Email:
		return "the mail server"
	case Webhook:
		return "the webhook"
	}
	return d.Label()
}

// Config is one destination's settings. Each destination uses only some
// fields; secret ones are write-only.
type Config struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url,omitempty"`
	Token   string `json:"token,omitempty"`
	Topic   string `json:"topic,omitempty"`
	ChatID  string `json:"chatId,omitempty"`
	// Email
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Security string `json:"security,omitempty"` // starttls, tls, or none
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
}

// secrets are the fields of c that never leave Ludock, by form name.
func (d Destination) secrets(c *Config) map[string]*string {
	switch d {
	case Discord:
		return map[string]*string{"url": &c.URL}
	case Ntfy:
		return map[string]*string{"topic": &c.Topic, "token": &c.Token}
	case Gotify, Telegram:
		return map[string]*string{"token": &c.Token}
	case Email:
		return map[string]*string{"password": &c.Password}
	case Webhook:
		return map[string]*string{"url": &c.URL, "token": &c.Token}
	}
	return nil
}

// configured reports whether c has everything d needs to send.
func (d Destination) configured(c Config) bool {
	switch d {
	case Discord, Webhook:
		return c.URL != ""
	case Ntfy:
		return c.URL != "" && c.Topic != ""
	case Gotify:
		return c.URL != "" && c.Token != ""
	case Telegram:
		return c.Token != "" && c.ChatID != ""
	case Email:
		return c.Host != "" && c.From != "" && c.To != ""
	}
	return false
}

func (d Destination) ready(c Config) bool { return c.Enabled && d.configured(c) }

const DefaultNtfyServer = "https://ntfy.sh"

var (
	discordPath   = regexp.MustCompile(`^/api/webhooks/\d+/[A-Za-z0-9_-]+$`)
	ntfyTopic     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	telegramToken = regexp.MustCompile(`^\d{1,20}:[A-Za-z0-9_-]{20,80}$`)
	telegramChat  = regexp.MustCompile(`^(-?\d{1,20}|@[A-Za-z][A-Za-z0-9_]{4,31})$`)
	hostname      = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}[A-Za-z0-9])?)*\.?$`)
)

func invalid(message string) error { return apperr.New("INVALID_NOTIFICATION_SETTINGS", 400, message) }

// printable rejects control characters, which could split a header or an
// SMTP command.
func printable(value string) bool {
	return !strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// serverURL checks a self-hosted service address: any HTTP or HTTPS host,
// since such services usually run on the local network.
func serverURL(value, what string, query bool) (string, error) {
	parsed, err := url.Parse(value)
	if len(value) > 1024 || err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", invalid("Enter a valid HTTP or HTTPS " + what + " URL")
	}
	if parsed.User != nil || parsed.Fragment != "" || strings.Contains(value, "#") || (!query && (parsed.RawQuery != "" || parsed.ForceQuery)) {
		return "", invalid("Enter the " + what + " URL without credentials, a query, or a fragment")
	}
	if !query {
		value = strings.TrimRight(value, "/")
	}
	return value, nil
}

// normalize validates c for d and returns it with only d's fields. A
// destination that is off may be saved partly filled in.
func (d Destination) normalize(c Config) (Config, error) {
	out := Config{Enabled: c.Enabled}
	var err error
	switch d {
	case Discord:
		if c.URL != "" {
			parsed, err := url.Parse(c.URL)
			if len(c.URL) > 1024 || err != nil || parsed.Host == "" {
				return out, invalid("Enter a valid Discord webhook URL")
			}
			if parsed.Scheme != "https" || parsed.Hostname() != "discord.com" || parsed.Port() != "" || parsed.User != nil ||
				parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(c.URL, "#") ||
				!discordPath.MatchString(parsed.EscapedPath()) {
				return out, invalid("Use a Discord HTTPS webhook URL")
			}
		}
		out.URL = c.URL
	case Webhook:
		if c.URL != "" {
			// Many receivers carry their token in the query string.
			if out.URL, err = serverURL(c.URL, "webhook", true); err != nil {
				return out, err
			}
		}
		if len(c.Token) > 1024 || !printable(c.Token) {
			return out, invalid("Enter an Authorization header value on one line")
		}
		out.Token = c.Token
	case Ntfy:
		out.URL = DefaultNtfyServer
		if c.URL != "" {
			if out.URL, err = serverURL(c.URL, "ntfy server", false); err != nil {
				return out, err
			}
		}
		if c.Topic != "" && !ntfyTopic.MatchString(c.Topic) {
			return out, invalid("Use a topic of up to 64 letters, digits, dashes, and underscores")
		}
		if len(c.Token) > 256 || !printable(c.Token) || strings.Contains(c.Token, " ") {
			return out, invalid("Enter a valid ntfy access token")
		}
		out.Topic, out.Token = c.Topic, c.Token
	case Gotify:
		if c.URL != "" {
			if out.URL, err = serverURL(c.URL, "Gotify server", false); err != nil {
				return out, err
			}
		}
		if len(c.Token) > 256 || !printable(c.Token) || strings.Contains(c.Token, " ") {
			return out, invalid("Enter a valid Gotify application token")
		}
		out.Token = c.Token
	case Telegram:
		if c.Token != "" && !telegramToken.MatchString(c.Token) {
			return out, invalid("Enter the bot token from BotFather, such as 123456:ABC-DEF…")
		}
		if c.ChatID != "" && !telegramChat.MatchString(c.ChatID) {
			return out, invalid("Enter a numeric chat ID or a public @channel name")
		}
		out.Token, out.ChatID = c.Token, c.ChatID
	case Email:
		if out, err = normalizeEmail(c); err != nil {
			return out, err
		}
	default:
		return out, apperr.New("NOTIFICATION_DESTINATION_NOT_FOUND", 404, "Notification destination not found")
	}
	if c.Enabled && !d.configured(out) {
		return out, apperr.New("NOTIFICATION_SETTINGS_REQUIRED", 400, "Fill in the required "+d.Label()+" settings before turning it on")
	}
	return out, nil
}

func normalizeEmail(c Config) (Config, error) {
	out := Config{Enabled: c.Enabled, Security: c.Security, Port: c.Port}
	switch out.Security {
	case "":
		out.Security = "starttls"
	case "starttls", "tls", "none":
	default:
		return out, invalid("Choose how to secure the mail server connection")
	}
	if out.Port == 0 {
		out.Port = map[string]int{"starttls": 587, "tls": 465, "none": 25}[out.Security]
	}
	if out.Port < 1 || out.Port > 65535 {
		return out, invalid("Enter a mail server port from 1 to 65535")
	}
	host := strings.Trim(c.Host, "[]")
	if c.Host != "" && (len(host) > 253 || (net.ParseIP(host) == nil && !hostname.MatchString(host))) {
		return out, invalid("Enter the mail server’s host name or IP address")
	}
	out.Host = host
	if len(c.Username) > 256 || len(c.Password) > 256 || !printable(c.Username) || strings.ContainsRune(c.Password, 0) {
		return out, invalid("Enter a username and password of up to 256 characters")
	}
	if c.Username != "" {
		if out.Security == "none" {
			return out, invalid("Use STARTTLS or TLS to sign in to the mail server; passwords are never sent unencrypted")
		}
		if c.Password == "" {
			return out, invalid("Enter the password for the mail server username")
		}
		// The password is kept only alongside its username.
		out.Username, out.Password = c.Username, c.Password
	}
	if c.From != "" {
		from, err := mail.ParseAddress(c.From)
		if err != nil || len(c.From) > 320 || !printable(c.From) {
			return out, invalid("Enter one sender address, such as ludock@example.com")
		}
		out.From = from.String()
	}
	if c.To != "" {
		to, err := mail.ParseAddressList(c.To)
		if err != nil || len(to) > 10 || len(c.To) > 2000 || !printable(c.To) {
			return out, invalid("Enter up to 10 recipient addresses, separated by commas")
		}
		addresses := make([]string, len(to))
		for i, address := range to {
			addresses[i] = address.String()
		}
		out.To = strings.Join(addresses, ", ")
	}
	return out, nil
}

type message struct {
	id, kind, text string
	createdAt      int64
}

const title = "Ludock"

func (d Destination) request(ctx context.Context, c Config, m message) (*http.Request, error) {
	var target string
	var body any
	header := http.Header{"Content-Type": {"application/json"}}
	switch d {
	case Discord:
		// Discord confirms it saved the message only with wait=true. Saved URLs
		// reject query strings, so this option is controlled here.
		target = c.URL + "?wait=true"
		body = map[string]any{"content": m.text, "allowed_mentions": map[string]any{"parse": []string{}}}
	case Ntfy:
		target = c.URL
		body = map[string]any{"topic": c.Topic, "title": title, "message": m.text}
		if c.Token != "" {
			header.Set("Authorization", "Bearer "+c.Token)
		}
	case Gotify:
		target = c.URL + "/message"
		body = map[string]any{"title": title, "message": m.text, "priority": 5}
		header.Set("X-Gotify-Key", c.Token)
	case Telegram:
		target = telegramAPI + "/bot" + c.Token + "/sendMessage"
		body = map[string]any{"chat_id": c.ChatID, "text": m.text, "link_preview_options": map[string]any{"is_disabled": true}}
	case Webhook:
		target = c.URL
		body = map[string]any{"source": "ludock", "id": m.id, "kind": m.kind, "message": m.text,
			"createdAt": time.UnixMilli(m.createdAt).UTC().Format(time.RFC3339)}
		if c.Token != "" {
			header.Set("Authorization", c.Token)
		}
	}
	payload, _ := json.Marshal(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header = header
	return request, nil
}

const telegramAPI = "https://api.telegram.org"
