package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/startup"
)

func main() {
	cfg, err := config.Load()
	startup.PrintBanner(os.Stdout)
	if err != nil {
		config.NewLogger(config.LevelInfo).Server("app").Fatal("app.config.invalid", "invalid runtime configuration", config.ErrorField(err))
	}
	rootLog := config.NewLogger(cfg.LogLevel)
	log := rootLog.Server("app")
	log.Debug("app.config.loaded", "loaded runtime configuration", config.F("log_level", cfg.LogLevel.String()))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = startup.Run(ctx, cfg, rootLog, os.Stdout)
	stop()
	if err != nil {
		var startupErr *startup.Error
		if errors.As(err, &startupErr) {
			log.Fatal(startupErr.Event, startupErr.Message, config.ErrorField(startupErr.Cause))
		}
		log.Fatal("app.start.failed", "application startup failed", config.ErrorField(err))
	}
}
