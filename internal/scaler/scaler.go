// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide.go/oxide"
)

// Scaler implements [github.com/actions/scaleset/listener.Scaler] to
// manage the lifecycle of Oxide instances as ephemeral GitHub Actions
// runners. It uses the GitHub Actions Scale Set API to provision runners
// within a scale set and decommission them when they are no longer needed.
//
// Scaler uses a reconciliation pattern to scale. The listener callbacks record
// demand and job events. The loop started by [Scaler.Run] rebuilds a unified
// view of every runner from GitHub and Oxide resources and converges it on the
// demanded runner count. See [Scaler.Run] for more information.
type Scaler struct {
	oxideClient    OxideClient
	scalesetClient ScaleSetClient
	logger         *slog.Logger

	scaleSet       ScaleSetConfig
	instanceConfig InstanceConfig
	minRunners     int
	maxRunners     int

	mu                 sync.Mutex
	desiredRunnerCount int
	jobEvents          []jobEvent

	activeRunnerCount atomic.Int64

	wakeCh chan struct{}
}

// New constructs a [Scaler] ready for use.
func New(
	oxideClient OxideClient,
	scalesetClient ScaleSetClient,
	config Config,
) (*Scaler, error) {
	if oxideClient == nil {
		return nil, fmt.Errorf("oxide client is required")
	}
	if scalesetClient == nil {
		return nil, fmt.Errorf("scale set client is required")
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Scaler{
		oxideClient:        oxideClient,
		scalesetClient:     scalesetClient,
		instanceConfig:     config.Instance,
		logger:             logger,
		scaleSet:           config.ScaleSet,
		minRunners:         config.MinRunners,
		maxRunners:         config.MaxRunners,
		mu:                 sync.Mutex{},
		desiredRunnerCount: -1,
		jobEvents:          make([]jobEvent, 0),
		activeRunnerCount:  atomic.Int64{},
		wakeCh:             make(chan struct{}, 1),
	}, nil
}

// OxideClient is the behavior from [oxide.Client] that [Scaler] uses,
// extracted into this interface for testing.
type OxideClient interface {
	DiskDelete(ctx context.Context, params oxide.DiskDeleteParams) error
	DiskListAllPages(
		ctx context.Context,
		params oxide.DiskListParams,
	) ([]oxide.Disk, error)
	ImageView(
		ctx context.Context,
		params oxide.ImageViewParams,
	) (*oxide.Image, error)
	InstanceCreate(
		ctx context.Context,
		params oxide.InstanceCreateParams,
	) (*oxide.Instance, error)
	InstanceDelete(
		ctx context.Context,
		params oxide.InstanceDeleteParams,
	) error
	InstanceListAllPages(
		ctx context.Context,
		params oxide.InstanceListParams,
	) ([]oxide.Instance, error)
	InstanceStop(
		ctx context.Context,
		params oxide.InstanceStopParams,
	) (*oxide.Instance, error)
}

// ScaleSetClient is the behavior from [scaleset.Client] that [Scaler] uses,
// extracted into this interface for testing.
type ScaleSetClient interface {
	GenerateJitRunnerConfig(
		ctx context.Context,
		settings *scaleset.RunnerScaleSetJitRunnerSetting,
		scaleSetID int,
	) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(
		ctx context.Context,
		name string,
	) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// namePrefix generates a prefix that [Scaler] uses for all the resources it
// creates in order to keep track of which resources it created across restarts.
func (s *Scaler) namePrefix() string {
	digest := sha256.Sum256([]byte(
		s.scaleSet.Namespace + "\x00" + strconv.Itoa(s.scaleSet.ID),
	))
	hash := hex.EncodeToString(digest[:namePrefixHashBytes])
	return "gha-runner-" + hash + "-"
}

// newRunnerName generates a unique runner name carrying the prefix from
// [Scaler.namePrefix].
func (s *Scaler) newRunnerName() string {
	suffix := make([]byte, 8)
	rand.Read(suffix)
	return s.namePrefix() + hex.EncodeToString(suffix)
}

const (
	// namePrefixHashBytes is the number of bytes to use from the hash generated
	// from [ScaleSetConfig.Namespace].
	namePrefixHashBytes = 12
)
