// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/oxide.go/oxide"
)

// Invariant: never create more Oxide instances than MaxRunners, counting
// every owned instance including halted ones awaiting teardown.

func TestScaleUpNeverExceedsMaxRunners(t *testing.T) {
	scaler, oxideClient, _ := newTestScaler(t)
	scaler.maxRunners = 5
	state := newReconcileState()

	states := []oxide.InstanceState{
		oxide.InstanceStateRunning,
		oxide.InstanceStateRunning,
		oxide.InstanceStateStopped, // Halted, but still holds capacity.
	}
	for i, runState := range states {
		runner := state.runner(fmt.Sprintf("existing-%d", i))
		runner.instance = &oxide.Instance{RunState: runState}
	}
	state.runner("existing-2").teardown = &teardownState{
		policy: teardownPolicyPermanent,
		phase:  teardownPhaseRemovingRegistration,
	}

	deadline := scaler.scaleUp(context.Background(), state, 2, 10)
	expectNoDeadline(t, deadline)
	if got := len(oxideClient.createCalls); got != 2 {
		t.Fatalf("expected 2 instance creates (5 max - 3 owned), got %d", got)
	}
}

func TestScaleUpProvisionsConcurrently(t *testing.T) {
	scaler, oxideClient, scaleSetClient := newTestScaler(t)
	oxideClient.createDelay = 25 * time.Millisecond
	state := newReconcileState()

	deadline := scaler.scaleUp(context.Background(), state, 0, 4)
	expectNoDeadline(t, deadline)
	if got := len(oxideClient.createCalls); got != 4 {
		t.Fatalf("expected 4 instance creates, got %d", got)
	}
	if oxideClient.createMaxInFlight < 2 {
		t.Fatalf("expected concurrent instance creation, max in flight %d",
			oxideClient.createMaxInFlight)
	}
	if got := state.activeRunnerCount(); got != 4 {
		t.Fatalf("expected 4 active runners, got %d", got)
	}

	seen := make(map[string]struct{})
	for _, name := range scaleSetClient.jitNames {
		if !strings.HasPrefix(name, scaler.namePrefix()) {
			t.Fatalf("JIT config name %q lacks owned prefix", name)
		}
		seen[name] = struct{}{}
	}
	if len(seen) != 4 {
		t.Fatalf("expected 4 unique runner names, got %d", len(seen))
	}
}

func TestScaleUpTearsDownFailedCreations(t *testing.T) {
	scaler, oxideClient, _ := newTestScaler(t)
	oxideClient.createErr = errors.New("no capacity")
	state := newReconcileState()

	before := time.Now()
	deadline := scaler.scaleUp(context.Background(), state, 0, 2)
	expectDeadlineIn(t, before, deadline, reconcileRetryDelay)

	// A failed create may still exist server-side, so each failed name
	// must be marked for permanent teardown.
	if got := state.teardownCount(); got != 2 {
		t.Fatalf("expected 2 teardowns, got %d", got)
	}
	for _, runner := range state.runners {
		if runner.teardown.policy != teardownPolicyPermanent {
			t.Fatalf("expected permanent teardown, got %s",
				runner.teardown.policy)
		}
	}
	if got := state.activeRunnerCount(); got != 0 {
		t.Fatalf("expected no active runners, got %d", got)
	}
}

func TestScaleUpTearsDownFailedJITConfigs(t *testing.T) {
	scaler, oxideClient, scaleSetClient := newTestScaler(t)
	scaleSetClient.jitErr = errors.New("github unavailable")
	state := newReconcileState()

	before := time.Now()
	deadline := scaler.scaleUp(context.Background(), state, 0, 2)
	expectDeadlineIn(t, before, deadline, reconcileRetryDelay)
	if got := state.teardownCount(); got != 2 {
		t.Fatalf("expected 2 teardowns, got %d", got)
	}
	if got := len(oxideClient.createCalls); got != 0 {
		t.Fatalf("expected no instance creates, got %d", got)
	}
	for _, runner := range state.runners {
		if runner.teardown.policy != teardownPolicyPermanent {
			t.Fatalf("expected permanent teardown, got %s",
				runner.teardown.policy)
		}
	}
}

// Invariant: an image fetch error other than not-found aborts the scale
// up and schedules a retry instead of falling back to a silo image.

func TestScaleUpRetriesWhenImageFetchFails(t *testing.T) {
	scaler, oxideClient, _ := newTestScaler(t)
	oxideClient.projectImageErr = errors.New("oxide unavailable")
	state := newReconcileState()

	before := time.Now()
	deadline := scaler.scaleUp(context.Background(), state, 0, 2)
	expectDeadlineIn(t, before, deadline, reconcileRetryDelay)
	if got := len(oxideClient.createCalls); got != 0 {
		t.Fatalf("expected no instance creates, got %d", got)
	}
	if got := len(oxideClient.imageViewCalls); got != 1 {
		t.Fatalf("expected no silo fallback for a transient error, "+
			"got %d lookups", got)
	}
}

// Invariant: rising demand reclaims runners whose scale-down is still
// inside its cancellation window instead of provisioning replacements,
// but never one whose registration removal already started, because a
// failed removal attempt has an ambiguous outcome.

func TestCancelPendingScaleDownsReclaimsWindowedTeardown(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	state := newReconcileState()
	runner := state.runner("reclaim")
	runner.instance = &oxide.Instance{
		RunState: oxide.InstanceStateRunning,
	}
	runner.teardown = &teardownState{
		policy: teardownPolicyCancelable,
		phase:  teardownPhaseCancellationWindow,
		registrationRemovalAfter: time.Now().Add(
			scaleDownCancellationDelay,
		),
	}

	scaler.cancelPendingScaleDowns(state, 1)
	if runner.teardown != nil {
		t.Fatal("expected increased demand to cancel pending scale down")
	}
	if got := state.activeRunnerCount(); got != 1 {
		t.Fatalf("expected one active runner, got %d", got)
	}
}

func TestCancelPendingScaleDownsSkipsRemovingRegistration(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	state := newReconcileState()
	runner := state.runner("ambiguous")
	runner.instance = &oxide.Instance{
		RunState: oxide.InstanceStateRunning,
	}
	runner.teardown = &teardownState{
		policy: teardownPolicyCancelable,
		phase:  teardownPhaseRemovingRegistration,
	}

	scaler.cancelPendingScaleDowns(state, 1)
	if runner.teardown == nil {
		t.Fatal("expected teardown with ambiguous removal to continue")
	}
}

func TestCancelPendingScaleDownsRespectsExpiredWindow(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	state := newReconcileState()
	runner := state.runner("expired")
	runner.instance = &oxide.Instance{
		RunState: oxide.InstanceStateRunning,
	}
	runner.teardown = &teardownState{
		policy: teardownPolicyCancelable,
		phase:  teardownPhaseCancellationWindow,
		registrationRemovalAfter: time.Now().Add(
			-time.Second,
		),
	}

	scaler.cancelPendingScaleDowns(state, 1)
	if runner.teardown == nil {
		t.Fatal("expected teardown with expired window to continue")
	}
}

// Invariant: busy runners and runners within the grace period are never
// scale-down candidates; the oldest idle runner goes first.

func TestScaleDownProtectsBusyAndFreshRunners(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	state := newReconcileState()

	busy := state.runner("busy")
	busy.busy = true
	busy.instance = &oxide.Instance{
		RunState:    oxide.InstanceStateRunning,
		TimeCreated: pastGrace(),
	}

	fresh := state.runner("fresh")
	now := time.Now()
	fresh.instance = &oxide.Instance{
		RunState:    oxide.InstanceStateRunning,
		TimeCreated: &now,
	}

	idle := state.runner("idle")
	idle.instance = &oxide.Instance{
		RunState:    oxide.InstanceStateRunning,
		TimeCreated: pastGrace(),
	}

	before := time.Now()
	deadline := scaler.scaleDown(state, 3, 0)
	expectDeadlineIn(t, before, deadline, scaleDownCancellationDelay)
	if busy.teardown != nil {
		t.Fatal("expected busy runner to be protected from scale down")
	}
	if fresh.teardown != nil {
		t.Fatal("expected fresh runner to be protected by grace period")
	}
	if !idle.teardown.canCancelForCapacity(time.Now()) {
		t.Fatal("expected idle runner to have a cancelable teardown")
	}
}

func TestScaleDownMarksOldestIdleFirst(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	state := newReconcileState()

	older := state.runner("older")
	olderCreated := time.Now().Add(-2 * time.Hour)
	older.instance = &oxide.Instance{
		RunState:    oxide.InstanceStateRunning,
		TimeCreated: &olderCreated,
	}

	newer := state.runner("newer")
	newerCreated := time.Now().Add(-1 * time.Hour)
	newer.instance = &oxide.Instance{
		RunState:    oxide.InstanceStateRunning,
		TimeCreated: &newerCreated,
	}

	scaler.scaleDown(state, 2, 1)
	if older.teardown == nil {
		t.Fatal("expected the oldest idle runner to be marked")
	}
	if newer.teardown != nil {
		t.Fatal("expected the newer idle runner to be spared")
	}
}
