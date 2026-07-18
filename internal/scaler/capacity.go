// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"context"
	"slices"
	"time"
)

// scaleUp provisions new runners to raise the active runner count toward
// target, never exceeding [Config.MaxRunners] worth of Oxide instances. It can
// reclaim runners whose teardown can still be cancelled. It returns a time at
// which another reconciliation should run.
func (s *Scaler) scaleUp(
	ctx context.Context,
	state *reconcileState,
	active int,
	target int,
) time.Time {
	// Every owned instance counts against capacity, including halted instances
	// awaiting teardown.
	available := max(s.maxRunners-state.instanceCount(), 0)
	requested := min(target-active, available)
	if requested <= 0 {
		return time.Time{}
	}

	image, err := s.fetchImage(ctx)
	if err != nil {
		s.logger.Error("fetching image failed",
			"image", s.instanceConfig.Image,
			"error", err,
		)
		return nextReconcileAfter(reconcileRetryDelay)
	}

	created := s.provisionRunners(ctx, state, image, requested)
	for _, name := range created {
		runner := state.runner(name)
		s.logger.Info("runner instance created",
			"runner.name", runner.name,
			"instance.id", runner.instance.Id,
			"instance.state", runner.instance.RunState,
		)
	}

	if len(created) < requested {
		return nextReconcileAfter(reconcileRetryDelay)
	}

	return time.Time{}
}

// scaleDown marks idle runners for cancelable teardown until the active count
// converges on target, returning the earliest cancellation window deadline so
// registration removal starts on time. Busy runners, runners already being torn
// down, and runners still within their grace period are never candidates.
func (s *Scaler) scaleDown(
	state *reconcileState,
	active int,
	target int,
) time.Time {
	candidates := make([]*runner, 0, len(state.runners))
	for _, runner := range state.runners {
		if runner.instance == nil || runner.busy ||
			runner.teardown != nil ||
			instanceHalted(runner.instance.RunState) ||
			!s.pastGracePeriod(runner.instance.TimeCreated) {
			continue
		}

		candidates = append(candidates, runner)
	}

	// Oldest first, which bounds the maximum lifetime of idle runners.
	slices.SortFunc(candidates, func(a, b *runner) int {
		if a.instance.TimeCreated == nil || b.instance.TimeCreated == nil {
			return 0
		}
		return a.instance.TimeCreated.Compare(*b.instance.TimeCreated)
	})

	requested := active - target
	selected := min(requested, len(candidates))
	var nextReconcile time.Time
	for _, runner := range candidates[:selected] {
		s.markForTeardown(runner, teardownPolicyCancelable, "scale down")
		deadline := runner.teardown.registrationRemovalAfter
		if nextReconcile.IsZero() || deadline.Before(nextReconcile) {
			nextReconcile = deadline
		}
	}

	return nextReconcile
}

// cancelPendingScaleDowns reclaims up to requested runners whose cancelable
// teardown is still inside its cancellation window. It runs before teardowns
// are driven forward so that rising demand reclaims warm runners instead of
// tearing them down and provisioning cold replacements.
func (s *Scaler) cancelPendingScaleDowns(
	state *reconcileState,
	requested int,
) {
	now := time.Now()
	for _, runner := range state.runners {
		if requested <= 0 {
			return
		}
		if runner.instance == nil ||
			instanceHalted(runner.instance.RunState) ||
			!runner.teardown.canCancelForCapacity(now) {
			continue
		}

		runner.teardown = nil
		requested--
		s.logger.Info("scale down canceled; runner needed for target capacity",
			"runner.name", runner.name,
		)
	}
}
