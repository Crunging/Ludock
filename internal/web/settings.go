package web

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/advanced"
	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/db"
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
	b := views.BackupSettingsView{Roots: s.App.Backups.Storage.ApprovedRoots(), ReserveGiB: gibText(backups.DefaultReserve)}
	saved, _, err := s.App.Backups.SavedSettings(r.Context())
	if err != nil {
		b.Error = publicMessage(err)
	}
	if saved != nil {
		b.Configured = true
		b.Destination, b.ReserveGiB, b.CopyTo = saved.Destination, gibText(saved.ReserveBytes), saved.CopyTo
		if saved.MaxBytes > 0 {
			b.MaxGiB = gibText(saved.MaxBytes)
		}
	} else if len(b.Roots) > 0 {
		b.Destination = b.Roots[0]
	}
	return b
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, request *Request) {
	view := views.SettingsView{
		Connection:   views.ConnectionSettings{Host: s.App.Servers.ConnectionHost(r.Context()), PageHost: pageHost(r)},
		Backups:      s.backupSettingsView(r),
		ImageUpdates: views.ImageUpdateSettings{Enabled: s.App.Checks.Enabled(r.Context())},
		Advanced: views.AdvancedSettings{Upload: s.uploadLimitView(r), Logging: s.loggingView(r),
			Proxies: s.proxyView(r)},
		Ludock: s.App.Checks.SelfStatus(r.Context()),
		Offer:  s.App.SelfUpdate.Offer(r.Context()),
	}
	healths := s.notificationHealth(r.Context())
	for _, configuration := range s.App.Notify.Configurations(r.Context()) {
		view.Notifications = append(view.Notifications, views.NotificationDestinationView{Configuration: configuration,
			Health: healths[configuration.Destination], Open: r.URL.Query().Get("destination") == string(configuration.Destination)})
	}
	render(w, r, http.StatusOK, views.SettingsPage(s.page(r, request, "Settings"), view))
}

// notificationHealth reads every destination's health; an unreadable one
// shows as unknown rather than failing the page.
func (s *Server) notificationHealth(ctx context.Context) map[notify.Destination]notify.Health {
	healths, err := s.App.Notify.Health(ctx)
	if err != nil {
		log.Warn("Could not read notification delivery health")
	}
	return healths
}

func (s *Server) destinationView(ctx context.Context, d notify.Destination) views.NotificationDestinationView {
	return views.NotificationDestinationView{Configuration: s.App.Notify.Configuration(ctx, d), Health: s.notificationHealth(ctx)[d]}
}

// adminRefused answers an administrator's failed change with its refusal,
// reporting true, once the session is no longer an administrator's. A change
// refused before its write, such as for invalid input, never checked the
// session, so it is checked here before the form shows what is saved.
func (s *Server) adminRefused(w http.ResponseWriter, r *http.Request, request *Request, err error) bool {
	if err == nil {
		return false
	}
	if public, ok := apperr.As(err); !ok || (public.Status != http.StatusUnauthorized && public.Status != http.StatusForbidden) {
		if err = s.recheckAdmin(r.Context(), request); err == nil {
			return false
		}
	}
	s.fail(w, r, request, err)
	return true
}

// settingStatus is the status for a settings form result; false means the
// response, a refusal, is already written.
func (s *Server) settingStatus(w http.ResponseWriter, r *http.Request, request *Request, err error) (int, bool) {
	// An account that is no longer an administrator is shown no settings.
	if s.adminRefused(w, r, request, err) {
		return 0, false
	}
	if err == nil {
		return http.StatusOK, true
	}
	if public, ok := apperr.As(err); ok {
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

// checkLudock asks the registry about Ludock's own image now.
func (s *Server) checkLudock(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	var refused error
	check := s.App.Checks.CheckSelfNow(ctx, func() error {
		refused = authz.AdminMutationError(s.App.DB.AdminMutation(ctx, request.User().ID, request.Principal.TokenHash, db.Now(),
			func(*sql.Tx) error { return nil }))
		return refused
	})
	cancel()
	// The check can wait behind another; the session may have lost the
	// administrator role meanwhile.
	if err := cmp.Or(refused, s.recheckAdmin(r.Context(), request)); err != nil {
		s.fail(w, r, request, err)
		return
	}
	if !s.App.Checks.Enabled(r.Context()) {
		check = nil
	}
	render(w, r, http.StatusOK, views.LudockCheckSaved(check, s.App.SelfUpdate.Offer(r.Context())))
}

// updateLudock starts updating Ludock itself; the notice then waits for it.
func (s *Server) updateLudock(w http.ResponseWriter, r *http.Request, request *Request) {
	err := s.App.SelfUpdate.Start(r.Context(), request.User().ID, request.Principal.TokenHash, request.Client.IP)
	if s.adminRefused(w, r, request, err) {
		return
	}
	offer := s.App.SelfUpdate.Offer(r.Context())
	if err != nil {
		offer.Reason, _ = apperr.Public(err)
	}
	render(w, r, http.StatusOK, views.LudockNotice(s.App.Checks.SelfStatus(r.Context()), offer))
}

// ludockUpdate is the notice again, which a waiting page polls.
func (s *Server) ludockUpdate(w http.ResponseWriter, r *http.Request, request *Request) {
	s.renderAdmin(w, r, request, http.StatusOK, views.LudockNotice(s.App.Checks.SelfStatus(r.Context()), s.App.SelfUpdate.Offer(r.Context())))
}

func (s *Server) saveImageUpdateSettings(w http.ResponseWriter, r *http.Request, request *Request) {
	enabled := r.PostFormValue("enabled") == "true"
	err := authz.AdminMutationError(s.App.Checks.SetEnabled(r.Context(), request.User().ID, request.Principal.TokenHash, enabled))
	status, ok := s.settingStatus(w, r, request, err)
	if !ok {
		return
	}
	u := views.ImageUpdateSettings{Enabled: enabled}
	if err != nil {
		u.Error = publicMessage(err)
	} else {
		s.audit(r, request, "settings.image_updates.updated", "", map[string]any{"enabled": enabled})
		u.Notice = "Image update checks turned off."
		if enabled {
			u.Notice = "Image update checks turned on."
		}
	}
	if err != nil {
		render(w, r, status, views.ImageUpdateSettingsForm(u))
		return
	}
	ludock := s.App.Checks.SelfStatus(r.Context())
	// SelfStatus waits on Docker; the session may have lost the role meanwhile.
	s.renderAdmin(w, r, request, status, views.ImageUpdateSettingsSaved(u, ludock, s.App.SelfUpdate.Offer(r.Context())))
}

func (s *Server) uploadLimitView(r *http.Request) views.UploadLimitSettings {
	limit, fixed := s.App.Advanced.UploadLimit(r.Context())
	u := views.UploadLimitSettings{Fixed: fixed}
	if limit > 0 {
		u.Limit = exactSize(limit)
	}
	return u
}

// exactSize is a size the upload limit field saves back unchanged: rounded
// binary text only when it is exact, else whole decimal units or bytes.
func exactSize(bytes int64) string {
	text := datasize.Format(bytes)
	if parsed, ok := datasize.Parse(text); ok && parsed == bytes {
		return text
	}
	for _, unit := range []struct {
		name string
		size int64
	}{{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}} {
		if bytes%unit.size == 0 {
			return strconv.FormatInt(bytes/unit.size, 10) + " " + unit.name
		}
	}
	return strconv.FormatInt(bytes, 10) + " B"
}

func (s *Server) saveUploadLimit(w http.ResponseWriter, r *http.Request, request *Request) {
	raw := strings.TrimSpace(r.PostFormValue("limit"))
	var limit int64
	var err error
	if raw != "" {
		// The same sizes LUDOCK_MAX_UPLOAD_SIZE accepts.
		var ok bool
		if limit, ok = datasize.Parse(raw); !ok || limit <= 0 {
			err = apperr.New("INVALID_UPLOAD_LIMIT", 400, "Enter a size such as 500 MB, 4 GiB, or 1.5 GiB, or leave it empty for no limit.")
		}
	}
	if err == nil {
		err = authz.AdminMutationError(s.App.Advanced.SetUploadLimit(r.Context(), request.User().ID, request.Principal.TokenHash,
			limit, s.auditEntry(request)))
	}
	status, ok := s.settingStatus(w, r, request, err)
	if !ok {
		return
	}
	u := s.uploadLimitView(r)
	if err != nil {
		u.Limit, u.Error = raw, publicMessage(err)
	} else if limit == 0 {
		u.Notice = "Upload limit removed."
	} else {
		u.Notice = "Upload limit saved."
	}
	render(w, r, status, views.UploadLimitForm(u))
}

func (s *Server) loggingView(r *http.Request) views.LoggingSettings {
	current := s.App.Advanced.Logging(r.Context())
	return views.LoggingSettings{Verbose: current.VerboseUntil > 0, Until: current.VerboseUntil, Level: current.Level, Fixed: current.Fixed}
}

func (s *Server) saveLogging(w http.ResponseWriter, r *http.Request, request *Request) {
	verbose := r.PostFormValue("verbose") == "true"
	err := authz.AdminMutationError(s.App.Advanced.SetVerbose(r.Context(), request.User().ID, request.Principal.TokenHash,
		verbose, s.auditEntry(request)))
	status, ok := s.settingStatus(w, r, request, err)
	if !ok {
		return
	}
	l := s.loggingView(r)
	if err != nil {
		l.Error = publicMessage(err)
	} else if verbose {
		l.Notice = "Verbose logging is on for the next 24 hours."
	} else {
		l.Notice = "Verbose logging is off."
	}
	render(w, r, status, views.LoggingForm(l))
}

func (s *Server) proxyView(r *http.Request) views.ProxySettings {
	current := s.App.Advanced.Proxies(r.Context())
	p := views.ProxySettings{Trusted: current.Trusted, Fixed: current.Fixed}
	if !current.Fixed {
		p.Forwarded, _ = s.App.ClientIP.UntrustedProxy(r)
	}
	return p
}

func (s *Server) saveProxies(w http.ResponseWriter, r *http.Request, request *Request) {
	value, trust := r.PostFormValue("trusted"), r.PostFormValue("trust")
	var err error
	if trust != "" {
		// One click trusts only the proxy this request itself came through,
		// and only while none is trusted.
		if peer, ok := s.App.ClientIP.UntrustedProxy(r); !ok || peer != trust {
			err = advanced.ErrProxiesChanged
		}
		value = trust
	}
	if err == nil {
		err = authz.AdminMutationError(s.App.Advanced.SetProxies(r.Context(), request.User().ID, request.Principal.TokenHash, value,
			trust != "", s.auditEntry(request)))
	}
	status, ok := s.settingStatus(w, r, request, err)
	if !ok {
		return
	}
	p := s.proxyView(r)
	if err != nil {
		p.Error = publicMessage(err)
		// Only a typed list that didn't parse is shown back for correcting.
		if errors.Is(err, advanced.ErrInvalidProxies) && trust == "" {
			p.Trusted = value
		}
	} else if p.Trusted == "" {
		p.Notice = "No proxy is trusted. Each visitor’s address is the one that connects to Ludock."
	} else {
		p.Notice = "Proxy trusted. New requests through it use each visitor’s own address."
	}
	render(w, r, status, views.ProxyForm(p))
}

// copyBeside turns the second copy off when its folder becomes the backup
// folder, which is how backups are reached after the first folder is lost;
// the choice sticks, so the folder isn't picked up again as a found one.
func copyBeside(destination, copyTo string, chosen bool) (string, bool) {
	if copyTo != "" && copyTo == destination {
		return "", true
	}
	return copyTo, chosen
}

func (s *Server) saveBackupSettings(w http.ResponseWriter, r *http.Request, request *Request) {
	b := s.backupSettingsView(r)
	b.Destination = strings.TrimSpace(r.PostFormValue("destination"))
	b.MaxGiB, b.ReserveGiB = strings.TrimSpace(r.PostFormValue("maxGiB")), strings.TrimSpace(r.PostFormValue("reserveGiB"))
	// The form sends copyTo only when it offers a choice; otherwise the
	// saved one, or a found folder, stays.
	_, chosen := r.PostForm["copyTo"]
	b.CopyTo = strings.TrimSpace(r.PostFormValue("copyTo"))
	copyChosen := chosen
	if saved, _, err := s.App.Backups.SavedSettings(r.Context()); !chosen && err == nil && saved != nil {
		b.CopyTo, copyChosen = saved.CopyTo, saved.CopyChosen
	}
	b.CopyTo, copyChosen = copyBeside(b.Destination, b.CopyTo, copyChosen)
	invalid := apperr.New("INVALID_REQUEST", 400,
		"Check the backup settings: choose a backup folder, a minimum free space of 0 or more, and a total limit above 0 (or leave it empty).")
	maxGiB, maxErr := 0.0, error(nil)
	if b.MaxGiB != "" {
		maxGiB, maxErr = strconv.ParseFloat(b.MaxGiB, 64)
	}
	reserveGiB, reserveErr := strconv.ParseFloat(b.ReserveGiB, 64)
	var err error
	settings := backups.Settings{Destination: b.Destination, MaxBytes: int64(math.Round(maxGiB * gib)), ReserveBytes: int64(math.Round(reserveGiB * gib)),
		CopyTo: b.CopyTo, CopyChosen: copyChosen}
	if b.Destination == "" || maxErr != nil || reserveErr != nil || (b.MaxGiB != "" && settings.MaxBytes <= 0) ||
		settings.MaxBytes < 0 || settings.ReserveBytes < 0 || math.IsInf(maxGiB, 0) || math.IsInf(reserveGiB, 0) {
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
	render(w, r, status, views.BackupSettingsSaved(b))
}

func (s *Server) backupStorage(w http.ResponseWriter, r *http.Request, request *Request) {
	status := s.App.Backups.Status(r.Context())
	s.renderAdmin(w, r, request, http.StatusOK, views.BackupStorage(status))
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
	for _, name := range r.PostForm["alerts"] {
		if alert, ok := notify.ParseAlert(name); ok {
			input.Alerts |= alert
		}
	}
	var err error
	if port := value("port"); port != "" {
		if input.Port, err = strconv.Atoi(port); err != nil || input.Port < 1 {
			err = apperr.New("INVALID_NOTIFICATION_SETTINGS", 400, "Enter a mail server port from 1 to 65535")
		}
	}
	if input.Alerts == 0 && err == nil {
		err = apperr.New("INVALID_NOTIFICATION_SETTINGS", 400, "Choose at least one alert type")
	}
	if err == nil {
		err = authz.AdminMutationError(s.App.Notify.Configure(r.Context(), request.User().ID, request.Principal.TokenHash, d,
			input, r.PostFormValue("clearToken") == "true"))
	}
	status, ok := s.settingStatus(w, r, request, err)
	if !ok {
		return
	}
	n := s.destinationView(r.Context(), d)
	n.Open = true
	if err != nil {
		// The visible fields keep what was entered; secrets are never echoed.
		saved := n.Saved
		n.Configuration = notify.Configuration{Destination: d, Enabled: input.Enabled, Configured: n.Configured, Saved: saved,
			Config: notify.Config{Enabled: input.Enabled, Alerts: input.Alerts, URL: input.URL, ChatID: input.ChatID, Host: input.Host,
				Port: input.Port, Security: input.Security, Username: input.Username, From: input.From, To: input.To}}
		if d == notify.Discord || d == notify.Webhook {
			n.Config.URL = ""
		}
		n.Error = publicMessage(err)
		n.NoAlerts = input.Alerts == 0
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
	healths := s.notificationHealth(r.Context())
	for _, c := range s.App.Notify.Configurations(r.Context()) {
		d.Destinations = append(d.Destinations, views.NotificationDestinationView{Configuration: c, Health: healths[c.Destination]})
	}
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
	if s.adminRefused(w, r, request, err) {
		return
	}
	n := s.destinationView(r.Context(), d)
	if err != nil {
		n.TestError = publicMessage(err)
	} else {
		s.audit(r, request, "notifications.test_queued", "", map[string]any{"deliveryId": delivery.ID, "destination": string(d)})
		n.Test = delivery
		w.Header().Set("HX-Trigger", "notifications-changed")
	}
	render(w, r, http.StatusOK, views.NotificationTestSent(n))
}

// testResult follows a test sent from a destination's card.
func (s *Server) testResult(w http.ResponseWriter, r *http.Request, request *Request) {
	d, ok := s.destination(w, r, request)
	if !ok {
		return
	}
	test, err := s.App.Notify.Test(r.Context(), d, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	n := s.destinationView(r.Context(), d)
	n.Test = test
	render(w, r, http.StatusOK, views.NotificationTestUpdate(n))
}

// retryNotification retries a delivery, then shows the list.
func (s *Server) retryNotification(w http.ResponseWriter, r *http.Request, request *Request) {
	delivery, err := s.App.Notify.Retry(r.Context(), request.User().ID, request.Principal.TokenHash, r.PathValue("id"))
	err = authz.AdminMutationError(err)
	if s.adminRefused(w, r, request, err) {
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

func (s *Server) systemPage(w http.ResponseWriter, r *http.Request, request *Request) {
	diagnostics, err := s.App.Observer.Diagnostics(r.Context())
	d := views.SystemView{Version: s.Version, DockerConnected: err == nil, Diagnostics: diagnostics}
	if err != nil {
		d.DockerProblem = s.dockerProblem()
	}
	if err := s.recheckAdmin(r.Context(), request); err != nil {
		s.fail(w, r, request, err)
		return
	}
	render(w, r, http.StatusOK, views.SystemPage(s.page(r, request, "System"), d))
}
