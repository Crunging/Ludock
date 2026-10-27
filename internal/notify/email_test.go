package notify

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// smtpServer is a mail server that accepts one conversation at a time.
type smtpServer struct {
	port     int
	starttls bool
	// Recorded from the last conversation.
	auth     string
	authTLS  bool
	rcpts    []string
	message  string
	finished chan struct{}
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func startSMTP(t *testing.T, starttls bool, password string) (*smtpServer, *x509.CertPool) {
	t.Helper()
	certificate, pool := selfSigned(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	server := &smtpServer{port: listener.Addr().(*net.TCPAddr).Port, starttls: starttls, finished: make(chan struct{}, 8)}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			server.serve(conn, certificate, password)
			server.finished <- struct{}{}
		}
	}()
	return server, pool
}

func (s *smtpServer) serve(conn net.Conn, certificate tls.Certificate, password string) {
	defer conn.Close()
	text := textproto.NewConn(conn)
	secure := false
	text.PrintfLine("220 fake ESMTP")
	for {
		line, err := text.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			if s.starttls && !secure {
				text.PrintfLine("250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN LOGIN")
			} else {
				text.PrintfLine("250-fake\r\n250 AUTH PLAIN LOGIN")
			}
		case "STARTTLS":
			text.PrintfLine("220 ready")
			tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}})
			if tlsConn.Handshake() != nil {
				return
			}
			conn, secure = tlsConn, true
			text = textproto.NewConn(conn)
		case "AUTH":
			encoded := strings.TrimPrefix(arg, "PLAIN ")
			decoded, _ := base64.StdEncoding.DecodeString(encoded)
			s.auth, s.authTLS = string(decoded), secure
			if string(decoded) != "\x00ludock\x00"+password {
				text.PrintfLine("535 5.7.8 bad credentials for %s", decoded)
				continue
			}
			text.PrintfLine("235 ok")
		case "MAIL":
			s.rcpts = nil
			text.PrintfLine("250 ok")
		case "RCPT":
			if strings.Contains(arg, "nobody@") {
				text.PrintfLine("550 no such user")
				continue
			}
			s.rcpts = append(s.rcpts, arg)
			text.PrintfLine("250 ok")
		case "DATA":
			text.PrintfLine("354 go ahead")
			body, _ := io.ReadAll(text.DotReader())
			s.message = string(body)
			text.PrintfLine("250 queued")
		case "QUIT":
			text.PrintfLine("221 bye")
			return
		default:
			text.PrintfLine("502 unknown")
		}
	}
}

// Email is sent over STARTTLS with the saved credentials, and refused
// credentials or addresses are reported without the server's reply.
func TestEmailDelivery(t *testing.T) {
	server, pool := startSMTP(t, true, "app-password")
	s := &Service{DB: adminDB(t), TLS: &tls.Config{RootCAs: pool}}
	config := Config{Enabled: true, Host: "127.0.0.1", Port: server.port, Security: "starttls", Username: "ludock",
		Password: "app-password", From: "Ludock <ludock@example.com>", To: "a@example.com, b@example.com"}
	if err := s.Configure(ctx, "admin", adminSession, Email, config, false); err != nil {
		t.Fatal(err)
	}
	s.Notify("outage", "Survival is unavailable.")
	if err := s.Deliver(ctx); err != nil {
		t.Fatal(err)
	}
	<-server.finished
	deliveries, _ := s.Deliveries(ctx)
	if deliveries[0].State != "delivered" {
		t.Fatalf("%+v", deliveries[0])
	}
	if !server.authTLS || server.auth != "\x00ludock\x00app-password" || len(server.rcpts) != 2 {
		t.Fatalf("auth over TLS=%v %q rcpts=%v", server.authTLS, server.auth, server.rcpts)
	}
	// The server's reader turns each CRLF into a newline.
	for _, want := range []string{"From: \"Ludock\" <ludock@example.com>\n", "To: <a@example.com>, <b@example.com>\n",
		"Subject: Ludock: Survival is unavailable.\n", "\n\nSurvival is unavailable.\n"} {
		if !strings.Contains(server.message, want) {
			t.Errorf("message lacks %q:\n%s", want, server.message)
		}
	}

	// A refused password and a refused recipient each get their own message.
	config.Password = "wrong"
	if err := s.Configure(ctx, "admin", adminSession, Email, config, false); err != nil {
		t.Fatal(err)
	}
	s.Notify("refused", "Creative is unavailable.")
	s.Deliver(ctx)
	<-server.finished
	config.Password, config.To = "app-password", "a@example.com, nobody@example.com"
	if err := s.Configure(ctx, "admin", adminSession, Email, config, false); err != nil {
		t.Fatal(err)
	}
	s.Notify("recipient", "Lobby is unavailable.")
	s.Deliver(ctx)
	<-server.finished
	deliveries, _ = s.Deliveries(ctx)
	// Both may be queued in the same millisecond, so their order varies.
	failures := map[string]bool{deliveries[0].LastFailure: true, deliveries[1].LastFailure: true}
	if !failures["Delivery to the mail server was refused. Replace the saved credentials."] ||
		!failures["Delivery to the mail server failed because it refused the sender or a recipient. Check the addresses."] {
		t.Fatalf("%+v", deliveries[:2])
	}
	var stored string
	s.DB.QueryRow("SELECT group_concat(COALESCE(failure_code,''), ',') FROM notification_deliveries").Scan(&stored)
	if strings.Contains(stored, "wrong") || strings.Contains(stored, "credentials") {
		t.Fatal("stored the server's reply:", stored)
	}
}

// A server that does not offer STARTTLS never receives the password.
func TestEmailRequiresStartTLS(t *testing.T) {
	server, _ := startSMTP(t, false, "app-password")
	s := &Service{DB: adminDB(t)}
	if err := s.Configure(ctx, "admin", adminSession, Email, Config{Enabled: true, Host: "127.0.0.1", Port: server.port,
		Username: "ludock", Password: "app-password", From: "ludock@example.com", To: "a@example.com"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueTest(ctx, "admin", adminSession, Email); err != nil {
		t.Fatal(err)
	}
	s.Deliver(ctx)
	<-server.finished
	deliveries, _ := s.Deliveries(ctx)
	if server.auth != "" || !strings.HasPrefix(deliveries[0].LastFailure, "Could not open a secure connection to the mail server.") {
		t.Fatalf("auth=%q %+v", server.auth, deliveries[0])
	}
}

// Line breaks in event text cannot add headers, and non-ASCII subjects are
// encoded.
func TestEmailMessageHeaders(t *testing.T) {
	message := string(emailMessage(Config{From: "<a@example.com>", To: "<b@example.com>"},
		message{id: "id-1", text: "Héllo\r\nBcc: c@example.com"}, time.Unix(0, 0)))
	headers, _, _ := strings.Cut(message, "\r\n\r\n")
	if strings.Contains(headers, "\r\nBcc:") || !strings.Contains(headers, "Subject: =?utf-8?q?Ludock:_H=C3=A9llo_Bcc:_c@example.com?=") ||
		!strings.Contains(headers, "Message-ID: <id-1@ludock.invalid>") {
		t.Fatal(headers)
	}
}
