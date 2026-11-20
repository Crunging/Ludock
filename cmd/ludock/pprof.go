//go:build pprof

package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"

	"github.com/Crunging/Ludock/internal/db"
)

// Built with -tags pprof, Ludock serves profiles and its SQL statement count
// on LUDOCK_PPROF (a loopback address) for measuring; release builds have
// neither.
func init() {
	address := os.Getenv("LUDOCK_PPROF")
	if address == "" {
		return
	}
	// Profiles can hold secrets and nothing signs in here.
	if !loopback(address) {
		fmt.Fprintln(os.Stderr, "LUDOCK_PPROF must be a loopback IP address and port, such as 127.0.0.1:6060; profiling is off")
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/queries", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, db.Statements())
	})
	go http.ListenAndServe(address, mux)
}

// loopback reports whether address names a loopback IP literal: a name such
// as localhost can resolve elsewhere, and an empty host listens everywhere.
func loopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}
