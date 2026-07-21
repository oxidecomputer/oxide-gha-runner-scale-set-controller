// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide.go/oxide"
)

// Invariant: teardown removes the GitHub registration exactly once, then
// stops the instance, deletes it, and deletes the boot disk, in order.

func TestTeardownLifecycle(t *testing.T) {
	scaler, oxideClient, scaleSetClient := newTestScaler(t)
	const name = "lifecycle"
	scaleSetClient.registrations[name] = &scaleset.RunnerReference{
		ID:               123,
		RunnerScaleSetID: scaler.scaleSet.ID,
	}
	state := newReconcileState()
	runner := state.runner(name)
	runner.instance = &oxide.Instance{
		RunState: oxide.InstanceStateRunning,
	}
	runner.disk = &oxide.Disk{
		State: oxide.DiskState{Value: oxide.DiskStateAttached{}},
	}
	runner.teardown = &teardownState{
		policy: teardownPolicyPermanent,
		phase:  teardownPhaseRemovingRegistration,
	}
	ctx := context.Background()

	// Running: remove the registration, then stop the instance.
	before := time.Now()
	expectDeadlineIn(t, before, scaler.teardown(ctx, runner),
		teardownPollDelay)
	if len(scaleSetClient.removedIDs) != 1 {
		t.Fatal("expected registration removal")
	}
	if len(oxideClient.stopCalls) != 1 {
		t.Fatal("expected instance stop")
	}

	// Stopping: wait, and do not remove the registration again.
	runner.instance.RunState = oxide.InstanceStateStopping
	before = time.Now()
	expectDeadlineIn(t, before, scaler.teardown(ctx, runner),
		teardownPollDelay)
	if len(scaleSetClient.getCalls) != 1 {
		t.Fatalf("expected registration removal exactly once, got %d lookups",
			len(scaleSetClient.getCalls))
	}

	// Stopped: delete the instance, then the disk in the same pass since
	// instance deletion detaches disks atomically.
	runner.instance.RunState = oxide.InstanceStateStopped
	expectNoDeadline(t, scaler.teardown(ctx, runner))
	if !runner.gone() {
		t.Fatal("expected nothing to remain of the runner")
	}

	// The full teardown performed exactly these operations, in order.
	wantOps := []string{
		"remove-runner",
		"stop-instance",
		"delete-instance",
		"delete-disk",
	}
	if got := oxideClient.ops.snapshot(); !slices.Equal(got, wantOps) {
		t.Fatalf("expected teardown operations %v, got %v", wantOps, got)
	}
}

// Invariant: a failed teardown step schedules a retry and blocks every
// later step, so a runner is never destroyed out of order.

func TestTeardownStepFailureBlocksLaterSteps(t *testing.T) {
	tests := []struct {
		name string
		// setup injects the failure into the step under test.
		setup func(*fakeOxide, *fakeScaleSet)
		// instanceState selects the teardown step that runs.
		instanceState oxide.InstanceState
		// wantOps are the operations attempted before the retry,
		// including the failing one.
		wantOps []string
	}{
		{
			name: "registration removal failure blocks stop",
			setup: func(_ *fakeOxide, client *fakeScaleSet) {
				client.removeErr = errors.New("github unavailable")
			},
			instanceState: oxide.InstanceStateRunning,
			wantOps:       []string{"remove-runner"},
		},
		{
			name: "stop failure blocks deletion",
			setup: func(client *fakeOxide, _ *fakeScaleSet) {
				client.stopErr = errors.New("oxide unavailable")
			},
			instanceState: oxide.InstanceStateRunning,
			wantOps:       []string{"remove-runner", "stop-instance"},
		},
		{
			name: "instance deletion failure blocks disk deletion",
			setup: func(client *fakeOxide, _ *fakeScaleSet) {
				client.deleteInstanceErr = errors.New("oxide unavailable")
			},
			instanceState: oxide.InstanceStateStopped,
			wantOps:       []string{"remove-runner", "delete-instance"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, oxideClient, scaleSetClient := newTestScaler(t)
			const name = "failing"
			scaleSetClient.registrations[name] = &scaleset.RunnerReference{
				ID:               123,
				RunnerScaleSetID: scaler.scaleSet.ID,
			}
			tt.setup(oxideClient, scaleSetClient)
			state := newReconcileState()
			runner := state.runner(name)
			runner.instance = &oxide.Instance{
				RunState: tt.instanceState,
			}
			runner.disk = &oxide.Disk{
				State: oxide.DiskState{Value: oxide.DiskStateAttached{}},
			}
			runner.teardown = &teardownState{
				policy: teardownPolicyPermanent,
				phase:  teardownPhaseRemovingRegistration,
			}

			before := time.Now()
			deadline := scaler.teardown(context.Background(), runner)
			expectDeadlineIn(t, before, deadline, reconcileRetryDelay)
			if got := oxideClient.ops.snapshot(); !slices.Equal(
				got, tt.wantOps,
			) {
				t.Fatalf("expected operations %v, got %v", tt.wantOps, got)
			}
			if runner.teardown == nil {
				t.Fatal("expected teardown to remain for the retry")
			}
		})
	}
}

func TestTeardownWaitsForCancellationWindow(t *testing.T) {
	scaler, oxideClient, scaleSetClient := newTestScaler(t)
	state := newReconcileState()
	runner := state.runner("waiting")
	runner.instance = &oxide.Instance{
		RunState: oxide.InstanceStateRunning,
	}
	removalAfter := time.Now().Add(time.Minute)
	runner.teardown = &teardownState{
		policy:                   teardownPolicyCancelable,
		phase:                    teardownPhaseCancellationWindow,
		registrationRemovalAfter: removalAfter,
	}

	deadline := scaler.teardown(context.Background(), runner)
	if !deadline.Equal(removalAfter) {
		t.Fatalf("expected the window deadline %s, got %s",
			removalAfter, deadline)
	}
	if len(scaleSetClient.getCalls) != 0 || len(oxideClient.stopCalls) != 0 {
		t.Fatal("expected no teardown operations during the window")
	}
}

func TestTeardownCancelsWhenJobIsStillRunning(t *testing.T) {
	scaler, oxideClient, scaleSetClient := newTestScaler(t)
	const name = "busy"
	scaleSetClient.registrations[name] = &scaleset.RunnerReference{
		ID:               123,
		RunnerScaleSetID: scaler.scaleSet.ID,
	}
	scaleSetClient.removeErr = scaleset.JobStillRunningError
	state := newReconcileState()
	runner := state.runner(name)
	runner.instance = &oxide.Instance{
		RunState: oxide.InstanceStateRunning,
	}
	runner.teardown = &teardownState{
		policy: teardownPolicyCancelable,
		phase:  teardownPhaseRemovingRegistration,
	}

	expectNoDeadline(t, scaler.teardown(context.Background(), runner))
	if runner.teardown != nil {
		t.Fatal("expected busy runner to cancel its scale down")
	}
	if !runner.busy {
		t.Fatal("expected busy to be re-derived from GitHub")
	}
	if len(oxideClient.stopCalls) != 0 {
		t.Fatal("expected instance to keep running")
	}
}

func TestPermanentTeardownWaitsForRunningJob(t *testing.T) {
	scaler, _, scaleSetClient := newTestScaler(t)
	const name = "completing"
	scaleSetClient.registrations[name] = &scaleset.RunnerReference{
		ID:               123,
		RunnerScaleSetID: scaler.scaleSet.ID,
	}
	scaleSetClient.removeErr = scaleset.JobStillRunningError
	state := newReconcileState()
	runner := state.runner(name)
	runner.instance = &oxide.Instance{
		RunState: oxide.InstanceStateRunning,
	}
	runner.teardown = &teardownState{
		policy: teardownPolicyPermanent,
		phase:  teardownPhaseRemovingRegistration,
	}

	before := time.Now()
	deadline := scaler.teardown(context.Background(), runner)
	expectDeadlineIn(t, before, deadline, reconcileRetryDelay)
	if runner.teardown == nil ||
		runner.teardown.phase != teardownPhaseRemovingRegistration {
		t.Fatal("expected teardown to wait until the job finishes")
	}
}

func TestTeardownRefusesForeignRunner(t *testing.T) {
	scaler, oxideClient, scaleSetClient := newTestScaler(t)
	const name = "foreign"
	scaleSetClient.registrations[name] = &scaleset.RunnerReference{
		ID:               123,
		RunnerScaleSetID: 99,
	}
	state := newReconcileState()
	runner := state.runner(name)
	runner.instance = &oxide.Instance{
		RunState: oxide.InstanceStateRunning,
	}
	runner.teardown = &teardownState{
		policy: teardownPolicyPermanent,
		phase:  teardownPhaseRemovingRegistration,
	}

	before := time.Now()
	deadline := scaler.teardown(context.Background(), runner)
	expectDeadlineIn(t, before, deadline, reconcileRetryDelay)
	if len(scaleSetClient.removedIDs) != 0 {
		t.Fatal("expected foreign runner registration to be left alone")
	}
	if len(oxideClient.stopCalls) != 0 {
		t.Fatal("expected foreign runner's instance to be left alone")
	}
}

func TestTeardownRetriesFailedDiskDeletion(t *testing.T) {
	scaler, oxideClient, _ := newTestScaler(t)
	oxideClient.deleteDiskErr = errors.New("disk is busy")
	state := newReconcileState()
	runner := state.runner("disk-retry")
	runner.instance = &oxide.Instance{
		RunState: oxide.InstanceStateStopped,
	}
	runner.disk = &oxide.Disk{
		State: oxide.DiskState{Value: oxide.DiskStateAttached{}},
	}
	runner.teardown = &teardownState{
		policy: teardownPolicyPermanent,
		phase:  teardownPhaseDeprovisioning,
	}
	ctx := context.Background()

	before := time.Now()
	deadline := scaler.teardown(ctx, runner)
	expectDeadlineIn(t, before, deadline, reconcileRetryDelay)
	if runner.instance != nil {
		t.Fatal("expected deleted instance to leave the snapshot")
	}
	if runner.teardown == nil {
		t.Fatal("expected teardown to remain after disk deletion failure")
	}

	// The instance is gone now, so the disk must be detached before the
	// retry can delete it.
	oxideClient.deleteDiskErr = nil
	runner.disk.State = oxide.DiskState{Value: oxide.DiskStateDetached{}}
	expectNoDeadline(t, scaler.teardown(ctx, runner))
	if !runner.gone() {
		t.Fatal("expected nothing to remain of the runner")
	}
}

// Invariant: observing a destroyed instance is not proof that its disk
// has detached; the snapshot may be stale. Only a deletion performed this
// reconciliation authorizes deleting a disk that still looks attached.

func TestTeardownDestroyedInstanceWaitsForDiskDetach(t *testing.T) {
	scaler, oxideClient, _ := newTestScaler(t)
	state := newReconcileState()
	runner := state.runner("destroyed")
	runner.instance = &oxide.Instance{
		RunState: oxide.InstanceStateDestroyed,
	}
	runner.disk = &oxide.Disk{
		State: oxide.DiskState{Value: oxide.DiskStateAttached{}},
	}
	runner.teardown = &teardownState{
		policy: teardownPolicyPermanent,
		phase:  teardownPhaseDeprovisioning,
	}

	before := time.Now()
	deadline := scaler.teardown(context.Background(), runner)
	expectDeadlineIn(t, before, deadline, teardownPollDelay)
	if len(oxideClient.deleteDiskCalls) != 0 {
		t.Fatal("expected attached-looking disk to be left alone")
	}
	if runner.teardown == nil {
		t.Fatal("expected teardown to continue next reconciliation")
	}
}

// Invariant: an instance stuck in creating or starting past the
// provisioning timeout is torn down; one that is merely slow within the
// timeout is left alone.

func TestMarkInstancesForTeardownReapsStuckProvisioning(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	state := newReconcileState()

	stuck := state.runner("stuck")
	stuckSince := time.Now().Add(-provisioningTimeout - time.Minute)
	stuck.instance = &oxide.Instance{
		RunState:            oxide.InstanceStateStarting,
		TimeCreated:         &stuckSince,
		TimeRunStateUpdated: &stuckSince,
	}

	slow := state.runner("slow")
	slowSince := time.Now().Add(-time.Minute)
	slow.instance = &oxide.Instance{
		RunState:            oxide.InstanceStateStarting,
		TimeCreated:         pastGrace(),
		TimeRunStateUpdated: &slowSince,
	}

	scaler.markInstancesForTeardown(state)

	if stuck.teardown == nil ||
		stuck.teardown.policy != teardownPolicyPermanent {
		t.Fatal("expected the stuck instance to be marked for teardown")
	}
	if slow.teardown != nil {
		t.Fatal("expected the slow instance to be left alone")
	}
}

// Invariant: a boot disk with no instance is an orphan left behind by a
// failed provision or an interrupted teardown. It is reaped once past its
// grace period.

func TestMarkDisksForTeardownReapsOrphanedDisks(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	state := newReconcileState()

	orphaned := state.runner("orphaned")
	orphaned.disk = &oxide.Disk{TimeCreated: pastGrace()}

	fresh := state.runner("fresh")
	now := time.Now()
	fresh.disk = &oxide.Disk{TimeCreated: &now}

	attached := state.runner("attached")
	attached.disk = &oxide.Disk{TimeCreated: pastGrace()}
	attached.instance = &oxide.Instance{
		RunState:    oxide.InstanceStateRunning,
		TimeCreated: pastGrace(),
	}

	scaler.markDisksForTeardown(state)

	if orphaned.teardown == nil {
		t.Fatal("expected the orphaned disk to be marked for teardown")
	}
	if fresh.teardown != nil {
		t.Fatal("expected the fresh disk to be protected by grace period")
	}
	if attached.teardown != nil {
		t.Fatal("expected the disk with an instance to be left alone")
	}
}
