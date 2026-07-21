// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide.go/oxide"
)

// opLog records destructive API calls across both fakes in the order they
// were attempted, so tests can assert cross-client sequencing such as the
// teardown order.
type opLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *opLog) record(op string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, op)
}

func (l *opLog) snapshot() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ops)
}

// fakeOxide implements [OxideClient]. It records mutating calls and is
// safe for concurrent use because provisioning runs in parallel.
type fakeOxide struct {
	mu  sync.Mutex
	ops *opLog

	instances       []oxide.Instance
	disks           []oxide.Disk
	instanceListErr error

	imageSize       oxide.ByteCount
	imageViewCalls  []oxide.ImageViewParams
	projectImageErr error
	siloImageErr    error

	createErr         error
	createDelay       time.Duration
	createCalls       []oxide.InstanceCreateParams
	createInFlight    int
	createMaxInFlight int

	stopCalls           []string
	stopErr             error
	deleteInstanceCalls []string
	deleteInstanceErr   error
	deleteDiskCalls     []string
	deleteDiskErr       error
}

func (f *fakeOxide) InstanceCreate(
	_ context.Context,
	params oxide.InstanceCreateParams,
) (*oxide.Instance, error) {
	f.mu.Lock()
	f.createInFlight++
	f.createMaxInFlight = max(f.createMaxInFlight, f.createInFlight)
	delay := f.createDelay
	err := f.createErr
	f.mu.Unlock()

	time.Sleep(delay)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.createInFlight--
	f.createCalls = append(f.createCalls, params)

	if err != nil {
		return nil, err
	}
	// Created instances show up in later listings, like the real API.
	now := time.Now()
	instance := oxide.Instance{
		Name:        params.Body.Name,
		RunState:    oxide.InstanceStateStarting,
		TimeCreated: &now,
	}
	f.instances = append(f.instances, instance)
	return &instance, nil
}

func (f *fakeOxide) InstanceListAllPages(
	_ context.Context,
	_ oxide.InstanceListParams,
) ([]oxide.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.instanceListErr != nil {
		return nil, f.instanceListErr
	}
	return slices.Clone(f.instances), nil
}

func (f *fakeOxide) DiskListAllPages(
	_ context.Context,
	_ oxide.DiskListParams,
) ([]oxide.Disk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.disks), nil
}

func (f *fakeOxide) ImageView(
	_ context.Context,
	params oxide.ImageViewParams,
) (*oxide.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imageViewCalls = append(f.imageViewCalls, params)
	if params.Project != "" && f.projectImageErr != nil {
		return nil, f.projectImageErr
	}
	if params.Project == "" && f.siloImageErr != nil {
		return nil, f.siloImageErr
	}
	size := f.imageSize
	if size == 0 {
		size = oxide.ByteCount(1024 * 1024 * 1024)
	}
	return &oxide.Image{Size: size}, nil
}

func (f *fakeOxide) InstanceStop(
	_ context.Context,
	params oxide.InstanceStopParams,
) (*oxide.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops.record("stop-instance")
	f.stopCalls = append(f.stopCalls, string(params.Instance))
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	// The transition persists, so later listings report the instance as
	// stopping, like the real API.
	for i := range f.instances {
		if f.instances[i].Name == oxide.Name(params.Instance) {
			f.instances[i].RunState = oxide.InstanceStateStopping
		}
	}
	return &oxide.Instance{RunState: oxide.InstanceStateStopping}, nil
}

func (f *fakeOxide) InstanceDelete(
	_ context.Context,
	params oxide.InstanceDeleteParams,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops.record("delete-instance")
	f.deleteInstanceCalls = append(
		f.deleteInstanceCalls, string(params.Instance),
	)
	return f.deleteInstanceErr
}

func (f *fakeOxide) DiskDelete(
	_ context.Context,
	params oxide.DiskDeleteParams,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops.record("delete-disk")
	f.deleteDiskCalls = append(f.deleteDiskCalls, string(params.Disk))
	return f.deleteDiskErr
}

// createCount and stopCount read call counts under the fake's lock so
// tests can poll them while [Scaler.Run] works concurrently.
func (f *fakeOxide) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.createCalls)
}

func (f *fakeOxide) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stopCalls)
}

// fakeScaleSet implements [ScaleSetClient]. Registrations are tracked by
// runner name so removal is visible to later calls.
type fakeScaleSet struct {
	mu  sync.Mutex
	ops *opLog

	registrations map[string]*scaleset.RunnerReference
	nextRunnerID  int

	jitErr   error
	jitNames []string

	getErr     error
	getCalls   []string
	removeErr  error
	removedIDs []int64
}

func (f *fakeScaleSet) GenerateJitRunnerConfig(
	_ context.Context,
	settings *scaleset.RunnerScaleSetJitRunnerSetting,
	scaleSetID int,
) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jitErr != nil {
		return nil, f.jitErr
	}
	f.jitNames = append(f.jitNames, settings.Name)
	// A JIT config registers the runner, like the real API.
	f.nextRunnerID++
	f.registrations[settings.Name] = &scaleset.RunnerReference{
		ID:               f.nextRunnerID,
		Name:             settings.Name,
		RunnerScaleSetID: scaleSetID,
	}
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		EncodedJITConfig: "encoded-jit-config",
	}, nil
}

func (f *fakeScaleSet) GetRunnerByName(
	_ context.Context,
	name string,
) (*scaleset.RunnerReference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls = append(f.getCalls, name)
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.registrations[name], nil
}

func (f *fakeScaleSet) RemoveRunner(
	_ context.Context,
	runnerID int64,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops.record("remove-runner")
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removedIDs = append(f.removedIDs, runnerID)
	for name, registration := range f.registrations {
		if int64(registration.ID) == runnerID {
			delete(f.registrations, name)
		}
	}
	return nil
}

// removedCount and jitRunnerNames read state under the fake's lock so
// tests can poll them while [Scaler.Run] works concurrently.
func (f *fakeScaleSet) removedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.removedIDs)
}

func (f *fakeScaleSet) jitRunnerNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.jitNames)
}

func testConfig() Config {
	return Config{
		Instance: InstanceConfig{
			Project:     "runner-project",
			Image:       "runner-image",
			BootDiskGiB: 50,
			CPUs:        4,
			MemoryGiB:   16,
			VPC:         "default",
			Subnet:      "default",
		},
		Runner: RunnerConfig{
			Version: "2.335.1",
			SHA256:  strings.Repeat("a", 64),
		},
		ScaleSet: ScaleSetConfig{
			Namespace: "test/linux-x64",
			ID:        42,
		},
		MinRunners: 0,
		MaxRunners: 10,
	}
}

func newTestScaler(
	t *testing.T,
) (*Scaler, *fakeOxide, *fakeScaleSet) {
	t.Helper()
	return newTestScalerWithConfig(t, testConfig())
}

func newTestScalerWithConfig(
	t *testing.T,
	config Config,
) (*Scaler, *fakeOxide, *fakeScaleSet) {
	t.Helper()
	ops := &opLog{}
	oxideClient := &fakeOxide{ops: ops}
	scaleSetClient := &fakeScaleSet{
		ops:           ops,
		registrations: make(map[string]*scaleset.RunnerReference),
	}
	scaler, err := New(oxideClient, scaleSetClient, config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return scaler, oxideClient, scaleSetClient
}

// pastGrace is a creation time safely past the resource grace period.
func pastGrace() *time.Time {
	created := time.Now().Add(-resourceGracePeriod - time.Minute)
	return &created
}

// expectDeadlineIn asserts that deadline is delay after some moment
// between before and now. Capture before immediately ahead of the call
// under test; the bounds then hold no matter how long the call or the
// test goroutine's scheduling takes.
func expectDeadlineIn(
	t *testing.T,
	before time.Time,
	deadline time.Time,
	delay time.Duration,
) {
	t.Helper()
	if deadline.IsZero() {
		t.Fatalf("expected a deadline about %s away, got none", delay)
	}
	if deadline.Before(before.Add(delay)) ||
		deadline.After(time.Now().Add(delay)) {
		t.Fatalf("expected a deadline %s after the call, got %s away",
			delay, time.Until(deadline))
	}
}

// expectNoDeadline asserts that no further reconciliation was requested.
func expectNoDeadline(t *testing.T, deadline time.Time) {
	t.Helper()
	if !deadline.IsZero() {
		t.Fatalf("expected no deadline, got %s away",
			time.Until(deadline))
	}
}

// waitFor polls condition until it holds, failing the test after a
// generous timeout. It's used to observe [Scaler.Run] from the outside.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Invariant: the name prefix is the ownership marker on Oxide resources,
// so its formula must be stable across releases and derive from both the
// namespace and the scale set ID.

func TestNamePrefixFormulaIsStable(t *testing.T) {
	prefixFor := func(namespace string, id int) string {
		scaler := &Scaler{scaleSet: ScaleSetConfig{
			Namespace: namespace,
			ID:        id,
		}}
		return scaler.namePrefix()
	}

	const namespace = "github.com/oxidecomputer/runner-test"
	// The prefix is the only ownership marker on Oxide resources. If the
	// formula ever changes, an upgraded scaler orphans every resource
	// created by previous versions. This golden value pins the formula.
	const golden = "gha-runner-b61407013ecc3f58f799c3c6-"
	if got := prefixFor(namespace, 42); got != golden {
		t.Fatalf("name prefix formula changed: got %q, want %q",
			got, golden)
	}
	if prefixFor(namespace, 42) == prefixFor(namespace, 43) {
		t.Fatal("expected scale set IDs to produce different prefixes")
	}
	if prefixFor(namespace, 42) ==
		prefixFor("github.com/another-org/runner-test", 42) {
		t.Fatal("expected namespaces to produce different prefixes")
	}
}

// Invariant: the runner name doubles as the Oxide instance, disk, and
// network interface name, so it must carry the owned prefix and fit
// Oxide's name limits.

func TestRunnerNamesFitOxideLimits(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	name := scaler.newRunnerName()
	if len(name) > 63 {
		t.Fatalf("runner name exceeds Oxide's 63-character limit: %q", name)
	}
	if !strings.HasPrefix(name, scaler.namePrefix()) {
		t.Fatalf("runner name %q lacks prefix %q", name, scaler.namePrefix())
	}
}

func TestNewRejectsMissingClientsAndInvalidConfig(t *testing.T) {
	if _, err := New(nil, &fakeScaleSet{}, testConfig()); err == nil {
		t.Fatal("expected an error for a nil Oxide client")
	}
	if _, err := New(&fakeOxide{}, nil, testConfig()); err == nil {
		t.Fatal("expected an error for a nil scale set client")
	}
	invalid := testConfig()
	invalid.MaxRunners = -1
	if _, err := New(&fakeOxide{}, &fakeScaleSet{}, invalid); err == nil {
		t.Fatal("expected an error for an invalid config")
	}
}

func TestNewDefaultsToDiscardLogger(t *testing.T) {
	scaler, _, _ := newTestScaler(t)
	if scaler.logger.Handler() != slog.DiscardHandler {
		t.Fatal("expected discard logger")
	}
}

func TestNewUsesConfiguredLogger(t *testing.T) {
	config := testConfig()
	config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	scaler, err := New(&fakeOxide{}, &fakeScaleSet{}, config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scaler.logger != config.Logger {
		t.Fatal("expected the configured logger to be used")
	}
}
