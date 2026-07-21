// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package scaler provisions ephemeral GitHub Actions runners on Oxide.
//
// A [Scaler] implements the callbacks expected by a GitHub Actions scale set
// listener. The callbacks record desired capacity and job events, while
// [Scaler.Run] reconciles those inputs with GitHub runner registrations and
// Oxide instances and disks. Each provisioned runner handles one job and is
// then removed.
//
// Create a scaler with GitHub and Oxide clients and the desired capacity:
//
//	s, err := scaler.New(oxideClient, scaleSetClient, scaler.Config{
//		ScaleSet: scaler.ScaleSetConfig{
//			Namespace: "https://github.com/example/acme",
//			ID:        scaleSetID,
//		},
//		Runner: scaler.RunnerConfig{
//			Version: "2.325.0",
//			SHA256:  runnerSHA256,
//		},
//		Instance: scaler.InstanceConfig{
//			Project:   "ci",
//			Image:     "github-runner",
//			CPUs:      4,
//			MemoryGiB: 16,
//			VPC:       "default",
//			Subnet:    "default",
//		},
//		MinRunners: 0,
//		MaxRunners: 10,
//	})
//	if err != nil {
//		return err
//	}
//
// Run the reconciliation loop and pass the scaler to the scale set listener:
//
//	go func() {
//		if err := s.Run(ctx); err != nil &&
//			!errors.Is(err, context.Canceled) {
//			log.Printf("scaler stopped: %v", err)
//		}
//	}()
//
//	if err := scaleSetListener.Run(ctx, s); err != nil {
//		return err
//	}
//
// [ScaleSetConfig.Namespace] must remain stable across restarts so the scaler
// can recover resources it previously created. Setting both capacity limits
// to zero drains those resources. [Scaler.Run] returns [ErrScaleSetDrained]
// when the drain is complete.
package scaler
