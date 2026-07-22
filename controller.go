// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/oxidecomputer/oxide.go/oxide"

	"github.com/oxidecomputer/oxide-github-actions-runner-scaleset/internal/config"
	"github.com/oxidecomputer/oxide-github-actions-runner-scaleset/internal/scaler"
)

type controller struct {
	config         *config.Config
	logger         *slog.Logger
	scaleSetClient *scaleset.Client
	oxideClient    *oxide.Client
	systemInfo     scaleset.SystemInfo
}

func newController(
	cfg *config.Config,
	systemInfo scaleset.SystemInfo,
) (*controller, error) {
	logger := cfg.Logger().WithGroup(systemInfo.Subsystem)

	scaleSetClient, err := cfg.ScaleSetClient(systemInfo)
	if err != nil {
		return nil, fmt.Errorf("creating scale set client: %w", err)
	}

	oxideClient, err := cfg.OxideClient()
	if err != nil {
		return nil, fmt.Errorf("creating oxide client: %w", err)
	}

	logger.Info("clients created",
		"github.config_url", cfg.GitHub.ConfigURL,
		"scale_set.name", cfg.ScaleSet.Name,
	)

	return &controller{
		config:         cfg,
		logger:         logger,
		scaleSetClient: scaleSetClient,
		oxideClient:    oxideClient,
		systemInfo:     systemInfo,
	}, nil
}

// Run adopts or creates the configured scale set, starts its listener and
// scaler, and waits until one exits or the context is canceled.
func (c *controller) Run(ctx context.Context) (err error) {
	runnerScaleSet, err := c.ensureScaleSet(ctx)
	if err != nil {
		return fmt.Errorf("ensuring scale set: %w", err)
	}
	c.systemInfo.ScaleSetID = runnerScaleSet.ID
	c.scaleSetClient.SetSystemInfo(c.systemInfo)

	logger := c.logger.With(
		"scale_set.name", runnerScaleSet.Name,
		"scale_set.id", runnerScaleSet.ID,
	)

	maxRunners := int(c.config.ScaleSet.MaxRunners)
	runnerScaler, err := c.newScaler(runnerScaleSet.ID, logger)
	if err != nil {
		return fmt.Errorf("creating oxide scaler: %w", err)
	}

	sessionClient, err := c.scaleSetClient.MessageSessionClient(
		ctx,
		runnerScaleSet.ID,
		applicationName,
	)
	if err != nil {
		return fmt.Errorf(
			"scale set %q: creating message session: %w",
			runnerScaleSet.Name, err,
		)
	}

	deleteScaleSet := false
	defer func() {
		cleanupCtx := context.WithoutCancel(ctx)
		if closeErr := sessionClient.Close(cleanupCtx); closeErr != nil {
			logger.Error("failed to close listener session",
				"error", closeErr,
			)
			err = errors.Join(err, fmt.Errorf(
				"scale set %q: closing listener session: %w",
				runnerScaleSet.Name, closeErr,
			))
		}

		if !deleteScaleSet {
			return
		}
		if deleteErr := c.scaleSetClient.DeleteRunnerScaleSet(
			cleanupCtx, runnerScaleSet.ID,
		); deleteErr != nil {
			err = errors.Join(err, fmt.Errorf(
				"scale set %q: deleting after drain: %w",
				runnerScaleSet.Name, deleteErr,
			))
			return
		}
		logger.Info("drained scale set deleted")
	}()

	scaleSetListener, err := listener.New(
		sessionClient,
		listener.Config{
			ScaleSetID: runnerScaleSet.ID,
			MaxRunners: maxRunners,
			Logger:     logger.WithGroup("listener"),
		},
	)
	if err != nil {
		return fmt.Errorf(
			"scale set %q: creating listener: %w",
			runnerScaleSet.Name, err,
		)
	}

	if maxRunners == 0 {
		logger.Info("scale set is draining; no new jobs will be acquired")
	}

	logger.Info("starting listener and scaler")
	listenerErr, scalerErr := runListenerAndScaler(
		ctx,
		func(ctx context.Context) error {
			return scaleSetListener.Run(ctx, runnerScaler)
		},
		runnerScaler.Run,
	)

	if errors.Is(scalerErr, scaler.ErrScaleSetDrained) {
		deleteScaleSet = true
		logger.Info("scale set drain complete")
		return nil
	}
	if scalerErr != nil && !errors.Is(scalerErr, context.Canceled) {
		return fmt.Errorf(
			"scale set %q: running scaler: %w",
			runnerScaleSet.Name, scalerErr,
		)
	}
	if listenerErr != nil && !errors.Is(listenerErr, context.Canceled) {
		return fmt.Errorf(
			"scale set %q: running listener: %w",
			runnerScaleSet.Name, listenerErr,
		)
	}

	return nil
}

func (c *controller) newScaler(
	scaleSetID int,
	logger *slog.Logger,
) (*scaler.Scaler, error) {
	scaleSetConfig := c.config.ScaleSet
	return scaler.New(
		c.oxideClient,
		c.scaleSetClient,
		scaler.Config{
			Instance: scaler.InstanceConfig(scaleSetConfig.Instance),
			Logger: logger.WithGroup("scaler").With(
				"project", scaleSetConfig.Instance.Project,
			),
			ScaleSet: scaler.ScaleSetConfig{
				Namespace: c.config.GitHub.ConfigURL,
				ID:        scaleSetID,
			},
			MinRunners: int(scaleSetConfig.MinRunners),
			MaxRunners: int(scaleSetConfig.MaxRunners),
		},
	)
}

// runListenerAndScaler runs both components as peers. On process cancellation,
// it stops and awaits the listener before canceling the scaler so the listener
// can finish handling any message already in progress.
func runListenerAndScaler(
	ctx context.Context,
	runListener func(context.Context) error,
	runScaler func(context.Context) error,
) (listenerErr, scalerErr error) {
	listenerCtx, cancelListener := context.WithCancel(
		context.WithoutCancel(ctx),
	)
	defer cancelListener()
	scalerCtx, cancelScaler := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelScaler()

	listenerErrCh := make(chan error, 1)
	scalerErrCh := make(chan error, 1)
	go func() {
		listenerErrCh <- runListener(listenerCtx)
	}()
	go func() {
		scalerErrCh <- runScaler(scalerCtx)
	}()

	select {
	case <-ctx.Done():
		cancelListener()
		listenerErr = <-listenerErrCh
		cancelScaler()
		scalerErr = <-scalerErrCh
	case listenerErr = <-listenerErrCh:
		cancelScaler()
		scalerErr = <-scalerErrCh
	case scalerErr = <-scalerErrCh:
		cancelListener()
		listenerErr = <-listenerErrCh
	}

	return listenerErr, scalerErr
}
