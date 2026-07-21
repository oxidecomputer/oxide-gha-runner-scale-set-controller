// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"fmt"
	"log/slog"
	"math"
)

const maxByteCountGiB = math.MaxUint64 / (1024 * 1024 * 1024)

// Config holds information to configure a [Scaler].
type Config struct {
	// Logger configures the logger for the [Scaler]. Leave unset to skip logging.
	Logger *slog.Logger

	// ScaleSet configures the scale set runners are created within.
	ScaleSet ScaleSetConfig

	// Instance configures the Oxide instance that's launched.
	Instance InstanceConfig

	// MinRunners is the minimum number of GitHub Actions runners to keep running
	// at any given time. This must be less than or equal to [MaxRunners]. Set both
	// this and [MaxRunners] to zero to drain all configured runners.
	MinRunners int

	// MaxRunners is the maximum number of GitHub Actions runners that will be
	// created, including runners that have errored or completed their job but
	// have not yet been torn down. This allows users to set a hard limit to manage
	// resource capacity. This must be greater than or equal to [MinRunners]. Set
	// both this and [MinRunners] to zero to drain all configured runners.
	MaxRunners int
}

// Validate validates [Config].
func (c Config) Validate() error {
	if c.ScaleSet.Namespace == "" {
		return fmt.Errorf("scale set namespace is required")
	}
	if c.ScaleSet.ID <= 0 {
		return fmt.Errorf("scale set ID must be > 0")
	}
	if c.MinRunners < 0 {
		return fmt.Errorf("minimum runners must be >= 0")
	}
	if c.MaxRunners < 0 {
		return fmt.Errorf("maximum runners must be >= 0")
	}
	if c.MinRunners > c.MaxRunners {
		return fmt.Errorf("minimum runners must be <= maximum runners")
	}
	if c.MaxRunners > math.MaxInt32 {
		return fmt.Errorf("maximum runners must be <= %d", math.MaxInt32)
	}
	if err := c.Instance.Validate(); err != nil {
		return fmt.Errorf("instance: %w", err)
	}
	return nil
}

// ScaleSetConfig configures the scale set runners are created within
type ScaleSetConfig struct {
	// Namespace is used to uniquely name resources so that [Scaler] can fetch
	// resources it manages across restarts. This value must not change across
	// [Scaler] restarts or it will orphan resources created under the previous
	// configuration. Setting this to the same value across [Scaler] instances that
	// target the same Oxide silo and project will cause both instances to co-own
	// resources and interfere with one another.
	Namespace string

	// ID configures which scale set ID to create runners within.
	ID int
}

// InstanceConfig configures the Oxide instances created by a Scaler.
type InstanceConfig struct {
	// Project is the Oxide project to create the instance in.
	Project string

	// Image is the image used as the instance's boot disk.
	Image string

	// BootDiskGiB is the size of the instance's boot disk in GiB. Set to
	// zero to dynamically calculate the boot disk size based on the size of
	// [InstanceConfig.Image].
	BootDiskGiB uint

	// CPUs are the number of CPUs to allocate to the instance.
	CPUs uint

	// MemoryGiB is the amount of memory to allocate to the instance in GiB.
	MemoryGiB uint

	// VPC is the name of the VPC the instance's network interface uses.
	VPC string

	// Subnet is the name of the VPC subnet the instance's network interface uses.
	Subnet string
}

// Validate validates the Oxide instance configuration.
func (c InstanceConfig) Validate() error {
	switch {
	case c.Project == "":
		return fmt.Errorf("project is required")
	case c.Image == "":
		return fmt.Errorf("image is required")
	case uint64(c.BootDiskGiB) > maxByteCountGiB:
		return fmt.Errorf("boot disk GiB must be <= %d", maxByteCountGiB)
	case c.CPUs == 0:
		return fmt.Errorf("CPUs must be > 0")
	case c.CPUs > math.MaxUint16:
		return fmt.Errorf("CPUs must be <= %d", math.MaxUint16)
	case c.MemoryGiB == 0:
		return fmt.Errorf("memory GiB must be > 0")
	case uint64(c.MemoryGiB) > maxByteCountGiB:
		return fmt.Errorf("memory GiB must be <= %d", maxByteCountGiB)
	case c.VPC == "":
		return fmt.Errorf("VPC is required")
	case c.Subnet == "":
		return fmt.Errorf("subnet is required")
	}
	return nil
}
