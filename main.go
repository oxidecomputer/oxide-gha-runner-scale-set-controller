// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/actions/scaleset"

	"github.com/oxidecomputer/oxide-github-actions-runner-scaleset/internal/config"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	showVersion := flag.Bool("version", false, "Print version")
	flag.Parse()

	info := getBuildInfo()
	if *showVersion {
		fmt.Printf("%s %s\n", applicationName, info.version)
		return 0
	}

	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", applicationName, err)
		return 1
	}
	logger := cfg.Logger()

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	controller, err := newController(cfg, scaleset.SystemInfo{
		System:    applicationName,
		Version:   info.version,
		CommitSHA: info.commit,
		Subsystem: "controller",
	})
	if err != nil {
		logger.Error("exiting", "error", err)
		return 1
	}

	if err := controller.Run(ctx); err != nil {
		logger.Error("exiting", "error", err)
		return 1
	}
	return 0
}
