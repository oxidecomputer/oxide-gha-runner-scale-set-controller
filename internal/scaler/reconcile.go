// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oxidecomputer/oxide.go/oxide"
)

const (
	// reconcileInterval is the maximum amount of time to wait between
	// reconciliations when no other event has triggered reconciliation.
	reconcileInterval = 5 * time.Minute

	// reconcileRetryDelay controls how soon reconciliation is retried when work
	// remains or an operation fails.
	reconcileRetryDelay = 15 * time.Second

	// reconcileTimeout limits how long one reconciliation runs for, including its
	// Oxide and GitHub API operations.
	reconcileTimeout = 5 * time.Minute

	// githubAuditInterval limits how often GitHub runner registrations
	// are reconciled against Oxide instances.
	githubAuditInterval = 10 * time.Minute
)

// reconcileResult holds the earliest deadline requested by work performed
// during one reconciliation. Deadlines are absolute so that time spent in the
// rest of the reconciliation does not delay them.
type reconcileResult struct {
	nextReconcile time.Time
}

func (r *reconcileResult) requestReconcile(at time.Time) {
	if at.IsZero() {
		return
	}
	if r.nextReconcile.IsZero() || at.Before(r.nextReconcile) {
		r.nextReconcile = at
	}
}

func nextReconcileAfter(delay time.Duration) time.Time {
	return time.Now().Add(delay)
}

// ErrScaleSetDrained is returned by [Scaler.Run] when both [Config.MinRunners]
// and [Config.MaxRunners] are zero and all resources owned by the scale set
// have been removed.
var ErrScaleSetDrained = errors.New("scale set drained")

// Run is the blocking loop that periodically reconciles all the resources
// managed by [Scaler]. It provisions new runners to meet demand and tears down
// runners when they are no longer needed.
//
// Each reconciliation runs in three steps:
//
//  1. Gather: Rebuild the in-memory state by querying GitHub runners,
//     Oxide instances and disks, and joining them by name.
//
//  2. Plan: Mark runners whose GitHub runners, Oxide resources, or demand
//     require teardown.
//
//  3. Act: Drive marked teardowns forward and provision new runners to meet
//     demand, never exceeding [Config.MaxRunners].
//
// Run can be called before or after starting a listener to respond to GitHub
// scale set messages.
func (s *Scaler) Run(ctx context.Context) error {
	state := newReconcileState()

	if err := ctx.Err(); err != nil {
		return err
	}

	// Run initial reconcile.
	result, err := s.runReconcile(ctx, state, reconcileReasonInitial)
	if err != nil {
		return err
	}

	interval := time.NewTicker(reconcileInterval)
	defer interval.Stop()

	for {
		var requeueCh <-chan time.Time
		if !result.nextReconcile.IsZero() {
			requeueCh = time.After(max(time.Until(result.nextReconcile), 0))
		}

		var reason reconcileReason
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wakeCh:
			reason = reconcileReasonWake
		case <-requeueCh:
			reason = reconcileReasonRetry
		case <-interval.C:
			reason = reconcileReasonInterval
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		result, err = s.runReconcile(ctx, state, reason)
		if err != nil {
			return err
		}
		interval.Reset(reconcileInterval)
	}
}

// reconcileReason is the reason a reconciliation was started.
type reconcileReason string

const (
	reconcileReasonInitial  reconcileReason = "initial"
	reconcileReasonWake     reconcileReason = "wake"
	reconcileReasonRetry    reconcileReason = "retry"
	reconcileReasonInterval reconcileReason = "interval"
)

// runReconcile is a wrapper around [Scaler.reconcile] used to bound a
// reconciliation with a context.
func (s *Scaler) runReconcile(
	ctx context.Context,
	state *reconcileState,
	reason reconcileReason,
) (reconcileResult, error) {
	reconcileCtx, cancelReconcile := context.WithTimeout(
		ctx, reconcileTimeout,
	)
	defer cancelReconcile()

	return s.reconcile(reconcileCtx, state, reason)
}

// reconcile runs one gather, plan, act cycle over the unified runner view. All
// of the resources managed by [Scaler] are prefixed with [Scaler.namePrefix],
// allowing the gather step to rebuild the view even after a restart.
func (s *Scaler) reconcile(
	ctx context.Context,
	state *reconcileState,
	reason reconcileReason,
) (reconcileResult, error) {
	var result reconcileResult
	s.processJobEvents(state)

	s.logger.Info("reconcile started",
		"reconcile.reason", reason,
	)
	defer s.logger.Info("reconcile finished",
		"reconcile.reason", reason,
	)

	// Gather: Rebuild the in-memory state.
	if err := s.observeRunners(ctx, state); err != nil {
		s.logger.Error("failed observing runners", "error", err)
		result.requestReconcile(nextReconcileAfter(reconcileRetryDelay))
		return result, nil
	}

	// Plan: Mark runners for teardown.
	s.markInstancesForTeardown(state)
	s.markDisksForTeardown(state)

	// The GitHub audit is bounded by an interval to limit expensive, rate-limited
	// API operations. GitHub automatically removes runner registrations that have
	// been offline for a period of time, so it's not critical to always reconcile.
	if time.Since(state.lastGitHubAudit) >= githubAuditInterval {
		auditRetry := s.markRegistrationsForTeardown(ctx, state)
		result.requestReconcile(auditRetry)
		if auditRetry.IsZero() {
			state.lastGitHubAudit = time.Now()
		}
	}

	// Reclaim runners whose scale-down can still be canceled before the teardown
	// loop below makes those teardowns irreversible.
	target := -1
	if state.desiredRunnerCount >= 0 {
		target = min(state.desiredRunnerCount+s.minRunners, s.maxRunners)
		if active := state.activeRunnerCount(); active < target {
			s.cancelPendingScaleDowns(state, target-active)
		}
	}

	// Act: Drive marked teardowns forward and provision new runners to meet demand.
	for _, runner := range state.runners {
		if runner.teardown == nil {
			continue
		}
		result.requestReconcile(s.teardown(ctx, runner))
	}
	state.prune()

	active := state.activeRunnerCount()
	if target >= 0 {
		switch {
		case active < target:
			result.requestReconcile(s.scaleUp(ctx, state, active, target))
		case active > target:
			result.requestReconcile(s.scaleDown(state, active, target))
		}
	}

	s.activeRunnerCount.Store(int64(state.activeRunnerCount()))

	if s.minRunners == 0 && s.maxRunners == 0 && len(state.runners) == 0 {
		return reconcileResult{}, ErrScaleSetDrained
	}

	if state.teardownCount() > 0 && result.nextReconcile.IsZero() {
		result.requestReconcile(nextReconcileAfter(reconcileRetryDelay))
	}

	return result, nil
}

// observeRunners rebuilds the in-memory state from the Oxide instances and
// disks owned by [Scaler], joined by runner name.
func (s *Scaler) observeRunners(
	ctx context.Context,
	state *reconcileState,
) error {
	instances, err := s.listOwnedInstances(ctx)
	if err != nil {
		return fmt.Errorf("listing owned instances: %w", err)
	}

	disks, err := s.listOwnedDisks(ctx)
	if err != nil {
		return fmt.Errorf("listing owned disks: %w", err)
	}

	for _, runner := range state.runners {
		runner.instance = nil
		runner.disk = nil
	}
	for name, instance := range instances {
		state.runner(name).instance = &instance
	}
	for name, disk := range disks {
		state.runner(name).disk = &disk
	}

	s.logger.Info("observed runners",
		"runner.count", len(state.runners),
		"instance.count", len(instances),
		"disk.count", len(disks),
	)

	return nil
}

// listOwnedInstances fetches all the Oxide instances that are managed by
// [Scaler], returning a map of runner name to Oxide instance.
func (s *Scaler) listOwnedInstances(
	ctx context.Context,
) (map[string]oxide.Instance, error) {
	instances, err := s.oxideClient.InstanceListAllPages(
		ctx,
		oxide.InstanceListParams{
			Project: oxide.NameOrId(s.instanceConfig.Project),
		},
	)
	if err != nil {
		return nil, err
	}

	owned := make(map[string]oxide.Instance)
	for _, instance := range instances {
		name := string(instance.Name)
		if strings.HasPrefix(name, s.namePrefix()) {
			owned[name] = instance
		}
	}

	return owned, nil
}

// listOwnedDisks fetches all the Oxide disks that are managed by [Scaler],
// returning a map of runner name to Oxide disk.
func (s *Scaler) listOwnedDisks(
	ctx context.Context,
) (map[string]oxide.Disk, error) {
	disks, err := s.oxideClient.DiskListAllPages(ctx, oxide.DiskListParams{
		Project: oxide.NameOrId(s.instanceConfig.Project),
	})
	if err != nil {
		return nil, err
	}

	owned := make(map[string]oxide.Disk)
	for _, disk := range disks {
		name := string(disk.Name)
		if strings.HasPrefix(name, s.namePrefix()) {
			owned[name] = disk
		}
	}

	return owned, nil
}
