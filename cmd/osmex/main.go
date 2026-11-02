// Command osmex is a project-scoped OpenStack exporter for Prometheus.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/krisiasty/osmex/internal/app"
	"github.com/krisiasty/osmex/internal/config"
)

// version, commit and date are set at build time with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
// date is when the binary was built, in RFC 3339 format.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// versionLine describes the build for -version. Without a commit from the build flags, it uses the one that the Go
// toolchain recorded when building from a checkout; there is no fallback for the build date.
func versionLine(info *debug.BuildInfo) string {
	rev, built := commit, date
	if rev == "" && info != nil {
		modified := false
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		if rev != "" && modified {
			rev += " (modified)"
		}
	}
	if rev == "" {
		rev = "unknown"
	}
	if built == "" {
		built = "unknown"
	}
	return fmt.Sprintf("osmex %s (commit %s, built %s)", version, rev, built)
}

func main() { os.Exit(run()) }

func run() int {
	cfg, err := config.Parse(os.Args[1:], os.Getenv)
	var help *config.HelpError
	switch {
	case errors.As(err, &help):
		fmt.Fprint(os.Stderr, help.Usage)
		return 0
	case err != nil:
		fmt.Fprintln(os.Stderr, "osmex:", err)
		return 2
	case cfg.ShowVersion:
		info, _ := debug.ReadBuildInfo()
		fmt.Println(versionLine(info))
		return 0
	}
	if err := cfg.LoadSecret(); err != nil {
		fmt.Fprintln(os.Stderr, "osmex:", err)
		return 2
	}

	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	log := slog.New(handler)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("starting osmex", "version", version, "services", cfg.Services, "all", cfg.All, "excluded", cfg.Excluded)
	for _, m := range cfg.UnusedExclusions {
		log.Warn("-no-"+m+" has no effect: the service is not enabled", "service", m)
	}
	if err := app.Run(ctx, cfg, app.Options{Version: version, Logger: log}); err != nil {
		log.Error("exiting", "err", err)
		return 1
	}
	return 0
}
