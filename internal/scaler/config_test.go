// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"math"
	"strings"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	tooManyGiB := uint64(maxByteCountGiB) + 1
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "valid",
			mutate:  func(*Config) {},
			wantErr: "",
		},
		{
			name: "minimum above maximum",
			mutate: func(c *Config) {
				c.MinRunners = 3
				c.MaxRunners = 2
			},
			wantErr: "minimum runners must be <= maximum runners",
		},
		{
			name: "negative minimum runners",
			mutate: func(c *Config) {
				c.MinRunners = -1
			},
			wantErr: "minimum runners must be >= 0",
		},
		{
			name: "negative maximum runners",
			mutate: func(c *Config) {
				c.MaxRunners = -1
			},
			wantErr: "maximum runners must be >= 0",
		},
		{
			name: "maximum runners above int32",
			mutate: func(c *Config) {
				c.MaxRunners = math.MaxInt32 + 1
			},
			wantErr: "maximum runners must be <=",
		},
		{
			name: "missing namespace",
			mutate: func(c *Config) {
				c.ScaleSet.Namespace = ""
			},
			wantErr: "scale set namespace is required",
		},
		{
			name: "invalid scale set ID",
			mutate: func(c *Config) {
				c.ScaleSet.ID = 0
			},
			wantErr: "scale set ID must be > 0",
		},
		{
			name: "missing project",
			mutate: func(c *Config) {
				c.Instance.Project = ""
			},
			wantErr: "instance: project is required",
		},
		{
			name: "missing image",
			mutate: func(c *Config) {
				c.Instance.Image = ""
			},
			wantErr: "instance: image is required",
		},
		{
			name: "zero CPUs",
			mutate: func(c *Config) {
				c.Instance.CPUs = 0
			},
			wantErr: "instance: CPUs must be > 0",
		},
		{
			name: "CPUs overflow API type",
			mutate: func(c *Config) {
				c.Instance.CPUs = math.MaxUint16 + 1
			},
			wantErr: "instance: CPUs must be <= 65535",
		},
		{
			name: "zero memory",
			mutate: func(c *Config) {
				c.Instance.MemoryGiB = 0
			},
			wantErr: "instance: memory GiB must be > 0",
		},
		{
			name: "memory overflows byte count",
			mutate: func(c *Config) {
				c.Instance.MemoryGiB = uint(tooManyGiB)
			},
			wantErr: "instance: memory GiB must be <= 17179869183",
		},
		{
			name: "boot disk overflows byte count",
			mutate: func(c *Config) {
				c.Instance.BootDiskGiB = uint(tooManyGiB)
			},
			wantErr: "instance: boot disk GiB must be <= 17179869183",
		},
		{
			name: "missing VPC",
			mutate: func(c *Config) {
				c.Instance.VPC = ""
			},
			wantErr: "instance: VPC is required",
		},
		{
			name: "missing subnet",
			mutate: func(c *Config) {
				c.Instance.Subnet = ""
			},
			wantErr: "instance: subnet is required",
		},
		{
			name: "missing runner version",
			mutate: func(c *Config) {
				c.Runner.Version = ""
			},
			wantErr: "runner: version is required",
		},
		{
			name: "invalid runner version",
			mutate: func(c *Config) {
				c.Runner.Version = "2.335.1; reboot"
			},
			wantErr: "runner: version must use X.Y.Z format",
		},
		{
			name: "runner version missing component",
			mutate: func(c *Config) {
				c.Runner.Version = "2.335"
			},
			wantErr: "runner: version must use X.Y.Z format",
		},
		{
			name: "missing runner SHA-256",
			mutate: func(c *Config) {
				c.Runner.SHA256 = ""
			},
			wantErr: "runner: SHA-256 is required",
		},
		{
			name: "short runner SHA-256",
			mutate: func(c *Config) {
				c.Runner.SHA256 = "abc123"
			},
			wantErr: "runner: SHA-256 must be a 64-character " +
				"hexadecimal checksum",
		},
		{
			name: "non-hexadecimal runner SHA-256",
			mutate: func(c *Config) {
				c.Runner.SHA256 = strings.Repeat("z", 64)
			},
			wantErr: "runner: SHA-256 must be a 64-character " +
				"hexadecimal checksum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testConfig()
			tt.mutate(&config)

			err := config.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v",
					tt.wantErr, err)
			}
		})
	}
}
