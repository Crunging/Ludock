package web

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/web/views"
)

const gib = 1 << 30

func gibText(bytes int64) string { return strconv.FormatFloat(float64(bytes)/gib, 'f', -1, 64) }

func pageHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		return strings.Trim(host, "[]")
	}
	return r.Host
}

// backupSettingsView shows the saved settings, or defaults with the single
// approved root, as new settings.
func (s *Server) backupSettingsView(r *http.Request) views.BackupSettingsView {
	b := views.BackupSettingsView{Roots: s.App.Config.BackupRoots, Retention: strconv.Itoa(backups.DefaultRetention),
		MaxGiB: gibText(backups.DefaultMaxBytes), ReserveGiB: gibText(backups.DefaultReserve)}
	saved, _, err := s.App.Backups.SavedSettings(r.Context())
	if err != nil {
		b.Error = publicMessage(err)
	}
	if saved != nil {
		b.Configured = true
		b.Destination, b.Retention = saved.Destination, strconv.Itoa(saved.RetentionCount)
		b.MaxGiB, b.ReserveGiB = gibText(saved.MaxBytes), gibText(saved.ReserveBytes)
	} else if len(b.Roots) == 1 {
		b.Destination = b.Roots[0]
	}
	return b
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, request *Request) {
	view := views.SettingsView{
		Connection: views.ConnectionSettings{Host: s.App.Servers.ConnectionHost(r.Context()), PageHost: pageHost(r)},
		Backups:    s.backupSettingsView(r),
	}
	for _, configuration := range s.App.Notify.Configurations(r.Context()) {
		view.Notifications = append(view.Notifications, views.NotificationDestinationView{Configuration: configuration})
	}
	render(w, r, http.StatusOK, views.SettingsPage(s.page(r, request, "Settings"), view))
}

// settingStatus is the status for a settings form result; false means the
// session ended and the response already redirects to sign-in.
func (s *Server) settingStatus(w http.ResponseWriter, r *http.Request, request *Request, err error) (int, bool) {
	public, ok := apperr.As(err)
	if ok && public.Status == http.StatusUnauthorized {
		s.fail(w, r, request, err)
		return 0, false
	}
	if err == nil {
		return http.StatusOK, true
	}
	if ok {
		return public.Status, true
	}
	return http.StatusInternalServerError, true
}

func (s *Server) saveConnection(w http.ResponseWriter, r *http.Request, request *Request) {
	err := authz.AdminMutationError(s.App.Servers.SetConnectionHost(r.Context(), request.User().ID, request.Principal.TokenHash,
		r.PostFormValue("host")))
	status, ok := s.settingStatus(w, r, request, err)
	if !ok {
		return
	}
	c := views.ConnectionSettings{Host: r.PostFormValue("host"), PageHost: pageHost(r)}
	if err != nil {
		c.Error = publicMessage(err)
	} else {
		s.audit(r, request, "settings.connection_updated", "", nil)
		c.Host = s.App.Servers.ConnectionHost(r.Context())
		c.Notice = "Server address cleared."
		if c.Host != "" {
			c.Notice = "Server address saved."
		}
	}
	render(w, r, status, views.ConnectionForm(c))
}

func (s *Server) saveBackupSettings(w http.ResponseWriter, r *http.Request, request *Request) {
	b := s.backupSettingsView(r)
	b.Destination = strings.TrimSpace(r.PostFormValue("destination"))
	b.Retention, b.MaxGiB, b.ReserveGiB = r.PostFormValue("retentionCount"), r.PostFormValue("maxGiB"), r.PostFormValue("reserveGiB")
	invalid := apperr.New("INVALID_REQUEST", 400,
		"Check the backup settings: enter a destination, 1–1,000 backups per server, a positive total limit, and a free-space reserve of zero or more.")
	retention, retentionErr := strconv.Atoi(strings.TrimSpace(b.Retention))
	maxGiB, maxErr := strconv.ParseFloat(strings.TrimSpace(b.MaxGiB), 64)
	reserveGiB, reserveErr := strconv.ParseFloat(strings.TrimSpace(b.ReserveGiB), 64)
	var err error
	settings := backups.Settings{Destination: b.Destination, RetentionCount: retention,
		MaxBytes: int64(math.Round(maxGiB * gib)), ReserveBytes: int64(math.Round(reserveGiB * gib))}
	if b.Destination == "" || retentionErr != nil || maxErr != nil || reserveErr != nil || retention < 1 || retention > 1000 ||
		settings.MaxBytes <= 0 || settings.ReserveBytes < 0 || math.IsInf(maxGiB, 0) || math.IsInf(reserveGiB, 0) {
		err = invalid
	} else if err = s.recheckAdmin(r.Context(), request); err == nil {
		settings, err = s.App.Backups.SaveSettings(r.Context(), settings, request.User(), request.Principal.TokenHash)
	}
	status, ok := s.settingStatus(w, r, request, err)
	if !ok {
		return
	}
	if err != nil {
		b.Error = publicMessage(err)
	} else {
		s.audit(r, request, "settings.backups.updated", "", nil)
		b.Configured, b.Saved, b.Destination = true, true, settings.Destination
	}
	render(w, r, status, views.BackupSettingsForm(b))
}

func (s *Server) backupStorage(w http.ResponseWriter, r *http.Request, request *Request) {
	status := s.App.Backups.Status(r.Context())
	if err := s.recheckAdmin(r.Context(), request); err != nil {
		s.fail(w, r, request, err)
		return
	}
	render(w, r, http.StatusOK, views.BackupStorage(status))
}

// destination is the destination named in the path; false means the
// response is already written.
func (s *Server) destination(w http.ResponseWriter, r *http.Request, request *Request) (notify.Destination, bool) {
	d, ok := notify.Parse(r.PathValue("destination"))
	if !ok {
		s.fail(w, r, request, apperr.New("NOTIFICATION_DESTINATION_NOT_FOUND", 404, "Notification destination not found"))
	}
	return d, ok
}

func (s *Server) saveNotifications(w http.ResponseWriter, r *http.Request, request *Request) {
	d, ok := s.destination(w, r, request)
	if !ok {
		return
	}
	value := func(name string) string { return strings.TrimSpace(r.PostFormValue(name)) }
	input := notify.Config{Enabled: r.PostFormValue("enabled") == "true", URL: value("url"), Token: value("token"),
		Topic: value("topic"), ChatID: value("chatId"), Host: value("host"), Security: value("security"),
		Username: value("username"), Password: r.PostFormValue("password"), From: value("sender"), To: value("recipients")}
	var err error
	if port := value("port"); port != "" {
		if input.Port, err = strconv.Atoi(port); err != nil || input.Port < 1 {
			err = apperr.New("INVALID_NOTIFICATION_SETTINGS", 400, "Enter a mail server port from 1 to 65535")
		}
	}
	if err == nil {
		err = authz.AdminMutationError(s.App.Notify.Configure(r.Context(), request.User().ID, request.Principal.TokenHash, d,
			input, r.PostFormValue("clearToken") == "true"))
	}
	status, ok := s.settingStatus(w, r, request, err)
	if !ok {
		return
	}
	n := views.NotificationDestinationView{Configuration: s.App.Notify.Configuration(r.Context(), d), Open: true}
	if err != nil {
		// The visible fields keep what was entered; secrets are never echoed.
		saved := n.Saved
		n.Configuration = notify.Configuration{Destination: d, Enabled: input.Enabled, Configured: n.Configured, Saved: saved,
			Config: notify.Config{Enabled: input.Enabled, URL: input.URL, ChatID: input.ChatID, Host: input.Host,
				Port: input.Port, Security: input.Security, Username: input.Username, From: input.From, To: input.To}}
		if d == notify.Discord || d == notify.Webhook {
			n.Config.URL = ""
		}
		n.Error = publicMessage(err)
	} else {
		s.audit(r, request, "notifications.configured", "", map[string]any{"destination": string(d)})
		n.Notice = d.Label() + " settings saved."
		w.Header().Set("HX-Trigger", "notifications-changed")
	}
	render(w, r, status, views.NotificationDestination(n))
}

func (s *Server) deliveriesView(r *http.Request) views.DeliveriesView {
	var d views.DeliveriesView
	deliveries, err := s.App.Notify.Deliveries(r.Context())
	if err != nil {
		d.Error = publicMessage(err)
	}
	d.Deliveries = deliveries
	return d
}

func (s *Server) deliveries(w http.ResponseWriter, r *http.Request, request *Request) {
	render(w, r, http.StatusOK, views.Deliveries(s.deliveriesView(r)))
}

func (s *Server) testNotification(w http.ResponseWriter, r *http.Request, request *Request) {
	d, ok := s.destination(w, r, request)
	if !ok {
		return
	}
	delivery, err := s.App.Notify.QueueTest(r.Context(), request.User().ID, request.Principal.TokenHash, d)
	err = authz.AdminMutationError(err)
	if public, ok := apperr.As(err); ok && public.Status == http.StatusUnauthorized {
		s.fail(w, r, request, err)
		return
	}
	n := views.NotificationDestinationView{Configuration: s.App.Notify.Configuration(r.Context(), d)}
	if err != nil {
		n.TestError = publicMessage(err)
	} else {
		s.audit(r, request, "notifications.test_queued", "", map[string]any{"deliveryId": delivery.ID, "destination": string(d)})
		n.TestNotice = "Test notification queued. Its delivery status appears in Recent deliveries."
		w.Header().Set("HX-Trigger", "notifications-changed")
	}
	render(w, r, http.StatusOK, views.NotificationTest(n))
}

// retryNotification retries a delivery, then shows the list.
func (s *Server) retryNotification(w http.ResponseWriter, r *http.Request, request *Request) {
	delivery, err := s.App.Notify.Retry(r.Context(), request.User().ID, request.Principal.TokenHash, r.PathValue("id"))
	err = authz.AdminMutationError(err)
	if public, ok := apperr.As(err); ok && public.Status == http.StatusUnauthorized {
		s.fail(w, r, request, err)
		return
	}
	if err == nil {
		s.audit(r, request, "notifications.retry_queued", "", map[string]any{"deliveryId": delivery.ID,
			"destination": string(delivery.Destination)})
	}
	d := s.deliveriesView(r)
	if err != nil {
		d.Error = publicMessage(err)
	} else {
		d.Notice = "Notification queued for retry. Delivery status will update below."
	}
	render(w, r, http.StatusOK, views.Deliveries(d))
}

func (s *Server) diagnosticsPage(w http.ResponseWriter, r *http.Request, request *Request) {
	diagnostics, err := s.App.Observer.Diagnostics(r.Context())
	d := views.DiagnosticsView{DockerConnected: err == nil, Diagnostics: diagnostics}
	if err := s.recheckAdmin(r.Context(), request); err != nil {
		s.fail(w, r, request, err)
		return
	}
	render(w, r, http.StatusOK, views.DiagnosticsPage(s.page(r, request, "Diagnostics"), d))
}
