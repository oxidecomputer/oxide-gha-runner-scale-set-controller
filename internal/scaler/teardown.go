// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide.go/oxide"
)

const (
	// resourceGracePeriod protects newly created instances and disks from teardown
	// while they initialize.
	resourceGracePeriod = 5 * time.Minute

	// provisioningTimeout limits how long an instance may remain in creating or
	// starting before it is torn down.
	provisioningTimeout = 10 * time.Minute

	// scaleDownCancellationDelay gives an assigned job event time to cancel a scale
	// down before runner registration removal makes it irreversible.
	scaleDownCancellationDelay = 5 * time.Second

	// teardownPollDelay controls how quickly asynchronous Oxide resource state
	// transitions are observed during teardown.
	teardownPollDelay = 5 * time.Second
)

// teardown drives one runner's teardown forward as far as it can go without
// blocking, returning the deadline at which it should be driven again. A zero
// return means no further work is scheduled and the teardown either finished or
// was canceled.
//
// The teardown order is fixed. Remove the GitHub registration so no new job
// can land on the runner, then stop and delete the instance, then delete the
// boot disk.
func (s *Scaler) teardown(
	ctx context.Context,
	runner *runner,
) time.Time {
	logger := s.logger.With(
		"runner.name", runner.name,
	)

	teardown := runner.teardown
	if teardown == nil {
		return time.Time{}
	}

	if teardown.phase == teardownPhaseCancellationWindow {
		if time.Now().Before(teardown.registrationRemovalAfter) {
			return teardown.registrationRemovalAfter
		}
		teardown.beginRemovingRegistration()
	}

	if teardown.phase == teardownPhaseRemovingRegistration {
		if err := s.removeRunner(ctx, runner.name); err != nil {
			if errors.Is(err, scaleset.JobStillRunningError) {
				runner.busy = true
				if teardown.policy == teardownPolicyCancelable {
					runner.teardown = nil
					logger.Info(
						"scale down canceled; runner is running a job",
					)
					return time.Time{}
				}

				logger.Info("runner is still running a job; waiting")
				return nextReconcileAfter(reconcileRetryDelay)
			}

			logger.Error("removing runner registration failed; will retry",
				"error", err,
			)
			return nextReconcileAfter(reconcileRetryDelay)
		}

		teardown.beginDeprovisioning()
	}

	instanceDeleted := false
	if instance := runner.instance; instance != nil {
		switch instance.RunState {
		case oxide.InstanceStateStopped, oxide.InstanceStateFailed:
			if err := s.deleteInstance(ctx, runner.name); err != nil {
				logger.Error("failed deleting instance; will retry",
					"error", err,
				)
				return nextReconcileAfter(reconcileRetryDelay)
			}
			runner.instance = nil
			instanceDeleted = true
			logger.Info("deleted instance")
		case oxide.InstanceStateDestroyed:
			// A destroyed instance no longer exists, but this observation may be stale, so
			// it does not authorize deleting a disk that still looks attached below.
			runner.instance = nil
		case oxide.InstanceStateStopping:
			logger.Info("waiting for instance state transition",
				"instance.state", instance.RunState,
			)
			return nextReconcileAfter(teardownPollDelay)
		default:
			if err := s.stopInstance(ctx, runner.name); err != nil {
				logger.Error("failed stopping instance; will retry",
					"error", err,
				)
				return nextReconcileAfter(reconcileRetryDelay)
			}
			return nextReconcileAfter(teardownPollDelay)
		}
	}

	if disk := runner.disk; disk != nil {
		diskState := disk.State.State()
		// Instance deletion detaches its disks atomically, so an attached state from
		// the pre-deletion snapshot no longer blocks disk deletion.
		diskDeletable := instanceDeleted ||
			diskState == oxide.DiskStateStateDetached ||
			diskState == oxide.DiskStateStateFaulted
		if !diskDeletable {
			logger.Info("waiting for boot disk to detach",
				"disk.state", diskState,
			)
			return nextReconcileAfter(teardownPollDelay)
		}

		if err := s.deleteDisk(ctx, runner.name); err != nil {
			logger.Error("deleting boot disk failed; will retry",
				"error", err,
			)
			return nextReconcileAfter(reconcileRetryDelay)
		}
		runner.disk = nil
		logger.Info("deleted boot disk")
	}

	runner.teardown = nil
	logger.Info("runner teardown complete")
	return time.Time{}
}

// markForTeardown records a runner for teardown. A permanent teardown
// supersedes a cancelable teardown. An existing teardown is otherwise left
// untouched. The reason is only logged.
func (s *Scaler) markForTeardown(
	runner *runner,
	policy teardownPolicy,
	reason string,
) {
	if teardown := runner.teardown; teardown != nil {
		// Existing teardown policies may only be changed by an upgrade.
		isUpgrade := teardown.policy == teardownPolicyCancelable &&
			policy == teardownPolicyPermanent
		if !isUpgrade {
			return
		}

		teardown.policy = policy
		if teardown.phase == teardownPhaseCancellationWindow {
			teardown.beginRemovingRegistration()
		}
	} else {
		teardown := &teardownState{policy: policy}
		if policy == teardownPolicyCancelable {
			teardown.phase = teardownPhaseCancellationWindow
			teardown.registrationRemovalAfter = time.Now().Add(
				scaleDownCancellationDelay,
			)
		} else {
			teardown.phase = teardownPhaseRemovingRegistration
		}
		runner.teardown = teardown
	}

	s.logger.Info("runner marked for teardown",
		"runner.name", runner.name,
		"teardown.reason", reason,
		"teardown.policy", policy.String(),
	)
}

// markInstancesForTeardown marks runners whose instances have halted or have
// been stuck provisioning for too long.
func (s *Scaler) markInstancesForTeardown(state *reconcileState) {
	for _, runner := range state.runners {
		if runner.instance == nil || runner.teardown != nil {
			continue
		}

		// Still within grace period.
		if !s.pastGracePeriod(runner.instance.TimeCreated) {
			continue
		}

		if instanceHalted(runner.instance.RunState) {
			s.markForTeardown(
				runner, teardownPolicyPermanent, "instance halted",
			)
			continue
		}

		if s.provisioningTimedOut(runner.instance) {
			s.markForTeardown(
				runner,
				teardownPolicyPermanent,
				"instance provisioning timed out",
			)
		}
	}
}

// markDisksForTeardown marks runners that have a boot disk but no instance,
// which happens when a previous teardown or failed provisioning left the disk
// behind.
func (s *Scaler) markDisksForTeardown(state *reconcileState) {
	for _, runner := range state.runners {
		if runner.disk == nil || runner.instance != nil ||
			runner.teardown != nil {
			continue
		}

		// Still within grace period.
		if !s.pastGracePeriod(runner.disk.TimeCreated) {
			continue
		}

		s.markForTeardown(
			runner, teardownPolicyPermanent, "orphaned boot disk",
		)
	}
}

// markRegistrationsForTeardown marks runners whose GitHub registration is
// gone. Their instances will never receive work, so they are torn down with the
// registration removal step already confirmed.
func (s *Scaler) markRegistrationsForTeardown(
	ctx context.Context,
	state *reconcileState,
) time.Time {
	retry := false
	for _, runner := range state.runners {
		if runner.instance == nil || runner.teardown != nil {
			continue
		}

		// Still within grace period.
		if !s.pastGracePeriod(runner.instance.TimeCreated) {
			continue
		}

		// Only check running instances for a missing registration.
		if instanceHalted(runner.instance.RunState) {
			continue
		}

		registration, err := s.scalesetClient.GetRunnerByName(
			ctx, runner.name,
		)
		if err != nil {
			s.logger.Error("failed fetching runner from github; will retry",
				"runner.name", runner.name,
				"error", err,
			)
			retry = true
			continue
		}

		switch {
		case registration == nil:
			s.markForTeardown(
				runner,
				teardownPolicyPermanent,
				"runner registration gone",
			)
			runner.teardown.beginDeprovisioning()
		case registration.RunnerScaleSetID != s.scaleSet.ID:
			s.logger.Warn("runner belongs to another scale set",
				"runner.name", runner.name,
				"runner.scale_set.id", registration.RunnerScaleSetID,
			)
		}
	}

	if retry {
		return nextReconcileAfter(reconcileRetryDelay)
	}

	return time.Time{}
}

func (s *Scaler) removeRunner(ctx context.Context, name string) error {
	registration, err := s.scalesetClient.GetRunnerByName(ctx, name)
	if err != nil {
		return err
	}
	if registration == nil {
		return nil
	}

	if registration.RunnerScaleSetID != s.scaleSet.ID {
		return fmt.Errorf(
			"runner belongs to scale set %d, expected %d",
			registration.RunnerScaleSetID,
			s.scaleSet.ID,
		)
	}

	return s.scalesetClient.RemoveRunner(ctx, int64(registration.ID))
}

func (s *Scaler) stopInstance(ctx context.Context, name string) error {
	_, err := s.oxideClient.InstanceStop(ctx, oxide.InstanceStopParams{
		Project:  oxide.NameOrId(s.instanceConfig.Project),
		Instance: oxide.NameOrId(name),
	})
	if errors.Is(err, oxide.ErrObjectNotFound) {
		return nil
	}
	return err
}

func (s *Scaler) deleteInstance(ctx context.Context, name string) error {
	err := s.oxideClient.InstanceDelete(ctx, oxide.InstanceDeleteParams{
		Project:  oxide.NameOrId(s.instanceConfig.Project),
		Instance: oxide.NameOrId(name),
	})
	if errors.Is(err, oxide.ErrObjectNotFound) {
		return nil
	}
	return err
}

func (s *Scaler) deleteDisk(ctx context.Context, name string) error {
	err := s.oxideClient.DiskDelete(ctx, oxide.DiskDeleteParams{
		Project: oxide.NameOrId(s.instanceConfig.Project),
		Disk:    oxide.NameOrId(name),
	})
	if errors.Is(err, oxide.ErrObjectNotFound) {
		return nil
	}
	return err
}

func (s *Scaler) pastGracePeriod(created *time.Time) bool {
	return created == nil || time.Since(*created) >= resourceGracePeriod
}

func (s *Scaler) provisioningTimedOut(instance *oxide.Instance) bool {
	switch instance.RunState {
	case oxide.InstanceStateCreating, oxide.InstanceStateStarting:
	default:
		return false
	}

	since := instance.TimeRunStateUpdated
	if since == nil {
		since = instance.TimeCreated
	}
	return since == nil || time.Since(*since) >= provisioningTimeout
}

// teardownPolicy is the policy to follow when tearing down a runner.
type teardownPolicy int

const (
	teardownPolicyPermanent teardownPolicy = iota
	teardownPolicyCancelable
)

// String implements [fmt.Stringer].
func (p teardownPolicy) String() string {
	switch p {
	case teardownPolicyPermanent:
		return "permanent"
	case teardownPolicyCancelable:
		return "cancelable"
	default:
		return "unknown"
	}
}

// teardownPhase is the externally confirmed progress of a teardown.
type teardownPhase int

const (
	// teardownPhaseCancellationWindow means registration removal has not started,
	// so the teardown may still be canceled for capacity.
	teardownPhaseCancellationWindow teardownPhase = iota

	// teardownPhaseRemovingRegistration means registration removal is eligible or
	// has been attempted. The outcome of a failed attempt is ambiguous, so capacity
	// may no longer cancel the teardown. Only an authoritative job-started signal
	// from GitHub can.
	teardownPhaseRemovingRegistration

	// teardownPhaseDeprovisioning means the registration is known to be gone and
	// Oxide resource teardown must run to completion.
	teardownPhaseDeprovisioning
)

// teardownState tracks one runner's progress through teardown, separating
// intent (policy), externally confirmed progress (phase), and the earliest time
// at which registration removal may proceed.
type teardownState struct {
	policy                   teardownPolicy
	phase                    teardownPhase
	registrationRemovalAfter time.Time
}

// canCancelForCapacity reports whether rising demand may reclaim this runner
// instead of tearing it down. Only teardowns still inside their cancellation
// window qualify. Once registration removal has been attempted, its outcome is
// ambiguous and the runner may never receive work again. A nil teardown is not
// cancelable because there is nothing to cancel.
func (t *teardownState) canCancelForCapacity(now time.Time) bool {
	return t != nil &&
		t.policy == teardownPolicyCancelable &&
		t.phase == teardownPhaseCancellationWindow &&
		now.Before(t.registrationRemovalAfter)
}

// canCancelForJob reports whether a job started signal from GitHub may cancel
// this teardown. GitHub is authoritative that the registration still exists, so
// cancellation is safe until the registration is confirmed gone. A nil teardown
// is not cancelable because there is nothing to cancel.
func (t *teardownState) canCancelForJob() bool {
	return t != nil &&
		t.policy == teardownPolicyCancelable &&
		t.phase != teardownPhaseDeprovisioning
}

// beginRemovingRegistration records that the cancellation window has closed.
// Registration removal may start and capacity can no longer cancel the
// teardown.
func (t *teardownState) beginRemovingRegistration() {
	t.phase = teardownPhaseRemovingRegistration
	t.registrationRemovalAfter = time.Time{}
}

// beginDeprovisioning records that the runner's GitHub registration is gone.
// The teardown is now irreversible and immediately actionable.
func (t *teardownState) beginDeprovisioning() {
	t.phase = teardownPhaseDeprovisioning
	t.registrationRemovalAfter = time.Time{}
}
