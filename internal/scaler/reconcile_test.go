// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide.go/oxide"
)

// Invariant: the unified runner view joins instances and disks by name.

func TestObserveRunnersJoinsResourcesByName(t *testing.T) {
	scaler, oxideClient, _ := newTestScaler(t)
	prefix := scaler.namePrefix()
	oxideClient.instances = []oxide.Instance{
		{Name: oxide.Name(prefix + "both")},
		{Name: oxide.Name("unrelated-instance")},
	}
	oxideClient.disks = []oxide.Disk{
		{Name: oxide.Name(prefix + "both")},
		{Name: oxide.Name(prefix + "disk-only")},
		{Name: oxide.Name("unrelated-disk")},
	}
	state := newReconcileState()

	if err := scaler.observeRunners(
		context.Background(), state,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(state.runners) != 2 {
		t.Fatalf("expected 2 owned runners, got %d", len(state.runners))
	}
	both := state.runners[prefix+"both"]
	if both == nil || both.instance == nil || both.disk == nil {
		t.Fatal("expected joined runner with instance and disk")
	}
	diskOnly := state.runners[prefix+"disk-only"]
	if diskOnly == nil || diskOnly.instance != nil || diskOnly.disk == nil {
		t.Fatal("expected disk-only runner without an instance")
	}
}

func TestObserveRunnersRefreshesStaleSnapshots(t *testing.T) {
	scaler, oxideClient, _ := newTestScaler(t)
	name := scaler.namePrefix() + "stale"
	state := newReconcileState()
	state.runner(name).instance = &oxide.Instance{
		RunState: oxide.InstanceStateRunning,
	}
	state.runner(name).busy = true
	oxideClient.instances = nil

	if err := scaler.observeRunners(
		context.Background(), state,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	runner := state.runners[name]
	if runner.instance != nil {
		t.Fatal("expected vanished instance to clear the snapshot")
	}
	if !runner.busy {
		t.Fatal("expected busy to persist across observations")
	}
}

// Invariant: state is rebuilt after a restart. A running instance whose
// registration is gone can never receive work and must be torn down.

func TestReconcileRebuildsTeardownsAfterRestart(t *testing.T) {
	scaler, oxideClient, scaleSetClient := newTestScaler(t)
	name := scaler.namePrefix() + "restart"
	oxideClient.instances = []oxide.Instance{
		{
			Name:        oxide.Name(name),
			RunState:    oxide.InstanceStateRunning,
			TimeCreated: pastGrace(),
		},
	}
	state := newReconcileState()

	before := time.Now()
	result, err := scaler.runReconcile(
		context.Background(), state, reconcileReasonInitial,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectDeadlineIn(t, before, result.nextReconcile, teardownPollDelay)

	runner := state.runners[name]
	if runner == nil || runner.teardown == nil {
		t.Fatal("expected unregistered runner to be marked for teardown")
	}
	if runner.teardown.phase != teardownPhaseDeprovisioning {
		t.Fatal("expected missing registration to skip removal")
	}
	if len(scaleSetClient.getCalls) != 1 {
		t.Fatalf("expected one registration audit, got %d",
			len(scaleSetClient.getCalls))
	}
	if len(oxideClient.stopCalls) != 1 {
		t.Fatalf("expected one instance stop, got %d",
			len(oxideClient.stopCalls))
	}
}

func TestReconcileRetriesIncompleteGitHubAudit(t *testing.T) {
	scaler, oxideClient, scaleSetClient := newTestScaler(t)
	oxideClient.instances = []oxide.Instance{
		{
			Name:        oxide.Name(scaler.namePrefix() + "unaudited"),
			RunState:    oxide.InstanceStateRunning,
			TimeCreated: pastGrace(),
		},
	}
	scaleSetClient.getErr = errors.New("github unavailable")
	state := newReconcileState()

	before := time.Now()
	result, err := scaler.runReconcile(
		context.Background(), state, reconcileReasonInitial,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectDeadlineIn(t, before, result.nextReconcile, reconcileRetryDelay)
	if !state.lastGitHubAudit.IsZero() {
		t.Fatal("expected failed audit not to advance the audit time")
	}
}

// Invariant: a failed observation aborts the reconciliation and schedules
// a retry instead of acting on a stale view or stopping Run.

func TestReconcileRetriesFailedObservation(t *testing.T) {
	scaler, oxideClient, _ := newTestScaler(t)
	oxideClient.instanceListErr = errors.New("oxide unavailable")
	state := newReconcileState()
	if _, err := scaler.HandleDesiredRunnerCount(
		context.Background(), 2,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	before := time.Now()
	result, err := scaler.runReconcile(
		context.Background(), state, reconcileReasonWake,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectDeadlineIn(t, before, result.nextReconcile, reconcileRetryDelay)
	if got := len(oxideClient.createCalls); got != 0 {
		t.Fatalf("expected no scaling on a failed observation, "+
			"got %d creates", got)
	}
}

func TestReconcilePrunesVanishedRunners(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	state := newReconcileState()
	// A job event for a runner with no resources, for example after its
	// teardown already completed.
	state.runner("vanished").busy = true

	_, err := scaler.runReconcile(
		context.Background(), state, reconcileReasonWake,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(state.runners) != 0 {
		t.Fatal("expected runner with no resources to be pruned")
	}
}

// Invariant: with MinRunners and MaxRunners at zero, Run drains all owned
// resources and reports completion.

func TestRunReportsCompletedDrain(t *testing.T) {
	config := testConfig()
	config.MinRunners = 0
	config.MaxRunners = 0
	scaler, _, _ := newTestScalerWithConfig(t, config)

	err := scaler.Run(context.Background())
	if !errors.Is(err, ErrScaleSetDrained) {
		t.Fatalf("expected completed drain error, got %v", err)
	}
}

func TestReconcileDrainsOwnedResourcesBeforeCompleting(t *testing.T) {
	config := testConfig()
	config.MinRunners = 0
	config.MaxRunners = 0
	scaler, oxideClient, _ := newTestScalerWithConfig(t, config)
	name := scaler.namePrefix() + "draining"
	oxideClient.instances = []oxide.Instance{
		{
			Name:        oxide.Name(name),
			RunState:    oxide.InstanceStateStopped,
			TimeCreated: pastGrace(),
		},
	}
	oxideClient.disks = []oxide.Disk{
		{
			Name:        oxide.Name(name),
			State:       oxide.DiskState{Value: oxide.DiskStateAttached{}},
			TimeCreated: pastGrace(),
		},
	}
	state := newReconcileState()

	_, err := scaler.runReconcile(
		context.Background(), state, reconcileReasonInitial,
	)
	if !errors.Is(err, ErrScaleSetDrained) {
		t.Fatalf("expected completed drain error, got %v", err)
	}
	if len(oxideClient.deleteInstanceCalls) != 1 ||
		len(oxideClient.deleteDiskCalls) != 1 {
		t.Fatal("expected the halted instance and its disk to be deleted")
	}
	if len(state.runners) != 0 {
		t.Fatal("expected no tracked runners after the drain")
	}
}

// Invariant: the public contract. The listener callbacks drive Run to
// converge Oxide instances on GitHub demand, and a completed job starts
// its runner's teardown.

func TestRunConvergesOnDemand(t *testing.T) {
	scaler, oxideClient, scaleSetClient := newTestScaler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- scaler.Run(ctx) }()

	// Demand two runners and wait for Run to provision them.
	if _, err := scaler.HandleDesiredRunnerCount(ctx, 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitFor(t, "2 instance creates", func() bool {
		return oxideClient.createCount() == 2
	})

	// The next demand report returns the active count observed by the
	// most recent reconciliation.
	waitFor(t, "2 active runners reported", func() bool {
		count, err := scaler.HandleDesiredRunnerCount(ctx, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return count == 2
	})

	// A job runs to completion on one runner. Its teardown removes the
	// GitHub registration, then stops the instance.
	name := scaleSetClient.jitRunnerNames()[0]
	if err := scaler.HandleJobStarted(ctx, &scaleset.JobStarted{
		RunnerName: name,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := scaler.HandleJobCompleted(ctx, &scaleset.JobCompleted{
		RunnerName: name,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitFor(t, "registration removal", func() bool {
		return scaleSetClient.removedCount() == 1
	})
	waitFor(t, "instance stop", func() bool {
		return oxideClient.stopCount() == 1
	})

	cancel()
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Run to honor cancellation")
	}
}

// Invariant: demand is a level. Scaling is suspended until GitHub reports
// the first desired count.

func TestReconcileSuspendsScalingUntilDemandIsKnown(t *testing.T) {
	scaler, oxideClient, _ := newTestScaler(t)
	state := newReconcileState()

	_, err := scaler.runReconcile(
		context.Background(), state, reconcileReasonInitial,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(oxideClient.createCalls) != 0 {
		t.Fatal("expected no scale up before demand is known")
	}

	if _, err := scaler.HandleDesiredRunnerCount(
		context.Background(), 2,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := scaler.runReconcile(
		context.Background(), state, reconcileReasonWake,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(oxideClient.createCalls); got != 2 {
		t.Fatalf("expected 2 instance creates, got %d", got)
	}
}

// Invariant: MinRunners keeps warm capacity beyond demand and MaxRunners
// caps the total, demand included.

func TestReconcileMaintainsMinRunnersAboveDemand(t *testing.T) {
	config := testConfig()
	config.MinRunners = 2
	scaler, oxideClient, _ := newTestScalerWithConfig(t, config)
	state := newReconcileState()
	ctx := context.Background()

	// Min runners also wait for the first demand report.
	if _, err := scaler.runReconcile(
		ctx, state, reconcileReasonInitial,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(oxideClient.createCalls); got != 0 {
		t.Fatalf("expected no creates before demand is known, got %d", got)
	}

	if _, err := scaler.HandleDesiredRunnerCount(ctx, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := scaler.runReconcile(
		ctx, state, reconcileReasonWake,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(oxideClient.createCalls); got != 2 {
		t.Fatalf("expected 2 instance creates at zero demand, got %d", got)
	}

	// MinRunners is warm headroom on top of demand, not a floor: demand 1
	// plus a minimum of 2 targets 3 runners, so one more is created.
	if _, err := scaler.HandleDesiredRunnerCount(ctx, 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := scaler.runReconcile(
		ctx, state, reconcileReasonWake,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(oxideClient.createCalls); got != 3 {
		t.Fatalf("expected 3 instance creates at demand 1, got %d", got)
	}
}

func TestReconcileCapsTargetAtMaxRunners(t *testing.T) {
	config := testConfig()
	config.MinRunners = 2
	config.MaxRunners = 3
	scaler, oxideClient, _ := newTestScalerWithConfig(t, config)
	state := newReconcileState()
	ctx := context.Background()

	if _, err := scaler.HandleDesiredRunnerCount(ctx, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := scaler.runReconcile(
		ctx, state, reconcileReasonWake,
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(oxideClient.createCalls); got != 3 {
		t.Fatalf("expected creates capped at 3, got %d", got)
	}
}
