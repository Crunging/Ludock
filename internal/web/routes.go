package web

// routes registers the signed-in pages, fragments, and actions.
func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /{$}", s.authed(s.dashboard))
	m.HandleFunc("GET /fragments/servers", s.authed(s.serversFragment))
	m.HandleFunc("GET /fragments/attention", s.authed(s.attentionFragment))
	m.HandleFunc("GET /events", s.authed(s.events))
	m.HandleFunc("POST /servers/{id}/start", s.authed(s.startServer))
	m.HandleFunc("POST /servers/{id}/stop", s.authed(s.queueLifecycle("stop")))
	m.HandleFunc("POST /servers/{id}/restart", s.authed(s.queueLifecycle("restart")))
}
