package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/eumarumar/concurtest/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	exitCode := app.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()

	os.Exit(exitCode)
}
