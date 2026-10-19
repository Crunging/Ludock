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

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/config"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
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

func serve() int {
	cfg := config.Load()
	level, invalid := logging.ParseLevel(cfg.LogLevel)
	logging.SetLevel(level)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	clientIP, err := auth.NewClientIP(cfg.TrustedProxies)
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	database, err := db.Open(cfg.DBPath)
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
	servers := &identity.Store{DB: database}
	authorization := &authz.Service{DB: database, Servers: servers}
	accounts := auth.NewService(database, authorization, setup)
	dockerClient := docker.New(cfg.DockerSocket)
	app := &web.Server{DB: database, Auth: accounts, ClientIP: clientIP, Docker: dockerClient.Ping}

	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		log.Error("Could not listen", logging.F("port", cfg.Port))
		return 1
	}
	httpServer := &http.Server{
		Handler:           app.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Info("Ludock listening", logging.F("address", "http://localhost:"+strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)),
		logging.F("logLevel", level.String()), logging.F("version", version))
	if invalid != "" {
		log.Warn("Invalid LOG_LEVEL; using info", logging.F("configuredValue", invalid),
			logging.F("supportedValues", "error,warn,info,debug"))
	}
	accounts.LogSetupInstructions(ctx, func(text string) { os.Stdout.WriteString(text) })

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
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
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
	database, err := db.Open(config.Load().DBPath)
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
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(config.Load().Port) + "/api/v1/health")
	if err != nil {
		return 1
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
