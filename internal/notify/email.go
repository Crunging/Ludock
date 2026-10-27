package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// sendEmail delivers m over SMTP. Server replies and connection errors are
// reduced to a failure code: they can echo the credentials or addresses.
func (s *Service) sendEmail(ctx context.Context, c Config, m message) string {
	timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	from, err := mail.ParseAddress(c.From)
	if err != nil {
		return "invalid_request"
	}
	to, err := mail.ParseAddressList(c.To)
	if err != nil {
		return "invalid_request"
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.TLS != nil {
		config = s.TLS.Clone()
	}
	config.ServerName = c.Host
	address := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	var conn net.Conn
	if c.Security == "tls" {
		conn, err = (&tls.Dialer{Config: config}).DialContext(timeout, "tcp", address)
	} else {
		conn, err = (&net.Dialer{}).DialContext(timeout, "tcp", address)
	}
	if err != nil {
		return smtpFailure(timeout, err)
	}
	defer conn.Close()
	// Ends a stalled conversation when the timeout passes or the settings
	// change.
	defer context.AfterFunc(timeout, func() { conn.Close() })()
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return smtpFailure(timeout, err)
	}
	if c.Security == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return "tls"
		}
		if err := client.StartTLS(config); err != nil {
			return smtpFailure(timeout, err)
		}
	}
	if c.Username != "" {
		_, mechanisms := client.Extension("AUTH")
		var auth smtp.Auth
		switch {
		case hasWord(mechanisms, "PLAIN"):
			auth = smtp.PlainAuth("", c.Username, c.Password, c.Host)
		case hasWord(mechanisms, "LOGIN"):
			auth = loginAuth{c.Username, c.Password}
		default:
			return "unauthorized"
		}
		if err := client.Auth(auth); err != nil {
			return smtpFailure(timeout, err)
		}
	}
	if err := client.Mail(from.Address); err != nil {
		return recipientFailure(timeout, err)
	}
	for _, address := range to {
		if err := client.Rcpt(address.Address); err != nil {
			return recipientFailure(timeout, err)
		}
	}
	writer, err := client.Data()
	if err != nil {
		return smtpFailure(timeout, err)
	}
	if _, err := writer.Write(emailMessage(c, m, time.Now())); err != nil {
		return smtpFailure(timeout, err)
	}
	if err := writer.Close(); err != nil {
		return smtpFailure(timeout, err)
	}
	// The message is accepted once DATA ends; a failed goodbye changes nothing.
	client.Quit()
	return ""
}

func hasWord(list, word string) bool {
	for _, item := range strings.Fields(list) {
		if strings.EqualFold(item, word) {
			return true
		}
	}
	return false
}

// emailMessage formats m as a plain-text message.
func emailMessage(c Config, m message, now time.Time) []byte {
	// Event text is one line, but a stray line break must not start a header.
	subject := strings.Join(strings.FieldsFunc(m.text, func(r rune) bool { return r < 0x20 || r == 0x7f }), " ")
	subject = mime.QEncoding.Encode("utf-8", truncate(title+": "+subject, 120))
	var b bytes.Buffer
	for _, header := range [][2]string{{"From", c.From}, {"To", c.To}, {"Subject", subject},
		{"Date", now.Format(time.RFC1123Z)}, {"Message-ID", "<" + m.id + "@ludock.invalid>"}, {"MIME-Version", "1.0"},
		{"Content-Type", "text/plain; charset=utf-8"}, {"Content-Transfer-Encoding", "quoted-printable"}} {
		b.WriteString(header[0] + ": " + header[1] + "\r\n")
	}
	b.WriteString("\r\n")
	body := quotedprintable.NewWriter(&b)
	body.Write([]byte(m.text + "\r\n\r\nSent by Ludock.\r\n"))
	body.Close()
	return b.Bytes()
}

// loginAuth is the LOGIN mechanism, which some providers offer instead of
// PLAIN. Like PLAIN it refuses to send the password without TLS.
type loginAuth struct{ username, password string }

func (a loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("unencrypted connection")
	}
	return "LOGIN", nil, nil
}

func (a loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(challenge))) {
	case "username:":
		return []byte(a.username), nil
	case "password:":
		return []byte(a.password), nil
	}
	return nil, errors.New("unexpected LOGIN challenge")
}

func smtpFailure(ctx context.Context, err error) string {
	var reply *textproto.Error
	var certificate *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	switch {
	case errors.As(err, &reply):
		switch {
		case reply.Code == 530 || reply.Code == 534 || reply.Code == 535 || reply.Code == 538:
			return "unauthorized"
		case reply.Code >= 400 && reply.Code < 500:
			return "unavailable"
		}
		return "rejected"
	case ctx.Err() != nil:
		return "timeout"
	case errors.As(err, &certificate), errors.As(err, &record), errors.As(err, &authority), errors.As(err, &hostname):
		return "tls"
	}
	var alert tls.AlertError
	if errors.As(err, &alert) {
		return "tls"
	}
	return "network"
}

// recipientFailure reads a permanent refusal of an address as an address
// problem.
func recipientFailure(ctx context.Context, err error) string {
	var reply *textproto.Error
	if errors.As(err, &reply) && reply.Code >= 550 && reply.Code <= 553 {
		return "recipient"
	}
	return smtpFailure(ctx, err)
}
