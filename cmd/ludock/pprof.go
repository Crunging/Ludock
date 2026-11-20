//go:build pprof

package main

import (
	"fmt"
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
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/queries", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, db.Statements())
	})
	go http.ListenAndServe(address, mux)
}
