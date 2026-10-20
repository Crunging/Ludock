// Command ludock runs the Ludock web panel and its helper subcommands.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Crunging/Ludock/internal/app"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/config"
	"github.com/Crunging/Ludock/internal/db"
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
	case "healthcheck":
		code = healthcheck()
	case "helper":
		code = helper.Main(args, os.Stdin, os.Stdout, os.Stderr)
	case "version", "--version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "Usage: ludock [serve|recover <username>|healthcheck|helper …|version]\n")
		code = 2
	}
	os.Exit(code)
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
	level, _ := logging.ParseLevel(cfg.LogLevel)
	logging.SetLevel(level)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(cfg.DBPath())
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	defer database.Close()
	setup, err := auth.NewSetupWindow(func() int64 { return time.Now().UnixMilli() }, auth.SetupWindowMs, cfg.SetupCode)
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	services, err := app.New(cfg, database, setup)
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	accounts := services.Auth
	server := &web.Server{App: services, DB: database, Auth: accounts, ClientIP: services.ClientIP, Docker: services.Docker.Ping}

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
		logging.F("logLevel", level.String()), logging.F("version", version))
	accounts.LogSetupInstructions(ctx, func(text string) { os.Stdout.WriteString(text) })

	services.Start(ctx)
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP server failed")
			return 1
		}
	case <-ctx.Done():
	}
	log.Info("Shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// End long-lived connections first: Shutdown waits for every handler,
	// and event streams and console sockets otherwise stay open. Background
	// work and held locks settle before the database closes.
	server.CloseSockets()
	server.CloseStreams()
	failed := httpServer.Shutdown(shutdownCtx) != nil
	services.Stop()
	if failed {
		log.Error("Failed to shut down cleanly")
		return 1
	}
	log.Info("Shutdown complete")
	return 0
}

// recoverAccount resets a password from the console. The new password comes
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
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	defer database.Close()
	user, err := auth.RecoverPassword(context.Background(), database, username, password)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	if user == nil {
		fmt.Fprintf(os.Stderr, "Account %q was not found.\n", username)
		return 1
	}
	fmt.Printf("Password reset for %q. All existing sessions were revoked.\n", user.Username)
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
