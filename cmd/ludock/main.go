// Command ludock runs the Ludock web panel and its helper subcommands.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/selfupdate"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Crunging/Ludock/internal/app"
	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/config"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/web"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

var log = logging.New("server")

func main() {
	args := os.Args[1:]
	command := "serve"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	var code int
	switch command {
	case "serve":
		code = serve()
	case "recover":
		code = recoverAccount(args)
	case "restore-data":
		code = restoreData(args)
	case "healthcheck":
		code = healthcheck()
	case "helper":
		code = helper.Main(args, os.Stdin, os.Stdout, os.Stderr)
	case "self-update":
		code = selfUpdate(args)
	case "version", "--version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "Usage: ludock [serve|recover <username>|restore-data <file>|healthcheck|helper …|self-update …|version]\n")
		code = 2
	}
	os.Exit(code)
}

// selfUpdate runs the helper that replaces Ludock's container with one of
// the pulled image, from a container of Ludock's own image, and exits with
// how it ended.
func selfUpdate(args []string) int {
	if len(args) != 3 || !identity.ValidID(args[2]) {
		log.Error("Ludock's update helper got arguments it can't read")
		return 2
	}
	socket := cmp.Or(os.Getenv("LUDOCK_DOCKER_SOCKET"), config.DefaultDockerSocket)
	return (&selfupdate.Helper{Docker: docker.New(socket), Container: args[0], Image: args[1], Operation: args[2]}).Run(context.Background())
}

// loadConfig reads the environment, reporting every invalid setting.
func loadConfig() (config.Config, bool) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Invalid configuration:\n"+err.Error())
		return cfg, false
	}
	return cfg, true
}

func serve() int {
	cfg, ok := loadConfig()
	if !ok {
		return 2
	}
	if cfg.LogLevel != "" {
		level, _ := logging.ParseLevel(cfg.LogLevel)
		logging.SetLevel(level)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// An update's helper that put this container back left the data copy
	// for it to restore before it opens the database.
	putBack, err := selfupdate.BeforeOpen(ctx, docker.New(cfg.DockerSocket), cfg.MountID, cfg.DataDir, time.Minute)
	if err != nil {
		log.Error(startupError(err, "Could not read Ludock's data to check for an unfinished update."))
		return 1
	}
	database, err := db.Open(cfg.DBPath())
	if apperr.Is(err, "DATABASE_INCOMPATIBLE") {
		return refuse(ctx, cfg.Listen, startupError(err, "")+selfupdate.Unfinished(cfg.DataDir))
	}
	if err != nil {
		log.Error(startupError(err, "Could not open the database. Check that LUDOCK_DATA_DIR exists and is writable."))
		return 1
	}
	defer database.Close()
	setup, err := auth.NewSetup(cfg.SetupCode)
	if err != nil {
		log.Error(startupError(err, "Could not prepare initial setup. Check LUDOCK_SETUP_CODE."))
		return 1
	}
	services, err := app.New(cfg, database, setup)
	if err != nil {
		log.Error(startupError(err, "Could not start Ludock's services."))
		return 1
	}
	services.SelfUpdate.Version, services.SelfUpdate.PutBack = version, putBack
	accounts := services.Auth
	server := &web.Server{App: services, DB: database, Auth: accounts, ClientIP: services.ClientIP, Docker: services.Docker.Ping, Version: version}

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("Could not listen", logging.F("address", cfg.Listen))
		return 1
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Info("Ludock listening", logging.F("address", "http://localhost:"+strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)),
		logging.F("logLevel", logging.CurrentLevel().String()), logging.F("version", version))
	accounts.LogSetupInstructions(ctx, func(text string) { os.Stdout.WriteString(text) })

	services.Start(ctx)
	// Ludock keeps running so its pages can tell the owner too.
	go func() {
		if err := services.Docker.Negotiate(ctx); apperr.Is(err, "DOCKER_INCOMPATIBLE") {
			log.Error(err.Error())
		}
	}()
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	// Exiting with an error lets the container's restart policy start Ludock
	// again, which mounts the socket Docker made.
	code := 0
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP server failed")
			return 1
		}
	case <-ctx.Done():
	case <-socketReplaced(ctx, cfg.DockerSocket):
		log.Error("Docker replaced its socket, which this container can't reach again; restarting to reach the new one")
		code = 1
	}
	// Running work gets a few seconds to finish, well inside the ten Docker
	// waits before killing the container. Work still running then is left as
	// a kill would leave it: operations are saved before they act, and
	// startup recovery resumes or reconciles them. A second signal exits at
	// once.
	stop()
	log.Info("Shutting down; signal again to exit without waiting for running operations")
	deadline := time.Now().Add(shutdownGrace)
	shutdownCtx, cancel := context.WithDeadline(context.Background(), deadline.Add(-shutdownGrace/2))
	defer cancel()
	// End long-lived connections first: Shutdown waits for every handler, and
	// event streams and console sockets otherwise stay open.
	server.CloseSockets()
	server.CloseStreams()
	failed := httpServer.Shutdown(shutdownCtx) != nil
	if !services.Stop(deadline) {
		// Exiting without closing the database leaves that work exactly as
		// a kill would, the case recovery is built for.
		log.Warn("Stopped before running work finished; it resumes or is checked on the next start")
		os.Exit(code)
	}
	if failed {
		log.Error("Failed to shut down cleanly")
		return 1
	}
	log.Info("Shutdown complete")
	return code
}

// socketReplaced closes once Docker's socket in this container turns out to
// be a deleted file.
func socketReplaced(ctx context.Context, socket string) <-chan struct{} {
	replaced := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if docker.SocketReplaced(socket) {
					close(replaced)
					return
				}
			}
		}
	}()
	return replaced
}

// shutdownGrace is how long a stopping Ludock waits for running work.
const shutdownGrace = 6 * time.Second

// recoverAccount resets a password and turns off two-factor sign-in from the
// console. The new password comes
// from LUDOCK_RECOVERY_PASSWORD so it stays out of the process list.
func recoverAccount(args []string) int {
	username := ""
	if len(args) > 0 {
		username = strings.TrimSpace(args[0])
	}
	password := os.Getenv("LUDOCK_RECOVERY_PASSWORD")
	if username == "" || password == "" {
		fmt.Fprintln(os.Stderr, "Usage: LUDOCK_RECOVERY_PASSWORD='<new password>' ludock recover <username>")
		return 2
	}
	if !auth.ValidPassword(password) {
		fmt.Fprintf(os.Stderr, "Recovery password must be between %d and %d characters.\n", auth.PasswordMin, auth.PasswordMax)
		return 2
	}
	cfg, ok := loadConfig()
	if !ok {
		return 2
	}
	database, err := db.Open(cfg.DBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, startupError(err, "Could not open the database. Check that LUDOCK_DATA_DIR exists and is writable."))
		return 1
	}
	defer database.Close()
	user, err := auth.RecoverPassword(context.Background(), database, username, password)
	if errors.Is(err, db.ErrLudockUpdating) {
		fmt.Fprintln(os.Stderr, "Ludock is updating itself, and putting the old version back would undo this. Start Ludock and let the update finish, then run this again.")
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, startupError(err, "Could not reset the password."))
		return 1
	}
	if user == nil {
		fmt.Fprintf(os.Stderr, "Account %q was not found.\n", username)
		return 1
	}
	if user.TwoFactor {
		fmt.Printf("Password reset and two-factor sign-in turned off for %q. All existing sessions were revoked.\n", user.Username)
		return 0
	}
	fmt.Printf("Password reset for %q. All existing sessions were revoked.\n", user.Username)
	return 0
}

// restoreData restores a saved copy of Ludock's data into an empty data
// folder, after checking that the copy is whole and its key matches.
func restoreData(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: ludock restore-data <ludock-data-….tar>")
		return 2
	}
	cfg, ok := loadConfig()
	if !ok {
		return 2
	}
	savedAt, err := db.RestoreSnapshot(args[0], cfg.DBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, startupError(err, "Could not restore Ludock's data. Check that the file can be read and the data folder is writable."))
		return 1
	}
	fmt.Printf("Restored Ludock's data as it was saved at %s. Start Ludock to use it.\n", savedAt.Local().Format("2006-01-02 15:04 MST"))
	return 0
}

// healthcheck backs the image HEALTHCHECK without shipping wget.
func healthcheck() int {
	cfg, ok := loadConfig()
	if !ok {
		return 2
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(cfg.HealthURL())
	if err != nil {
		return 1
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// refuse keeps running with a database Ludock can't use, saying why once in
// its log and on every page, until it is stopped: exiting would have a
// container restarted over and over with the reason buried in its log.
func refuse(ctx context.Context, address, message string) int {
	log.Error(message)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Error("Could not listen", logging.F("address", address))
		return 1
	}
	server := &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, message)
	})}
	go server.Serve(listener)
	<-ctx.Done()
	server.Close()
	return 0
}

// startupError describes a failure without its underlying message, which can
// name host paths: a public error keeps its message, anything else gets the
// fixed description.
func startupError(err error, fallback string) string {
	if public, ok := apperr.As(err); ok {
		return public.Message
	}
	return fallback
}
