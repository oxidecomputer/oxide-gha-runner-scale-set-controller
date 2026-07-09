package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/oxidecomputer/oxide-actions-scaleset/internal/config"
	"github.com/oxidecomputer/oxide-actions-scaleset/internal/scalerv2"
	"github.com/oxidecomputer/oxide.go/oxide"
)

type controller struct {
	config         *config.Config
	logger         *slog.Logger
	scaleSetClient *scaleset.Client
	oxideClient    *oxide.Client
}

func newController(
	cfg *config.Config,
	systemInfo scaleset.SystemInfo,
) (*controller, error) {
	logger := cfg.Logger().WithGroup(systemInfo.Subsystem)

	scalesetClient, err := cfg.ScaleSetClient(systemInfo)
	if err != nil {
		return nil, fmt.Errorf("creating scaleset client: %w", err)
	}

	oxideClient, err := cfg.OxideClient()
	if err != nil {
		return nil, fmt.Errorf("creating oxide client: %w", err)
	}

	logger.Info("scale set client created",
		"github.config_url", cfg.GitHub.ConfigURL,
		"scale_set.name", cfg.ScaleSet.Name,
	)

	return &controller{
		config:         cfg,
		logger:         logger,
		scaleSetClient: scalesetClient,
		oxideClient:    oxideClient,
	}, nil
}

// Run drives the controller logic. It ensures the scale set exists, opens
// a listener message session on the scale set, and runs the Oxide scaler to
// reponse to scale set events.
func (c *controller) Run(ctx context.Context) (err error) {
	shutdownTimeout := c.config.ShutdownTimeoutDuration()

	scaleSet, err := c.ensureScaleSet(ctx)
	if err != nil {
		return fmt.Errorf("creating scale set: %w", err)
	}

	logger := c.logger.With(
		"scale_set.name", scaleSet.Name,
		"scale_set.id", scaleSet.ID,
	)
	minRunners := int(c.config.ScaleSet.MinRunners)
	maxRunners := int(c.config.ScaleSet.MaxRunners)

	// oxideScaler, err := scaler.New(scaler.Config{
	// 	OxideClient:    c.oxideClient,
	// 	ScaleSetClient: c.scaleSetClient,
	// 	Instance:       &c.config.ScaleSet.Instance,
	// 	Logger:         c.logger,
	// 	ScaleSetName:   c.config.ScaleSet.Name,
	// 	ScaleSetID:     scaleSet.ID,
	// 	MinRunners:     minRunners,
	// 	MaxRunners:     maxRunners,
	// })
	oxideScaler, err := scalerv2.New(
		c.oxideClient,
		c.scaleSetClient,
		&c.config.ScaleSet.Instance,
		c.logger,
		c.config.ScaleSet.Name,
		scaleSet.ID,
		minRunners,
		maxRunners,
	)
	if err != nil {
		return fmt.Errorf("creating oxide scaler: %w", err)
	}

	messageSessionClient, err := c.scaleSetClient.MessageSessionClient(ctx, scaleSet.ID, "oxide-actions-scaleset")
	if err != nil {
		return fmt.Errorf("scale set %q: creating message session: %w", scaleSet.Name, err)
	}

	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancelClose()
		if closeErr := messageSessionClient.Close(closeCtx); closeErr != nil {
			logger.Error("failed to close listener session",
				"error", closeErr,
			)
			err = errors.Join(err, fmt.Errorf(
				"scale set %q: closing listener session: %w",
				scaleSet.Name, closeErr,
			))
		}
	}()

	scalesetListener, err := listener.New(
		messageSessionClient,
		listener.Config{
			ScaleSetID: scaleSet.ID,
			MaxRunners: maxRunners,
			Logger:     logger.WithGroup("listener"),
		},
	)
	if err != nil {
		return fmt.Errorf("scale set %q: creating listener: %w", scaleSet.Name, err)
	}

	// A canceled listener context requests graceful scaler shutdown below.
	// Keep the scaler's hard-cancellation context detached so an in-flight
	// runner transaction can finish unless the shutdown timeout expires.
	listenerCtx, cancelListener := context.WithCancel(ctx)
	defer cancelListener()
	scalerCtx, cancelScaler := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelScaler()

	scalerErrCh := make(chan error, 1)
	go func() {
		defer cancelListener()
		scalerErrCh <- oxideScaler.Run(scalerCtx)
	}()

	if c.config.ScaleSet.MaxRunners == 0 {
		logger.Info("scale set is draining; no new jobs will be acquired")
	}

	logger.Info("starting listener")
	listenerErr := scalesetListener.Run(listenerCtx, oxideScaler)

	// Stop accepting messages, then let the scaler finish its current runner
	// transaction. If graceful shutdown times out, hard cancellation aborts
	// the in-flight API operation.
	cancelListener()
	// logger.Info("listener stopped; waiting for scaler shutdown",
	// 	"shutdown.timeout", shutdownTimeout,
	// )
	// shutdownCtx, cancelShutdown := context.WithTimeout(
	// 	context.WithoutCancel(ctx), shutdownTimeout,
	// )
	// shutdownErr := oxideScaler.Shutdown(shutdownCtx)
	// cancelShutdown()
	// if shutdownErr != nil {
	// 	logger.Warn("graceful scaler shutdown timed out",
	// 		"error", shutdownErr,
	// 	)
	cancelScaler()
	// }
	scalerErr := <-scalerErrCh

	if scalerErr != nil && !errors.Is(scalerErr, context.Canceled) {
		return fmt.Errorf("scale set %q: running scaler: %w", scaleSet.Name, scalerErr)
	}
	if listenerErr != nil && !errors.Is(listenerErr, context.Canceled) {
		return fmt.Errorf("scale set %q: running listener: %w", scaleSet.Name, listenerErr)
	}

	return nil
}
