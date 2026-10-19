package web

// routes registers the signed-in pages, fragments, and actions.
func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /{$}", s.authed(s.dashboard))
	m.HandleFunc("GET /fragments/servers", s.authed(s.serversFragment))
	m.HandleFunc("GET /fragments/attention", s.authed(s.attentionFragment))
	m.HandleFunc("GET /events", s.authed(s.events))
	m.HandleFunc("GET /account", s.authed(s.account))
	m.HandleFunc("GET /fragments/account/sessions", s.authed(s.accountSessions))
	m.HandleFunc("POST /account/password", s.authed(s.changePassword))
	m.HandleFunc("POST /account/sessions/{id}/revoke", s.authed(s.revokeSession))
	m.HandleFunc("GET /servers/{id}", s.authed(s.serverDetail))
	m.HandleFunc("GET /fragments/servers/{id}/live", s.authed(s.detailLive))
	m.HandleFunc("GET /fragments/servers/{id}/overview", s.authed(s.detailOverview))
	m.HandleFunc("GET /fragments/servers/{id}/activity", s.authed(s.detailActivity))
	m.HandleFunc("GET /fragments/servers/{id}/operations/{operation}", s.authed(s.detailOperation))
	m.HandleFunc("POST /servers/{id}/availability", s.admin(s.configureAvailability))
	m.HandleFunc("POST /servers/{id}/binding-review", s.admin(s.reviewBinding))
	m.HandleFunc("POST /servers/{id}/start", s.authed(s.startServer))
	m.HandleFunc("POST /servers/{id}/stop", s.authed(s.queueLifecycle("stop")))
	m.HandleFunc("POST /servers/{id}/restart", s.authed(s.queueLifecycle("restart")))
}
