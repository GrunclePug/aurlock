// aurlock is a minimal AUR helper that never installs an AUR package at a
// commit its owner hasn't approved.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/grunclepug/aurlock/internal/app"
)

// Version is set at build time.
var Version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// The first Ctrl-C stops at the next safe point; a second one kills.
	go func() {
		<-ctx.Done()
		stop()
	}()
	fi, err := os.Stdin.Stat()
	tty := err == nil && fi.Mode()&os.ModeCharDevice != 0
	code := app.Run(ctx, app.Env{
		Args:    os.Args[1:],
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		TTY:     tty,
		Getenv:  os.Getenv,
		Version: Version,
	})
	stop()
	os.Exit(code)
}
