// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"context"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// Invariant: a job start cancels a cancelable teardown, even one whose
// registration removal is in flight because GitHub is authoritative that
// the registration still exists, but never one whose registration removal
// already succeeded.

func TestJobStartedCancelsPendingScaleDown(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	scaler.jobEvents = []jobEvent{
		{runnerName: "started", jobStatus: jobStatusStarted},
	}
	state := newReconcileState()
	runner := state.runner("started")
	runner.teardown = &teardownState{
		policy: teardownPolicyCancelable,
		phase:  teardownPhaseCancellationWindow,
	}

	scaler.processJobEvents(state)

	if runner.teardown != nil {
		t.Fatal("expected job start to cancel the pending scale down")
	}
	if !runner.busy {
		t.Fatal("expected runner to be busy")
	}
}

func TestJobStartedCancelsDuringRegistrationRemoval(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	scaler.jobEvents = []jobEvent{
		{runnerName: "removing", jobStatus: jobStatusStarted},
	}
	state := newReconcileState()
	runner := state.runner("removing")
	runner.teardown = &teardownState{
		policy: teardownPolicyCancelable,
		phase:  teardownPhaseRemovingRegistration,
	}

	scaler.processJobEvents(state)

	if runner.teardown != nil {
		t.Fatal("expected job start to cancel the in-flight scale down")
	}
	if !runner.busy {
		t.Fatal("expected runner to be busy")
	}
}

func TestJobStartedCannotCancelDeprovisioning(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	scaler.jobEvents = []jobEvent{
		{runnerName: "deprovisioning", jobStatus: jobStatusStarted},
	}
	state := newReconcileState()
	runner := state.runner("deprovisioning")
	runner.teardown = &teardownState{
		policy: teardownPolicyCancelable,
		phase:  teardownPhaseDeprovisioning,
	}

	scaler.processJobEvents(state)

	if runner.teardown == nil {
		t.Fatal("expected teardown to continue after registration removal")
	}
	if !runner.busy {
		t.Fatal("expected runner to be busy")
	}
}

func TestJobCompletedUpgradesTeardownPreservingProgress(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	scaler.jobEvents = []jobEvent{
		{runnerName: "completed", jobStatus: jobStatusCompleted},
	}
	state := newReconcileState()
	runner := state.runner("completed")
	runner.teardown = &teardownState{
		policy: teardownPolicyCancelable,
		phase:  teardownPhaseDeprovisioning,
	}

	scaler.processJobEvents(state)

	teardown := runner.teardown
	if teardown.policy != teardownPolicyPermanent {
		t.Fatalf("expected permanent policy, got %s", teardown.policy)
	}
	if teardown.phase != teardownPhaseDeprovisioning {
		t.Fatal("expected teardown progress to be preserved")
	}
}

func TestJobCompletedUpgradeClosesCancellationWindow(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	scaler.jobEvents = []jobEvent{
		{runnerName: "windowed", jobStatus: jobStatusCompleted},
	}
	state := newReconcileState()
	runner := state.runner("windowed")
	runner.teardown = &teardownState{
		policy: teardownPolicyCancelable,
		phase:  teardownPhaseCancellationWindow,
		registrationRemovalAfter: time.Now().Add(
			scaleDownCancellationDelay,
		),
	}

	scaler.processJobEvents(state)

	teardown := runner.teardown
	if teardown.policy != teardownPolicyPermanent {
		t.Fatalf("expected permanent policy, got %s", teardown.policy)
	}
	if teardown.phase != teardownPhaseRemovingRegistration {
		t.Fatal("expected the cancellation window to close")
	}
	if !teardown.registrationRemovalAfter.IsZero() {
		t.Fatal("expected permanent teardown delay to be cleared")
	}
}

func TestJobEventsRequireRunnerNames(t *testing.T) {
	scaler, _, _ := newTestScaler(t)

	err := scaler.HandleJobStarted(
		context.Background(), &scaleset.JobStarted{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err = scaler.HandleJobCompleted(
		context.Background(), &scaleset.JobCompleted{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(scaler.jobEvents) != 0 {
		t.Fatal("expected events without runner names to be dropped")
	}
}
