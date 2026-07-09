package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide-actions-scaleset/internal/config"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	showVersion := flag.Bool("version", false, "Print version")
	flag.Parse()

	info := getBuildInfo()
	if *showVersion {
		fmt.Printf("oxide-actions-scaleset %s\n", info.version)
		return
	}

	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "oxide-actions-scaleset: %v\n", err)
		os.Exit(1)
	}
	logger := cfg.Logger()

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	controller, err := newController(cfg, scaleset.SystemInfo{
		System:    "oxide-actions-scaleset",
		Version:   info.version,
		CommitSHA: info.vcsRevision,
		Subsystem: "controller",
	})
	if err != nil {
		logger.Error("exiting", "error", err)
		os.Exit(1)
	}

	if err := controller.Run(ctx); err != nil {
		logger.Error("exiting", "error", err)
		os.Exit(1)
	}
}

var buildInfo BuildInfo

type BuildInfo struct {
	version     string
	vcsRevision string
}

func getBuildInfo() BuildInfo {
	version := buildInfo.version
	vcsRevision := buildInfo.vcsRevision
	info, ok := debug.ReadBuildInfo()
	if ok {
		if version == "" {
			version = info.Main.Version
		}
		if vcsRevision == "" {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					vcsRevision = setting.Value
					break
				}
			}
		}
	}

	if version == "" {
		version = "unknown"
	}
	if vcsRevision == "" {
		vcsRevision = "unknown"
	}

	return BuildInfo{
		version:     version,
		vcsRevision: vcsRevision,
	}
}
