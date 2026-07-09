package scaler

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide-actions-scaleset/internal/config"
	"github.com/oxidecomputer/oxide.go/oxide"
)

const (
	testScaleSetName = "test"
	testNamePrefix   = "gha-runner-" + testScaleSetName + "-"
	testScaleSetID   = 7
)

// fakeOxide is an in-memory OxideClient. Instances stop immediately
// and deleting an instance detaches its boot disk, mirroring the real
// API closely enough for teardown sequencing. The mutex only matters
// for tests that exercise [Scaler.Run] concurrently.
type fakeOxide struct {
	mu        sync.Mutex
	instances map[string]*oxide.Instance
	disks     map[string]*oxide.Disk

	// errs injects an error for a method name, e.g. "InstanceCreate".
	errs map[string]error

	instanceCreateStarted chan struct{}
	instanceCreateBlock   <-chan struct{}
	instanceCreates       int
	lastInstanceCreate    oxide.InstanceCreateParams
}

func newFakeOxide() *fakeOxide {
	return &fakeOxide{
		instances: make(map[string]*oxide.Instance),
		disks:     make(map[string]*oxide.Disk),
		errs:      make(map[string]error),
	}
}

func (f *fakeOxide) addInstance(
	name string,
	state oxide.InstanceState,
	created time.Time,
) {
	f.instances[name] = &oxide.Instance{
		Name:        oxide.Name(name),
		RunState:    state,
		TimeCreated: &created,
	}
	diskState := oxide.DiskState{Value: oxide.DiskStateAttached{}}
	if instanceHalted(state) && state != oxide.InstanceStateStopped {
		diskState = oxide.DiskState{Value: oxide.DiskStateDetached{}}
	}
	f.disks[name] = &oxide.Disk{
		Name:        oxide.Name(name),
		State:       diskState,
		TimeCreated: &created,
	}
}

func (f *fakeOxide) addDisk(
	name string,
	state oxide.DiskState,
	created time.Time,
) {
	f.disks[name] = &oxide.Disk{
		Name:        oxide.Name(name),
		State:       state,
		TimeCreated: &created,
	}
}

// hasInstance and instanceCount are for assertions in tests that run
// the reconcile loop concurrently.
func (f *fakeOxide) hasInstance(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.instances[name]
	return ok
}

func (f *fakeOxide) instanceCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.instances)
}

func (f *fakeOxide) InstanceListAllPages(
	_ context.Context, _ oxide.InstanceListParams,
) ([]oxide.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["InstanceList"]; err != nil {
		return nil, err
	}
	instances := make([]oxide.Instance, 0, len(f.instances))
	for _, instance := range f.instances {
		instances = append(instances, *instance)
	}
	return instances, nil
}

func (f *fakeOxide) DiskListAllPages(
	_ context.Context, _ oxide.DiskListParams,
) ([]oxide.Disk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["DiskList"]; err != nil {
		return nil, err
	}
	disks := make([]oxide.Disk, 0, len(f.disks))
	for _, disk := range f.disks {
		disks = append(disks, *disk)
	}
	return disks, nil
}

func (f *fakeOxide) ImageView(
	_ context.Context, _ oxide.ImageViewParams,
) (*oxide.Image, error) {
	if err := f.errs["ImageView"]; err != nil {
		return nil, err
	}
	return &oxide.Image{Id: "image-id"}, nil
}

func (f *fakeOxide) InstanceCreate(
	ctx context.Context, params oxide.InstanceCreateParams,
) (*oxide.Instance, error) {
	if f.instanceCreateStarted != nil {
		select {
		case f.instanceCreateStarted <- struct{}{}:
		default:
		}
	}
	if f.instanceCreateBlock != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.instanceCreateBlock:
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.instanceCreates++
	f.lastInstanceCreate = params
	if err := f.errs["InstanceCreate"]; err != nil {
		return nil, err
	}
	name := string(params.Body.Name)
	now := time.Now()
	f.addInstance(name, oxide.InstanceStateRunning, now)
	return f.instances[name], nil
}

func (f *fakeOxide) InstanceStop(
	_ context.Context, params oxide.InstanceStopParams,
) (*oxide.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["InstanceStop"]; err != nil {
		return nil, err
	}
	instance, ok := f.instances[string(params.Instance)]
	if !ok {
		return nil, oxide.ErrObjectNotFound
	}
	instance.RunState = oxide.InstanceStateStopped
	return instance, nil
}

func (f *fakeOxide) InstanceDelete(
	_ context.Context, params oxide.InstanceDeleteParams,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["InstanceDelete"]; err != nil {
		return err
	}
	name := string(params.Instance)
	if _, ok := f.instances[name]; !ok {
		return oxide.ErrObjectNotFound
	}
	delete(f.instances, name)
	if disk, ok := f.disks[name]; ok {
		disk.State = oxide.DiskState{Value: oxide.DiskStateDetached{}}
	}
	return nil
}

func (f *fakeOxide) DiskDelete(
	_ context.Context, params oxide.DiskDeleteParams,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["DiskDelete"]; err != nil {
		return err
	}
	name := string(params.Disk)
	disk, ok := f.disks[name]
	if !ok {
		return oxide.ErrObjectNotFound
	}
	if disk.State.State() == oxide.DiskStateStateAttached {
		return errors.New("disk is attached")
	}
	delete(f.disks, name)
	return nil
}

// fakeScaleSet is an in-memory ScaleSetClient.
type fakeScaleSet struct {
	runners map[string]*scaleset.RunnerReference

	// jobRunning makes RemoveRunner fail with JobStillRunningError for
	// the named runner.
	jobRunning map[string]bool

	// errs injects an error for a method name.
	errs map[string]error

	getRunnerBlock <-chan struct{}
	getRunnerCalls atomic.Int64

	nextID  int
	removed []string
}

func newFakeScaleSet() *fakeScaleSet {
	return &fakeScaleSet{
		runners:    make(map[string]*scaleset.RunnerReference),
		jobRunning: make(map[string]bool),
		errs:       make(map[string]error),
		nextID:     1,
	}
}

func (f *fakeScaleSet) register(name string, scaleSetID int) {
	f.runners[name] = &scaleset.RunnerReference{
		ID:               f.nextID,
		Name:             name,
		RunnerScaleSetID: scaleSetID,
	}
	f.nextID++
}

func (f *fakeScaleSet) GenerateJitRunnerConfig(
	_ context.Context,
	settings *scaleset.RunnerScaleSetJitRunnerSetting,
	scaleSetID int,
) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	if err := f.errs["GenerateJitRunnerConfig"]; err != nil {
		return nil, err
	}
	f.register(settings.Name, scaleSetID)
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner:           f.runners[settings.Name],
		EncodedJITConfig: "and0LWNvbmZpZw==",
	}, nil
}

func (f *fakeScaleSet) GetRunnerByName(
	ctx context.Context, name string,
) (*scaleset.RunnerReference, error) {
	f.getRunnerCalls.Add(1)
	if f.getRunnerBlock != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.getRunnerBlock:
		}
	}
	if err := f.errs["GetRunnerByName"]; err != nil {
		return nil, err
	}
	ref, ok := f.runners[name]
	if !ok {
		return nil, nil
	}
	return ref, nil
}

func (f *fakeScaleSet) RemoveRunner(_ context.Context, runnerID int64) error {
	if err := f.errs["RemoveRunner"]; err != nil {
		return err
	}
	for name, ref := range f.runners {
		if int64(ref.ID) != runnerID {
			continue
		}
		if f.jobRunning[name] {
			return fmt.Errorf("cannot remove: %w",
				scaleset.JobStillRunningError)
		}
		delete(f.runners, name)
		f.removed = append(f.removed, name)
		return nil
	}
	return nil
}

func newTestScaler(
	t *testing.T,
	fakeOxide *fakeOxide,
	fakeScaleSet *fakeScaleSet,
	minRunners, maxRunners int,
) *Scaler {
	t.Helper()

	s, err := New(Config{
		OxideClient:    fakeOxide,
		ScaleSetClient: fakeScaleSet,
		Instance: &config.Instance{
			Project:     "project",
			Image:       "image",
			BootDiskGiB: 32,
			CPUs:        2,
			MemoryGiB:   4,
		},
		Logger:       slog.New(slog.DiscardHandler),
		ScaleSetName: testScaleSetName,
		ScaleSetID:   testScaleSetID,
		MinRunners:   minRunners,
		MaxRunners:   maxRunners,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return s
}

// settle runs reconcile passes until no more work is requested,
// failing the test if convergence takes suspiciously many passes.
func settle(t *testing.T, s *Scaler, st *state, audit bool) {
	t.Helper()

	for pass := range 20 {
		if !s.reconcile(t.Context(), st, audit) {
			return
		}
		_ = pass
	}
	t.Fatal("reconcile did not settle after 20 passes")
}

func runnerName(suffix string) string {
	return testNamePrefix + suffix
}

func past(t *testing.T) time.Time {
	t.Helper()
	return time.Now().Add(-time.Hour)
}

func desire(t *testing.T, s *Scaler, count int) {
	t.Helper()
	if _, err := s.HandleDesiredRunnerCount(t.Context(), count); err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
}

func TestAdoptsRegisteredRunningInstance(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()
	name := runnerName("adopted")
	fo.addInstance(name, oxide.InstanceStateRunning, past(t))
	fss.register(name, testScaleSetID)

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()
	desire(t, s, 1)

	settle(t, s, st, true)

	if _, ok := fo.instances[name]; !ok {
		t.Error("adopted instance was deleted")
	}
	if _, ok := fss.runners[name]; !ok {
		t.Error("adopted runner registration was removed")
	}
	if got := s.active.Load(); got != 1 {
		t.Errorf("active = %d, want 1", got)
	}
	// The adopted instance satisfies the desired count.
	if fo.instanceCreates != 0 {
		t.Errorf("instances created = %d, want 0", fo.instanceCreates)
	}
}

func TestRetiresUnregisteredInstance(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()
	name := runnerName("unregistered")
	fo.addInstance(name, oxide.InstanceStateRunning, past(t))

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()

	// One missing-registration observation must not shut down an
	// alive instance: the lookup could be transiently inconsistent
	// while the runner is busy. Teardown waits for a second pass to
	// confirm.
	s.reconcile(t.Context(), st, true)
	if instance, ok := fo.instances[name]; !ok ||
		instance.RunState != oxide.InstanceStateRunning {
		t.Fatal("alive instance acted on after a single observation")
	}

	settle(t, s, st, true)

	if _, ok := fo.instances[name]; ok {
		t.Error("unregistered instance still exists")
	}
	if _, ok := fo.disks[name]; ok {
		t.Error("boot disk still exists")
	}
	if len(st.retired) != 0 {
		t.Errorf("retired = %v, want empty", st.retired)
	}
}

func TestJobCompletedTearsDownRunner(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()
	name := runnerName("completed")
	fo.addInstance(name, oxide.InstanceStateRunning, time.Now())
	fss.register(name, testScaleSetID)

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()

	err := s.HandleJobCompleted(t.Context(), &scaleset.JobCompleted{
		RunnerName: name,
	})
	if err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}

	settle(t, s, st, false)

	if _, ok := fss.runners[name]; ok {
		t.Error("runner registration still exists")
	}
	if _, ok := fo.instances[name]; ok {
		t.Error("instance still exists")
	}
	if _, ok := fo.disks[name]; ok {
		t.Error("boot disk still exists")
	}
	if len(st.retired) != 0 || len(st.started) != 0 {
		t.Errorf("state not pruned: retired=%v started=%v",
			st.retired, st.started)
	}
}

func TestJobStillRunningKeepsRunner(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()
	name := runnerName("busy")
	fo.addInstance(name, oxide.InstanceStateRunning, past(t))
	fss.register(name, testScaleSetID)
	fss.jobRunning[name] = true

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()

	// A premature completed event retires the runner, but GitHub
	// refuses the deregistration because its job is still running.
	err := s.HandleJobCompleted(t.Context(), &scaleset.JobCompleted{
		RunnerName: name,
	})
	if err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}

	for range 3 {
		if !s.reconcile(t.Context(), st, false) {
			t.Fatal("reconcile stopped retrying while job still running")
		}
	}

	if _, ok := fo.instances[name]; !ok {
		t.Error("busy runner's instance was deleted")
	}
	if _, ok := fss.runners[name]; !ok {
		t.Error("busy runner's registration was removed")
	}
	if len(st.retired) != 1 {
		t.Errorf("retired = %v, want the busy runner", st.retired)
	}
	if !st.started[name] {
		t.Error("busy runner not marked started")
	}

	// Once the job ends, the retried deregistration succeeds and
	// teardown completes.
	fss.jobRunning[name] = false
	settle(t, s, st, false)

	if _, ok := fo.instances[name]; ok {
		t.Error("instance still exists after job ended")
	}
	if _, ok := fo.disks[name]; ok {
		t.Error("boot disk still exists after job ended")
	}
}

func TestJobStartedCancelsScaleDownRetirement(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	winner := runnerName("winner")
	fo.addInstance(winner, oxide.InstanceStateRunning, past(t).Add(-time.Hour))
	fss.register(winner, testScaleSetID)

	idle := runnerName("idle")
	fo.addInstance(idle, oxide.InstanceStateRunning, past(t))
	fss.register(idle, testScaleSetID)

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()

	// Scale to zero: the pass retires both runners but hasn't
	// deregistered them yet (teardown runs before scale-down within a
	// pass).
	desire(t, s, 0)
	s.reconcile(t.Context(), st, false)
	if len(st.retired) != 2 {
		t.Fatalf("retired = %d, want 2", len(st.retired))
	}

	// GitHub assigned a job to the oldest runner before scale-down
	// could deregister it: the started fact must cancel its
	// retirement, or teardown would kill a busy runner.
	err := s.HandleJobStarted(t.Context(), &scaleset.JobStarted{
		RunnerName: winner,
	})
	if err != nil {
		t.Fatalf("HandleJobStarted: %v", err)
	}

	settle(t, s, st, false)

	if _, ok := fo.instances[winner]; !ok {
		t.Error("runner with started job was torn down")
	}
	if _, ok := fss.runners[winner]; !ok {
		t.Error("registration of runner with started job was removed")
	}
	if _, ok := fo.instances[idle]; ok {
		t.Error("idle runner still exists; want it scaled down")
	}
}

func TestScaleUpCreatesRunners(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()
	desire(t, s, 2)

	settle(t, s, st, false)

	if len(fo.instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(fo.instances))
	}
	for name := range fo.instances {
		if !strings.HasPrefix(name, testNamePrefix) {
			t.Errorf("instance %q missing name prefix", name)
		}
		suffix := strings.TrimPrefix(name, testNamePrefix)
		if len(suffix) != 16 {
			t.Errorf("instance %q suffix length = %d, want 16", name, len(suffix))
		} else if _, err := hex.DecodeString(suffix); err != nil {
			t.Errorf("instance %q suffix is not hexadecimal: %v", name, err)
		}
		if _, ok := fss.runners[name]; !ok {
			t.Errorf("instance %q has no runner registration", name)
		}
	}
	if got := s.active.Load(); got != 2 {
		t.Errorf("active = %d, want 2", got)
	}
}

func TestCreateInstanceBootDiskSize(t *testing.T) {
	const bytesPerGiB = oxide.ByteCount(1024 * 1024 * 1024)

	tests := []struct {
		name            string
		imageSize       oxide.ByteCount
		configuredGiB   uint
		wantBootDiskGiB oxide.ByteCount
	}{
		{
			name:            "rounds image size up",
			imageSize:       20*bytesPerGiB + bytesPerGiB/2,
			configuredGiB:   20,
			wantBootDiskGiB: 21,
		},
		{
			name:            "uses image size when configuration is zero",
			imageSize:       20*bytesPerGiB + bytesPerGiB/2,
			configuredGiB:   0,
			wantBootDiskGiB: 21,
		},
		{
			name:            "preserves exact image size",
			imageSize:       20 * bytesPerGiB,
			configuredGiB:   0,
			wantBootDiskGiB: 20,
		},
		{
			name:            "uses larger configured size",
			imageSize:       20*bytesPerGiB + bytesPerGiB/2,
			configuredGiB:   22,
			wantBootDiskGiB: 22,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fo := newFakeOxide()
			s := newTestScaler(t, fo, newFakeScaleSet(), 0, 1)
			s.instance.BootDiskGiB = tt.configuredGiB

			_, err := s.createInstance(
				t.Context(),
				runnerName("create"),
				&oxide.Image{Id: "image-id", Size: tt.imageSize},
				&scaleset.RunnerScaleSetJitRunnerConfig{},
			)
			if err != nil {
				t.Fatalf("createInstance: %v", err)
			}

			attachment := fo.lastInstanceCreate.Body.BootDisk.Value
			bootDisk, ok := attachment.(oxide.InstanceDiskAttachmentCreate)
			if !ok {
				t.Fatalf(
					"boot disk attachment type = %T, want create",
					attachment,
				)
			}
			want := tt.wantBootDiskGiB * bytesPerGiB
			if bootDisk.Size != want {
				t.Errorf("boot disk size = %d, want %d", bootDisk.Size, want)
			}
		})
	}
}

func TestMinRunnersKeptIdle(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	s := newTestScaler(t, fo, fss, 2, 5)
	st := newState()
	desire(t, s, 0)

	settle(t, s, st, false)

	if len(fo.instances) != 2 {
		t.Errorf("instances = %d, want 2 idle minimum", len(fo.instances))
	}
}

func TestDrainModeRemovesRunnersWithoutInterruptingJobs(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	idle := runnerName("idle")
	fo.addInstance(idle, oxide.InstanceStateRunning, past(t))
	fss.register(idle, testScaleSetID)

	busy := runnerName("busy")
	fo.addInstance(busy, oxide.InstanceStateRunning, past(t))
	fss.register(busy, testScaleSetID)
	fss.jobRunning[busy] = true

	s := newTestScaler(t, fo, fss, 0, 0)
	st := newState()

	// Even if GitHub still reports assigned work while draining, zero
	// capacity makes the target zero. The idle runner is removed, while
	// GitHub's job-still-running response protects the busy runner.
	desire(t, s, 2)
	for range 10 {
		s.reconcile(t.Context(), st, false)
	}

	if _, ok := fo.instances[idle]; ok {
		t.Error("idle instance still exists while draining")
	}
	if _, ok := fss.runners[idle]; ok {
		t.Error("idle runner registration still exists while draining")
	}
	if _, ok := fo.instances[busy]; !ok {
		t.Error("busy instance was removed before its job finished")
	}
	if _, ok := fss.runners[busy]; !ok {
		t.Error("busy runner registration was removed before its job finished")
	}
	if fo.instanceCreates != 0 {
		t.Errorf("instances created = %d, want 0 while draining",
			fo.instanceCreates)
	}

	// Once the job ends, the retried retirement completes. The scaler
	// must not replace either runner even though desired remains two.
	fss.jobRunning[busy] = false
	settle(t, s, st, false)

	if len(fo.instances) != 0 {
		t.Errorf("instances = %d, want 0 after drain", len(fo.instances))
	}
	if len(fss.runners) != 0 {
		t.Errorf("registrations = %d, want 0 after drain", len(fss.runners))
	}
	if fo.instanceCreates != 0 {
		t.Errorf("instances created = %d, want 0 after drain",
			fo.instanceCreates)
	}
}

func TestNoScalingBeforeFirstDesiredReport(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()
	name := runnerName("idle")
	fo.addInstance(name, oxide.InstanceStateRunning, past(t))
	fss.register(name, testScaleSetID)

	// minRunners 2 would normally create another runner, and desired 0
	// would normally scale the idle runner down. Neither may happen
	// before GitHub reports a desired count.
	s := newTestScaler(t, fo, fss, 2, 5)
	st := newState()

	settle(t, s, st, true)

	if fo.instanceCreates != 0 {
		t.Errorf("instances created = %d, want 0", fo.instanceCreates)
	}
	if _, ok := fo.instances[name]; !ok {
		t.Error("idle instance was deleted")
	}
}

func TestMaxRunnersCountsHaltedInstances(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	// A halted instance still occupies capacity until its teardown
	// finishes; block its teardown to keep it around.
	halted := runnerName("halted")
	fo.addInstance(halted, oxide.InstanceStateStopped, past(t))
	fo.errs["InstanceDelete"] = errors.New("injected")

	s := newTestScaler(t, fo, fss, 0, 2)
	st := newState()
	desire(t, s, 5)

	s.reconcile(t.Context(), st, false)

	// Capacity is max (2) minus all owned instances (1 halted), even
	// though the desired count asks for more.
	if fo.instanceCreates != 1 {
		t.Errorf("instances created = %d, want 1", fo.instanceCreates)
	}
}

func TestMaxRunnersPreventsCreationWhenOverCapacity(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	for i := range 3 {
		name := runnerName(fmt.Sprintf("halted-%d", i))
		fo.addInstance(name, oxide.InstanceStateStopped, past(t))
	}
	fo.errs["InstanceDelete"] = errors.New("injected")

	s := newTestScaler(t, fo, fss, 0, 2)
	st := newState()
	desire(t, s, 5)

	s.reconcile(t.Context(), st, false)

	if fo.instanceCreates != 0 {
		t.Errorf("instances created = %d, want 0", fo.instanceCreates)
	}
}

func TestInstanceCreateFailureRetiresRegistration(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()
	fo.errs["InstanceCreate"] = errors.New("injected")

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()
	desire(t, s, 1)

	s.reconcile(t.Context(), st, false)

	if len(fss.runners) != 1 {
		t.Fatalf("registrations = %d, want 1 leftover", len(fss.runners))
	}
	if len(st.retired) != 1 {
		t.Fatalf("retired = %d, want 1", len(st.retired))
	}

	// Allow creation to succeed again: the leftover registration is
	// removed and the deficit is filled with a fresh runner.
	delete(fo.errs, "InstanceCreate")
	settle(t, s, st, false)

	if len(fss.runners) != 1 {
		t.Errorf("registrations = %d, want 1", len(fss.runners))
	}
	if len(fo.instances) != 1 {
		t.Errorf("instances = %d, want 1", len(fo.instances))
	}
	for name := range fss.runners {
		if _, ok := fo.instances[name]; !ok {
			t.Errorf("registration %q has no instance", name)
		}
	}
}

func TestScaleDownRetiresOldestIdleRunners(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	oldest := runnerName("oldest")
	fo.addInstance(oldest, oxide.InstanceStateRunning, past(t).Add(-time.Hour))
	fss.register(oldest, testScaleSetID)

	newest := runnerName("newest")
	fo.addInstance(newest, oxide.InstanceStateRunning, past(t))
	fss.register(newest, testScaleSetID)

	busy := runnerName("busy")
	fo.addInstance(busy, oxide.InstanceStateRunning, past(t).Add(-2*time.Hour))
	fss.register(busy, testScaleSetID)

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()

	err := s.HandleJobStarted(t.Context(), &scaleset.JobStarted{
		RunnerName: busy,
	})
	if err != nil {
		t.Fatalf("HandleJobStarted: %v", err)
	}

	// One busy runner plus one assigned job: two idle runners are one
	// too many, and the busy runner must not be a scale-down victim
	// despite being oldest.
	desire(t, s, 2)
	settle(t, s, st, false)

	if _, ok := fo.instances[busy]; !ok {
		t.Error("busy runner was scaled down")
	}
	if _, ok := fo.instances[newest]; !ok {
		t.Error("newest idle runner was scaled down; want oldest first")
	}
	if _, ok := fo.instances[oldest]; ok {
		t.Error("oldest idle runner still exists; want it scaled down")
	}
}

func TestOrphanedDiskDeleted(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()
	name := runnerName("orphan")
	fo.addDisk(name, oxide.DiskState{Value: oxide.DiskStateDetached{}}, past(t))

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()

	settle(t, s, st, true)

	if _, ok := fo.disks[name]; ok {
		t.Error("orphaned disk still exists")
	}
}

func TestGracePeriodProtectsFreshResources(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	// Fresh resources that look like garbage: a stopped instance, an
	// unregistered running instance, and an orphaned disk. All must
	// survive because they may still be provisioning.
	stopped := runnerName("stopped")
	fo.addInstance(stopped, oxide.InstanceStateStopped, time.Now())
	unregistered := runnerName("unregistered")
	fo.addInstance(unregistered, oxide.InstanceStateRunning, time.Now())
	orphan := runnerName("orphan")
	fo.addDisk(orphan, oxide.DiskState{Value: oxide.DiskStateDetached{}}, time.Now())

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()

	settle(t, s, st, true)

	if _, ok := fo.instances[stopped]; !ok {
		t.Error("fresh stopped instance was cleaned up within grace period")
	}
	if _, ok := fo.instances[unregistered]; !ok {
		t.Error("fresh unregistered instance was cleaned up within grace period")
	}
	if _, ok := fo.disks[orphan]; !ok {
		t.Error("fresh orphaned disk was cleaned up within grace period")
	}
}

func TestStoppedInstanceRetiredAfterGrace(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	// A runner whose script crashed: the instance shut down without a
	// job ever completing, and its registration lingers.
	name := runnerName("crashed")
	fo.addInstance(name, oxide.InstanceStateStopped, past(t))
	fss.register(name, testScaleSetID)

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()

	settle(t, s, st, false)

	if _, ok := fss.runners[name]; ok {
		t.Error("crashed runner's registration still exists")
	}
	if _, ok := fo.instances[name]; ok {
		t.Error("crashed runner's instance still exists")
	}
	if _, ok := fo.disks[name]; ok {
		t.Error("crashed runner's boot disk still exists")
	}
}

func TestProvisioningInstanceRetiredAfterTimeout(t *testing.T) {
	for _, state := range []oxide.InstanceState{
		oxide.InstanceStateCreating,
		oxide.InstanceStateStarting,
	} {
		t.Run(string(state), func(t *testing.T) {
			fo := newFakeOxide()
			fss := newFakeScaleSet()
			name := runnerName(string(state))
			fo.addInstance(name, state, past(t))
			stateUpdated := past(t)
			fo.instances[name].TimeRunStateUpdated = &stateUpdated
			fss.register(name, testScaleSetID)

			s := newTestScaler(t, fo, fss, 0, 5)
			s.provisioningTimeout = time.Minute
			st := newState()

			settle(t, s, st, false)

			if _, ok := fss.runners[name]; ok {
				t.Error("timed-out runner registration still exists")
			}
			if _, ok := fo.instances[name]; ok {
				t.Error("timed-out instance still exists")
			}
			if _, ok := fo.disks[name]; ok {
				t.Error("timed-out boot disk still exists")
			}
		})
	}
}

func TestProvisioningTimeoutUsesRunStateTimestamp(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()
	name := runnerName("recently-starting")
	fo.addInstance(name, oxide.InstanceStateStarting, past(t))
	stateUpdated := time.Now()
	fo.instances[name].TimeRunStateUpdated = &stateUpdated
	fss.register(name, testScaleSetID)

	s := newTestScaler(t, fo, fss, 0, 5)
	s.provisioningTimeout = time.Minute
	st := newState()

	settle(t, s, st, false)

	if _, ok := fo.instances[name]; !ok {
		t.Error("recently starting instance was cleaned up")
	}
	if _, ok := fss.runners[name]; !ok {
		t.Error("recently starting runner registration was removed")
	}
}

func TestIgnoresForeignRunnerNames(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()

	err := s.HandleJobCompleted(t.Context(), &scaleset.JobCompleted{
		RunnerName: "other-scaler-runner",
	})
	if err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}

	settle(t, s, st, false)

	if len(st.retired) != 0 {
		t.Errorf("retired = %v, want empty", st.retired)
	}
}

func TestForeignRegistrationNotRemoved(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	// An instance with our prefix whose registration belongs to a
	// different scale set: the instance is torn down but the foreign
	// registration must be left alone.
	name := runnerName("collision")
	fo.addInstance(name, oxide.InstanceStateStopped, past(t))
	fss.register(name, testScaleSetID+1)

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()

	settle(t, s, st, false)

	if _, ok := fss.runners[name]; !ok {
		t.Error("foreign registration was removed")
	}
	if len(fss.removed) != 0 {
		t.Errorf("removed registrations = %v, want none", fss.removed)
	}
	if _, ok := fo.instances[name]; ok {
		t.Error("halted instance still exists")
	}
}

func TestTeardownRetriesAfterFailure(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()
	name := runnerName("flaky")
	fo.addInstance(name, oxide.InstanceStateRunning, past(t))
	fss.register(name, testScaleSetID)

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()

	err := s.HandleJobCompleted(t.Context(), &scaleset.JobCompleted{
		RunnerName: name,
	})
	if err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}

	// Every teardown step fails; the runner must stay retired with
	// its resources intact.
	fo.errs["InstanceStop"] = errors.New("injected")
	for range 3 {
		s.reconcile(t.Context(), st, false)
	}
	if len(st.retired) != 1 {
		t.Fatalf("retired = %d, want 1", len(st.retired))
	}

	// Once the failure clears, teardown finishes.
	delete(fo.errs, "InstanceStop")
	settle(t, s, st, false)

	if _, ok := fo.instances[name]; ok {
		t.Error("instance still exists")
	}
	if _, ok := fo.disks[name]; ok {
		t.Error("boot disk still exists")
	}
}

func TestListFailureRequeuesWithoutChanges(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()
	fo.errs["InstanceList"] = errors.New("injected")

	s := newTestScaler(t, fo, fss, 0, 5)
	st := newState()
	desire(t, s, 2)

	if !s.reconcile(t.Context(), st, false) {
		t.Error("reconcile did not request a retry after list failure")
	}
	if fo.instanceCreates != 0 {
		t.Errorf("instances created = %d, want 0", fo.instanceCreates)
	}
}

func TestRunAdoptsAndConverges(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	leftover := runnerName("leftover")
	fo.addInstance(leftover, oxide.InstanceStateStopped, past(t))
	fss.register(leftover, testScaleSetID)

	s := newTestScaler(t, fo, fss, 0, 5)
	s.retryInterval = time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	desire(t, s, 1)

	deadline := time.After(5 * time.Second)
	for {
		if !fo.hasInstance(leftover) && fo.instanceCount() == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run did not converge: leftover not cleaned or runner not created")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	<-done
}

func TestShutdownFinishesInFlightInstanceCreation(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()
	started := make(chan struct{}, 1)
	createBlock := make(chan struct{})
	fo.instanceCreateStarted = started
	fo.instanceCreateBlock = createBlock

	s := newTestScaler(t, fo, fss, 0, 5)
	desire(t, s, 2)

	runCtx, cancelRun := context.WithCancel(t.Context())
	defer cancelRun()
	runDone := make(chan error, 1)
	go func() {
		runDone <- s.Run(runCtx)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("instance creation did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- s.Shutdown(t.Context())
	}()
	<-s.stop

	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before instance creation finished: %v", err)
	default:
	}

	close(createBlock)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fo.instanceCreates != 1 {
		t.Errorf("instances created = %d, want 1", fo.instanceCreates)
	}
	if len(fo.instances) != 1 {
		t.Errorf("instances = %d, want 1", len(fo.instances))
	}
	if len(fss.runners) != 1 {
		t.Errorf("registrations = %d, want 1", len(fss.runners))
	}
}

func TestRunExitsAfterDrainCompletes(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	idle := runnerName("idle")
	fo.addInstance(idle, oxide.InstanceStateRunning, past(t))
	fss.register(idle, testScaleSetID)

	s := newTestScaler(t, fo, fss, 0, 0)
	s.retryInterval = time.Millisecond

	done := make(chan error, 1)
	go func() {
		done <- s.Run(t.Context())
	}()

	deadline := time.After(5 * time.Second)
	for s.active.Load() != 1 {
		select {
		case err := <-done:
			t.Fatalf("Run exited before GitHub reported desired count: %v", err)
		case <-deadline:
			t.Fatal("initial reconcile did not discover the instance")
		case <-time.After(time.Millisecond):
		}
	}

	desire(t, s, 0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-deadline:
		t.Fatal("Run did not exit after drain completed")
	}

	if len(fo.instances) != 0 {
		t.Errorf("instances = %d, want 0 after drain", len(fo.instances))
	}
	if len(fo.disks) != 0 {
		t.Errorf("disks = %d, want 0 after drain", len(fo.disks))
	}
	if len(fss.runners) != 0 {
		t.Errorf("registrations = %d, want 0 after drain", len(fss.runners))
	}
}

func TestRunCleansInstanceThatStopsAfterStartup(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	name := runnerName("stops-later")
	fo.addInstance(name, oxide.InstanceStateRunning, time.Now())

	s := newTestScaler(t, fo, fss, 0, 5)
	s.auditInterval = 5 * time.Millisecond
	s.retryInterval = time.Millisecond
	s.gracePeriod = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	deadline := time.After(5 * time.Second)
	for s.active.Load() != 1 {
		select {
		case <-deadline:
			t.Fatal("initial reconcile did not discover the instance")
		case <-time.After(time.Millisecond):
		}
	}

	fo.mu.Lock()
	fo.instances[name].RunState = oxide.InstanceStateStopped
	fo.mu.Unlock()

	for fo.hasInstance(name) {
		select {
		case <-deadline:
			t.Fatal("periodic audit did not clean the stopped instance")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	<-done
}

func TestRunRetriesAfterPassTimeout(t *testing.T) {
	fo := newFakeOxide()
	fss := newFakeScaleSet()

	name := runnerName("stalled-api")
	fo.addInstance(name, oxide.InstanceStateStopped, past(t))
	block := make(chan struct{})
	fss.getRunnerBlock = block

	s := newTestScaler(t, fo, fss, 0, 5)
	s.passTimeout = 10 * time.Millisecond
	s.retryInterval = time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.After(5 * time.Second)
	for fss.getRunnerCalls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("reconcile did not retry after its pass timed out")
		case <-time.After(time.Millisecond):
		}
	}

	close(block)
	for fo.hasInstance(name) {
		select {
		case <-deadline:
			t.Fatal("instance was not cleaned after the API recovered")
		case <-time.After(time.Millisecond):
		}
	}
}
