// Command fakedocker serves an in-memory Docker Engine API for development
// and browser tests. It never talks to a real daemon.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Crunging/Ludock/internal/docker/fakedocker"
)

func main() {
	directory := flag.String("dir", "", "directory for the socket and server data")
	self := flag.String("self", "ludock-dev", "name of the fake Ludock container (LUDOCK_SELF_CONTAINER)")
	backups := flag.String("backups", "", "backup roots mounted into the fake Ludock container, ':'-separated")
	data := flag.String("data", "", "Ludock's data folder, mounted into the fake Ludock container (LUDOCK_DATA_DIR)")
	demo := flag.Bool("demo", false, "seed the servers shown in the README screenshots")
	extra := flag.Int("extra", 0, "add this many more Minecraft servers, each with its own console and data folder")
	socket := flag.String("socket", "", "socket path, if not DIR/docker.sock (unix socket paths are limited to about 100 bytes)")
	flag.Parse()
	if *directory == "" {
		fmt.Fprintln(os.Stderr, "Usage: fakedocker -dir DIR [-backups DIR] [-demo]")
		os.Exit(2)
	}
	os.MkdirAll(*directory, 0o755)
	// A chosen socket path is never removed first, in case it is a real one.
	start := func() (*fakedocker.Engine, error) { return fakedocker.StartSocket(*socket) }
	if *socket == "" {
		start = func() (*fakedocker.Engine, error) { return fakedocker.Start(*directory) }
	}
	engine, err := start()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	seed(engine, *directory, *self, *backups, *data, *demo)
	for index := 1; index <= *extra; index++ {
		seedExtra(engine, *directory, index)
	}
	fmt.Println(engine.Socket)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	engine.Close()
}
