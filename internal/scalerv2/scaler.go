package scalerv2

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/oxidecomputer/oxide-actions-scaleset/internal/config"
	"github.com/oxidecomputer/oxide.go/oxide"
)

// ScaleSetClient is the subset of [oxide.Client] the scaler uses.
type OxideClient interface {
	DiskDelete(ctx context.Context, params oxide.DiskDeleteParams) error
	DiskListAllPages(ctx context.Context, params oxide.DiskListParams) ([]oxide.Disk, error)
	ImageView(ctx context.Context, params oxide.ImageViewParams) (*oxide.Image, error)
	InstanceCreate(ctx context.Context, params oxide.InstanceCreateParams) (*oxide.Instance, error)
	InstanceDelete(ctx context.Context, params oxide.InstanceDeleteParams) error
	InstanceListAllPages(ctx context.Context, params oxide.InstanceListParams) ([]oxide.Instance, error)
	InstanceStop(ctx context.Context, params oxide.InstanceStopParams) (*oxide.Instance, error)
}

// ScaleSetClient is the subset of [scaleset.Client] the scaler uses.
type ScaleSetClient interface {
	GenerateJitRunnerConfig(ctx context.Context, settings *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(ctx context.Context, name string) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

var (
	_ OxideClient     = (*oxide.Client)(nil)
	_ ScaleSetClient  = (*scaleset.Client)(nil)
	_ listener.Scaler = (*Scaler)(nil)
)

type JobEvent struct {
	RunnerID   string
	RunnerName string
	JobStatus  JobStatus
}

type JobStatus string

const (
	JobStatusStarted   JobStatus = "started"
	JobStatusCompleted JobStatus = "completed"
)

type state struct {
	desiredRunnerCount int
	started            map[string]struct{}
	retired            map[string]retirePolicy
	lastGitHubAudit    time.Time
}

type retirePolicy int

const (
	retirePolicyPermanent  retirePolicy = iota
	retirePolicyCancelable retirePolicy = iota
)

type Scaler struct {
	oxideClient    OxideClient
	scalesetClient ScaleSetClient
	instance       *config.Instance
	logger         *slog.Logger

	scaleSetName string
	scalesetID   int
	minRunners   int
	maxRunners   int

	mu                 sync.Mutex
	desiredRunnerCount int
	jobEvents          []JobEvent

	activeRunnerCount atomic.Int64

	wakeCh chan struct{}
}

func New(oxideClient OxideClient, scalesetClient ScaleSetClient, instance *config.Instance, logger *slog.Logger, scalesetName string, scalesetID int, minRunners int, maxRunners int) (*Scaler, error) {
	return &Scaler{
		oxideClient:        oxideClient,
		scalesetClient:     scalesetClient,
		instance:           instance,
		logger:             logger,
		scaleSetName:       scalesetName,
		scalesetID:         scalesetID,
		minRunners:         minRunners,
		maxRunners:         maxRunners,
		mu:                 sync.Mutex{},
		desiredRunnerCount: -1,
		jobEvents:          make([]JobEvent, 0),
		activeRunnerCount:  atomic.Int64{},
		wakeCh:             make(chan struct{}, 1),
	}, nil
}

func (s *Scaler) HandleDesiredRunnerCount(_ context.Context, count int) (int, error) {
	s.logger.Info("desired runner count",
		"count", count,
		"active", s.activeRunnerCount.Load(),
	)
	s.mu.Lock()
	s.desiredRunnerCount = count
	s.mu.Unlock()
	s.wake()

	return int(s.activeRunnerCount.Load()), nil
}

func (s *Scaler) HandleJobStarted(_ context.Context, jobInfo *scaleset.JobStarted) error {
	s.logger.Info("job started",
		"job.id", jobInfo.JobID,
		"job.runner_name", jobInfo.RunnerName,
	)
	s.recordJobEvent(JobEvent{
		RunnerID:   jobInfo.JobDisplayName,
		RunnerName: jobInfo.RunnerName,
		JobStatus:  JobStatusStarted,
	})
	return nil
}

func (s *Scaler) HandleJobCompleted(_ context.Context, jobInfo *scaleset.JobCompleted) error {
	s.logger.Info("job completed",
		"job.id", jobInfo.JobID,
		"job.runner_name", jobInfo.RunnerName,
	)
	s.recordJobEvent(JobEvent{
		RunnerID:   jobInfo.JobDisplayName,
		RunnerName: jobInfo.RunnerName,
		JobStatus:  JobStatusCompleted,
	})
	return nil
}

func (s *Scaler) Run(ctx context.Context) error {
	// Create state.
	state := &state{
		desiredRunnerCount: 0,
		started:            make(map[string]struct{}),
		retired:            make(map[string]retirePolicy),
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Run initial reconcile.
	requeue := s.runReconcile(ctx, state)

	interval := time.NewTicker(1 * time.Minute)
	defer interval.Stop()

	for {
		var requeueCh <-chan time.Time
		if requeue {
			requeueCh = time.After(5 * time.Second)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wakeCh:
		case <-requeueCh:
		case <-interval.C:
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		// Run periodic reconcile.
		requeue = s.runReconcile(ctx, state)
	}
}

func (s *Scaler) runReconcile(ctx context.Context, state *state) bool {
	reconcileCtx, cancelReconcile := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelReconcile()

	requeue := s.reconcile(reconcileCtx, state)
	if err := reconcileCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return true
		}
	}

	return requeue
}

func (s *Scaler) reconcile(ctx context.Context, state *state) bool {
	s.processJobEvents(state)

	s.logger.Info("reconcile started")
	defer s.logger.Info("reconcile complete")

	ownedInstances, err := s.listOwnedInstances(ctx)
	if err != nil {
		s.logger.Error("failed listing owned instances", "error", err)
		return true
	}

	s.logger.Info("retrieved owned instances",
		"instances", slices.Sorted(maps.Keys(ownedInstances)),
	)

	ownedDisks, err := s.listOwnedDisks(ctx)
	if err != nil {
		s.logger.Error("failed listing owned disks", "error", err)
		return true
	}

	s.logger.Info("retrieved owned disks",
		"disks", slices.Sorted(maps.Keys(ownedDisks)),
	)

	s.retireInstances(state, ownedInstances)
	s.retireDisks(state, ownedInstances, ownedDisks)

	auditGitHub := time.Since(state.lastGitHubAudit) >= 1*time.Minute
	if auditGitHub {
		state.lastGitHubAudit = time.Now()
		s.retireRegistrations(ctx, state, ownedInstances)
	}

	s.teardown(ctx, state, ownedInstances, ownedDisks)

	// Update the runners we currently have started.
	for name := range state.started {
		_, isOwnedInstance := ownedInstances[name]
		_, isMarkedRetired := state.retired[name]

		if isOwnedInstance || isMarkedRetired {
			continue
		}

		delete(state.started, name)
	}

	created, requeue := s.scale(ctx, state, ownedInstances)

	s.activeRunnerCount.Store(int64(activeRunnerCount(state, ownedInstances) + created))

	return requeue || len(state.retired) > 0
}
func (s *Scaler) scale(ctx context.Context, state *state, instances map[string]oxide.Instance) (int, bool) {
	// GitHub hasn't reported a desired runner count yet. Nothing to scale.
	if state.desiredRunnerCount < 0 {
		return 0, false
	}

	active := activeRunnerCount(state, instances)
	target := min(state.desiredRunnerCount+s.minRunners, s.maxRunners)

	switch {
	case active < target:
		capacity := s.maxRunners - len(instances)
		return s.scaleUp(ctx, state, min(target-active, capacity))
	case active > target:
		s.scaleDown(state, active-target, instances)
	}

	return 0, false
}

func (s *Scaler) scaleDown(state *state, excess int, instances map[string]oxide.Instance) {
	candidates := make([]oxide.Instance, 0, len(instances))
	for name, instance := range instances {
		_, isStarted := state.started[name]
		_, isRetired := state.retired[name]

		if instanceHalted(instance.RunState) || isRetired || isStarted || !s.pastGracePeriod(instance.TimeCreated) {
			continue
		}

		candidates = append(candidates, instance)
	}

	slices.SortFunc(candidates, func(a, b oxide.Instance) int {
		return timeCreatedOrZero(a.TimeCreated).Compare(
			timeCreatedOrZero(b.TimeCreated),
		)
	})

	for _, instance := range candidates[:min(excess, len(candidates))] {
		s.retire(state, string(instance.Name), retirePolicyCancelable, "scale down")
	}
}

func timeCreatedOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func activeRunnerCount(state *state, instances map[string]oxide.Instance) int {
	active := 0
	for name, instance := range instances {
		_, isRetired := state.retired[name]
		if !instanceHalted(instance.RunState) && !isRetired {
			active++
		}
	}
	return active
}

func (s *Scaler) teardown(ctx context.Context, state *state, instances map[string]oxide.Instance, disks map[string]oxide.Disk) {
	for name := range state.retired {
		s.teardownStep(ctx, state, name, instances, disks)
	}
}

func (s *Scaler) teardownStep(ctx context.Context, state *state, name string, instances map[string]oxide.Instance, disks map[string]oxide.Disk) {
	logger := s.logger.With(
		"runner.name", name,
	)

	if err := s.removeRunner(ctx, name); err != nil {
		if errors.Is(err, scaleset.JobStillRunningError) {
			logger.Info("runner is still running a job; waiting")
			state.started[name] = struct{}{}
			return
		}

		logger.Error("removing runner registration failed; will retry",
			"error", err,
		)
		return
	}

	if instance, ok := instances[name]; ok {
		switch instance.RunState {
		case oxide.InstanceStateStopped, oxide.InstanceStateFailed:
			if err := s.deleteInstance(ctx, name); err != nil {
				logger.Error("failed deleting instance; will retry",
					"error", err,
				)
				return
			}
			logger.Info("deleted instance")
		case oxide.InstanceStateStopping, oxide.InstanceStateDestroyed:
			// Wait for the instance to settle.
		default:
			if err := s.stopInstance(ctx, name); err != nil {
				logger.Error("failed stopping instance; will retry",
					"error", err,
				)
			}
		}
		return
	}

	logger.Info("teardown: disks loop")

	if disk, ok := disks[name]; ok {
		switch disk.State.State() {
		case oxide.DiskStateStateDetached, oxide.DiskStateStateFaulted:
			if err := s.deleteDisk(ctx, name); err != nil {
				logger.Error("deleting boot disk failed; will retry",
					"error", err,
				)
				return
			}
			logger.Info("deleted boot disk")
		default:
			// Wait for the disk to detach.
		}
		return
	}

	delete(state.retired, name)
	logger.Info("runner teardown complete")
}

func (s *Scaler) stopInstance(ctx context.Context, name string) error {
	_, err := s.oxideClient.InstanceStop(ctx, oxide.InstanceStopParams{
		Project:  oxide.NameOrId(s.instance.Project),
		Instance: oxide.NameOrId(name),
	})
	if errors.Is(err, oxide.ErrObjectNotFound) {
		return nil
	}
	return err
}

func (s *Scaler) deleteInstance(ctx context.Context, name string) error {
	err := s.oxideClient.InstanceDelete(ctx, oxide.InstanceDeleteParams{
		Project:  oxide.NameOrId(s.instance.Project),
		Instance: oxide.NameOrId(name),
	})
	if errors.Is(err, oxide.ErrObjectNotFound) {
		return nil
	}
	return err
}

func (s *Scaler) deleteDisk(ctx context.Context, name string) error {
	err := s.oxideClient.DiskDelete(ctx, oxide.DiskDeleteParams{
		Project: oxide.NameOrId(s.instance.Project),
		Disk:    oxide.NameOrId(name),
	})
	if errors.Is(err, oxide.ErrObjectNotFound) {
		return nil
	}
	return err
}

func (s *Scaler) removeRunner(ctx context.Context, name string) error {
	runnerReference, err := s.scalesetClient.GetRunnerByName(ctx, name)
	if err != nil {
		return err
	}
	if runnerReference == nil {
		return nil
	}

	if runnerReference.RunnerScaleSetID != s.scalesetID {
		return nil
	}

	return s.scalesetClient.RemoveRunner(ctx, int64(runnerReference.ID))
}

func (s *Scaler) pastGracePeriod(created *time.Time) bool {
	return created == nil || time.Since(*created) >= 5*time.Minute
}

func (s *Scaler) provisioningTimedOut(instance oxide.Instance) bool {
	switch instance.RunState {
	case oxide.InstanceStateCreating, oxide.InstanceStateStarting:
	default:
		return false
	}

	since := instance.TimeRunStateUpdated
	if since == nil {
		since = instance.TimeCreated
	}
	return since == nil || time.Since(*since) >= 10*time.Minute
}

func instanceHalted(state oxide.InstanceState) bool {
	switch state {
	case oxide.InstanceStateStopped,
		oxide.InstanceStateFailed,
		oxide.InstanceStateDestroyed:
		return true
	}
	return false
}

func (s *Scaler) retireInstances(state *state, instances map[string]oxide.Instance) {
	for name, instance := range instances {
		// Already retired.
		if _, ok := state.retired[name]; ok {
			continue
		}

		// Still within grace period.
		if !s.pastGracePeriod(instance.TimeCreated) {
			continue
		}

		if instanceHalted(instance.RunState) {
			s.retire(state, name, retirePolicyPermanent, "instance halted")
			continue
		}

		if s.provisioningTimedOut(instance) {
			s.retire(state, name, retirePolicyPermanent, "instance provisioning timed out")
			continue
		}
	}
}

func (s *Scaler) retireRegistrations(ctx context.Context, state *state, instances map[string]oxide.Instance) {
	for name, instance := range instances {
		// Already retired.
		if _, ok := state.retired[name]; ok {
			continue
		}

		// Still within grace period.
		if !s.pastGracePeriod(instance.TimeCreated) {
			continue
		}

		// Only check running instances for a missing registration.
		if instanceHalted(instance.RunState) {
			continue
		}

		runnerReference, err := s.scalesetClient.GetRunnerByName(ctx, name)
		if err != nil {
			s.logger.Error("failed fetching runner from github; will retry",
				"error", err,
			)
			continue
		}

		switch {
		case runnerReference == nil:
			s.retire(state, name, retirePolicyPermanent, "runner registration gone")
		case runnerReference.RunnerScaleSetID != s.scalesetID:
			s.logger.Warn("runner is managed by another scale set",
				"runner.name", name,
				"runner.scale_set.id", runnerReference.RunnerScaleSetID,
				"scale_set.id", s.scalesetID,
			)
		}
	}
}

func (s *Scaler) retireDisks(state *state, instances map[string]oxide.Instance, disks map[string]oxide.Disk) {
	for name, disk := range disks {
		// Already retired.
		if _, ok := state.retired[name]; ok {
			continue
		}

		// There's still an owned instance that we haven't retired.
		if _, ok := instances[name]; ok {
			continue
		}

		// Still within grace period.
		if !s.pastGracePeriod(disk.TimeCreated) {
			continue
		}

		s.retire(state, name, retirePolicyPermanent, "orphaned boot disk")
	}
}

func (s *Scaler) listOwnedInstances(ctx context.Context) (map[string]oxide.Instance, error) {
	instances, err := s.oxideClient.InstanceListAllPages(ctx, oxide.InstanceListParams{
		Project: oxide.NameOrId(s.instance.Project),
	})
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

func (s *Scaler) listOwnedDisks(ctx context.Context) (map[string]oxide.Disk, error) {
	disks, err := s.oxideClient.DiskListAllPages(ctx, oxide.DiskListParams{
		Project: oxide.NameOrId(s.instance.Project),
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

func (s *Scaler) namePrefix() string {
	return "gha-runner-" + s.scaleSetName + "-"
}

func (s *Scaler) processJobEvents(state *state) {
	s.mu.Lock()
	state.desiredRunnerCount = s.desiredRunnerCount
	jobEvents := s.jobEvents
	s.jobEvents = nil
	s.mu.Unlock()

	for _, jobEvent := range jobEvents {
		switch jobEvent.JobStatus {
		case JobStatusStarted, JobStatusCompleted:
		default:
			s.logger.Warn("ignoring job event with unknown status",
				"job.status", jobEvent.JobStatus,
				"runner.name", jobEvent.RunnerName,
			)
			continue
		}

		state.started[jobEvent.RunnerName] = struct{}{}

		policy, isRetired := state.retired[jobEvent.RunnerName]
		if isRetired && policy == retirePolicyCancelable {
			delete(state.retired, jobEvent.RunnerName)
			if jobEvent.JobStatus == JobStatusStarted {
				s.logger.Info("scale down canceled; job started",
					"runner.name", jobEvent.RunnerName,
				)
			}
		}

		if jobEvent.JobStatus == JobStatusCompleted {
			s.retire(state, jobEvent.RunnerName, retirePolicyPermanent, "job completed")
		}
	}
}

// retire records a runner for teardown. cause is only logged.
func (s *Scaler) retire(state *state, name string, policy retirePolicy, reason string) {
	if _, ok := state.retired[name]; ok {
		return
	}
	state.retired[name] = policy

	s.logger.Info("retiring runner",
		"runner.name", name,
		"retire.reason", reason,
	)
}

func (s *Scaler) recordJobEvent(jobEvent JobEvent) {
	s.mu.Lock()
	s.jobEvents = append(s.jobEvents, jobEvent)
	s.mu.Unlock()
	s.wake()
}

func (s *Scaler) wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

//go:embed userdata.sh.tmpl
var userDataTemplateText string

// userDataTemplate renders the user data script that downloads the
// GitHub Actions runner and starts it with a JIT config. The JIT
// config is base64 encoded, so interpolating it into the script's
// double-quoted string is injection safe.
var userDataTemplate = template.Must(
	template.New("userdata").Parse(userDataTemplateText),
)

// scaleUp creates up to count runners and returns how many it created,
// plus whether a failure stopped the batch so the remainder should be
// retried by another pass soon.
//
// Creating a runner is the one transaction reconciliation cannot fully
// observe: the GitHub registration must exist before the instance so
// the instance can boot with a JIT config, and a registration without
// an instance has an unlisted, random name. When instance creation
// fails, the runner is retired so teardown removes the registration.
// Only a crash in between leaks it, and GitHub removes never-used JIT
// runners automatically.
func (s *Scaler) scaleUp(ctx context.Context, state *state, count int) (created int, retry bool) {
	if count <= 0 {
		return 0, false
	}

	image, err := s.fetchImage(ctx)
	if err != nil {
		s.logger.Error("fetching image failed", "error", err)
		return 0, true
	}

	for range count {
		name, err := s.newRunnerName()
		if err != nil {
			s.logger.Error("generating runner name failed", "error", err)
			return created, true
		}

		jitConfig, err := s.scalesetClient.GenerateJitRunnerConfig(
			ctx,
			&scaleset.RunnerScaleSetJitRunnerSetting{
				Name: name,
			},
			s.scalesetID,
		)
		if err != nil {
			s.logger.Error("generating jit config failed", "error", err)
			return created, true
		}

		if _, err := s.createInstance(ctx, name, image, jitConfig); err != nil {
			s.logger.Error("creating instance failed",
				"runner.name", name,
				"error", err,
			)

			s.retire(state, name, retirePolicyPermanent, "instance creation failed")
			return created, true
		}

		created++
		s.logger.Info("created runner", "runner.name", name)
	}

	return created, false
}

func (s *Scaler) newRunnerName() (string, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generating name suffix: %w", err)
	}
	return s.namePrefix() + hex.EncodeToString(suffix), nil
}

// fetchImage resolves the configured image, checking for a project
// image first and falling back to a silo image when the project has
// none. It is called on every scale-up, rather than once at startup,
// so a republished image is picked up without restarting the process.
func (s *Scaler) fetchImage(ctx context.Context) (*oxide.Image, error) {
	image, err := s.oxideClient.ImageView(ctx, oxide.ImageViewParams{
		Image:   oxide.NameOrId(s.instance.Image),
		Project: oxide.NameOrId(s.instance.Project),
	})
	if err != nil && errors.Is(err, oxide.ErrObjectNotFound) {
		// Not a project image; fall back to a silo image.
		image, err = s.oxideClient.ImageView(ctx, oxide.ImageViewParams{
			Image: oxide.NameOrId(s.instance.Image),
		})
	}

	if err != nil {
		return nil, err
	}

	return image, nil
}

func (s *Scaler) createInstance(
	ctx context.Context,
	name string,
	image *oxide.Image,
	jitConfig *scaleset.RunnerScaleSetJitRunnerConfig,
) (*oxide.Instance, error) {
	var userData bytes.Buffer
	err := userDataTemplate.Execute(&userData, struct {
		JITConfig string
	}{
		JITConfig: jitConfig.EncodedJITConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("rendering user data: %w", err)
	}

	const bytesPerGiB = oxide.ByteCount(1024 * 1024 * 1024)
	imageGiB := image.Size / bytesPerGiB
	if image.Size%bytesPerGiB != 0 {
		imageGiB++
	}
	bootDiskGiB := max(oxide.ByteCount(s.instance.BootDiskGiB), imageGiB)

	instance, err := s.oxideClient.InstanceCreate(ctx, oxide.InstanceCreateParams{
		Project: oxide.NameOrId(s.instance.Project),
		Body: &oxide.InstanceCreate{
			AutoRestartPolicy: oxide.InstanceAutoRestartPolicyNever,
			BootDisk: oxide.InstanceDiskAttachment{
				Value: oxide.InstanceDiskAttachmentCreate{
					Name:        oxide.Name(name),
					Description: "Managed by oxide-actions-scaleset.",
					Size:        bootDiskGiB * bytesPerGiB,
					DiskBackend: oxide.DiskBackend{
						Value: oxide.DiskBackendDistributed{
							DiskSource: oxide.DiskSource{
								Value: oxide.DiskSourceImage{
									ImageId: image.Id,
								},
							},
						},
					},
				},
			},
			Description: "Managed by oxide-actions-scaleset.",
			Hostname:    oxide.Hostname(name),
			Memory:      oxide.ByteCount(s.instance.MemoryGiB * 1024 * 1024 * 1024),
			Name:        oxide.Name(name),
			Ncpus:       oxide.InstanceCpuCount(s.instance.CPUs),
			NetworkInterfaces: oxide.InstanceNetworkInterfaceAttachment{
				Value: oxide.InstanceNetworkInterfaceAttachmentDefaultDualStack{},
			},
			Start:    new(true),
			UserData: base64.StdEncoding.EncodeToString(userData.Bytes()),
		},
	})
	if err != nil {
		return nil, err
	}

	return instance, nil
}
