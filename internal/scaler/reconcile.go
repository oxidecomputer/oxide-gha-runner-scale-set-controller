package scaler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide.go/oxide"
)

// state is the reconcile loop's working memory, owned exclusively by
// [Scaler.Run]. Everything else a pass needs is observed fresh from
// Oxide and GitHub, so this holds only what cannot be observed: which
// runners have run a job, and which runners have been retired and how
// far their teardown has progressed.
type state struct {
	// desired is the runner count most recently reported by GitHub,
	// or -1 before the first report. No scaling happens until it is
	// known.
	desired int

	// drained is set after a complete observation finds no owned
	// resources or pending teardown work in zero-capacity mode.
	drained bool

	// started tracks runners whose job started. Runners are single
	// use, so a started runner is busy until it is retired. Scale-down
	// only ever picks runners that never started a job.
	started map[string]bool

	// retired tracks runners whose teardown is in progress. Retirement
	// is the single gateway to cleanup: every resource is deleted by
	// retiring its runner name and letting teardown steps converge.
	retired map[string]*retirement

	// discovered tracks instance names already logged, so adopted or
	// externally created instances are announced once.
	discovered map[string]bool
}

// retireReason explains why a runner was retired. It is logged, and
// drainInbox uses it to cancel a scale-down retirement when the
// runner turns out to have won a job.
type retireReason string

const (
	retiredJobCompleted        retireReason = "job completed"
	retiredInstanceHalted      retireReason = "instance halted"
	retiredProvisioningTimeout retireReason = "instance provisioning timed out"
	retiredRegistrationGone    retireReason = "runner registration gone"
	retiredOrphanedDisk        retireReason = "orphaned boot disk"
	retiredScaleDown           retireReason = "scale down"
	retiredCreateFailed        retireReason = "instance creation failed"
)

// retirement records teardown progress for a retired runner. The
// remaining progress is observed from Oxide each pass; only the GitHub
// deregistration is remembered to avoid repeated lookups.
type retirement struct {
	reason       retireReason
	deregistered bool

	// sawNoRegistration records that a pass observed no GitHub
	// registration for this runner while its instance was alive.
	// Teardown requires the absence to hold across two passes before
	// shutting down an alive instance, in case a single lookup was
	// transiently inconsistent while the runner was busy.
	sawNoRegistration bool
}

func newState() *state {
	return &state{
		desired:    -1,
		started:    make(map[string]bool),
		retired:    make(map[string]*retirement),
		discovered: make(map[string]bool),
	}
}

// reconcile performs one convergence pass and reports whether another
// pass should run soon because work is still in progress or failed.
// Passes never return errors; failures are logged and retried because
// every step is idempotent.
//
// An audit pass additionally verifies GitHub registrations of alive
// instances and sweeps for orphaned disks. Those checks call GitHub
// per instance, so they run on the audit interval rather than on every
// pass.
func (s *Scaler) reconcile(ctx context.Context, st *state, audit bool) (requeue bool) {
	st.drained = false
	s.drainInbox(st)

	instances, err := s.listInstances(ctx)
	if err != nil {
		s.logger.Error("listing instances failed", "error", err)
		return true
	}

	disks, err := s.listDisks(ctx)
	if err != nil {
		s.logger.Error("listing disks failed", "error", err)
		return true
	}

	s.discover(st, instances)
	s.retireDefunctRunners(ctx, st, instances, audit)
	if audit {
		s.retireOrphanedDisks(st, instances, disks)
	}

	s.teardown(ctx, st, instances, disks)
	s.prune(st, instances)
	created, retry := s.scale(ctx, st, instances)

	s.active.Store(int64(activeRunnerCount(st, instances) + created))
	st.drained = s.maxRunners == 0 && st.desired >= 0 &&
		len(instances) == 0 && len(disks) == 0 && len(st.retired) == 0

	// Runners still retired at the end of a pass — including ones just
	// retired by scale-down or a failed creation — have teardown work
	// left for the next pass, and a failed scale-up leaves a deficit
	// to retry.
	return retry || len(st.retired) > 0
}

// drainInbox moves facts recorded by the listener handlers into the
// loop's state. A completed job retires its runner; teardown does the
// rest.
func (s *Scaler) drainInbox(st *state) {
	s.mu.Lock()
	st.desired = s.desired
	events := s.events
	s.events = nil
	s.mu.Unlock()

	for _, event := range events {
		st.started[event.runnerName] = true
		if r := st.retired[event.runnerName]; r != nil &&
			r.reason == retiredScaleDown && !r.deregistered {
			// The runner won the race: GitHub assigned it a job
			// before scale-down could deregister it. Keep it; the
			// instance halts when the job ends.
			delete(st.retired, event.runnerName)
			s.logger.Info("scale down canceled; job started",
				"runner.name", event.runnerName,
			)
		}
		if event.completed {
			s.retire(st, event.runnerName, retiredJobCompleted)
		}
	}
}

func (s *Scaler) retire(st *state, name string, reason retireReason) {
	if st.retired[name] != nil {
		return
	}
	st.retired[name] = &retirement{reason: reason}
	s.logger.Info("retiring runner",
		"runner.name", name,
		"retire.reason", reason,
	)
}

// discover logs each owned instance the first time it is observed,
// which announces adoption of instances left behind by a previous
// process.
func (s *Scaler) discover(st *state, instances map[string]oxide.Instance) {
	for name, instance := range instances {
		if st.discovered[name] {
			continue
		}
		st.discovered[name] = true
		s.logger.Info("discovered runner instance",
			"runner.name", name,
			"instance.state", instance.RunState,
		)
	}
}

// retireDefunctRunners retires runners that can no longer run a job:
// their instance halted (the runner script shuts the instance down
// when it exits, and instances never auto-restart), their instance
// remained in an initial provisioning state for too long, or, on audit
// passes, their GitHub registration is gone so no job will ever be
// assigned to them. The grace period protects fresh resources.
func (s *Scaler) retireDefunctRunners(
	ctx context.Context,
	st *state,
	instances map[string]oxide.Instance,
	audit bool,
) {
	for name, instance := range instances {
		if st.retired[name] != nil {
			continue
		}
		if !s.pastGracePeriod(instance.TimeCreated) {
			continue
		}

		if instanceHalted(instance.RunState) {
			s.retire(st, name, retiredInstanceHalted)
			continue
		}
		if s.provisioningTimedOut(instance) {
			s.retire(st, name, retiredProvisioningTimeout)
			continue
		}

		if !audit {
			continue
		}

		ref, err := s.scalesetClient.GetRunnerByName(ctx, name)
		if err != nil {
			s.logger.Error("fetching runner registration failed",
				"runner.name", name,
				"error", err,
			)
			continue
		}
		switch {
		case ref == nil:
			s.retire(st, name, retiredRegistrationGone)
		case ref.RunnerScaleSetID != s.scalesetID:
			s.logger.Warn(
				"instance name collides with a runner from another "+
					"scale set; ensure scale set names are unique "+
					"within the Oxide project",
				"runner.name", name,
				"runner.scale_set_id", ref.RunnerScaleSetID,
			)
		}
	}
}

// retireOrphanedDisks retires runner names for which only a boot disk
// remains, such as when a prior teardown deleted the instance but
// failed before deleting the disk. Teardown then deletes the disk. The
// grace period protects disks whose instance hasn't appeared in a
// listing yet.
func (s *Scaler) retireOrphanedDisks(
	st *state,
	instances map[string]oxide.Instance,
	disks map[string]oxide.Disk,
) {
	for name, disk := range disks {
		if st.retired[name] != nil {
			continue
		}
		if _, ok := instances[name]; ok {
			continue
		}
		if !s.pastGracePeriod(disk.TimeCreated) {
			continue
		}
		s.retire(st, name, retiredOrphanedDisk)
	}
}

// teardown advances every retired runner one step toward deletion. A
// full teardown takes several passes: deregister from GitHub, stop the
// instance, delete the instance, delete the boot disk. Each step is
// observed fresh from the listings, so a teardown interrupted at any
// point resumes where it left off, including across process restarts.
func (s *Scaler) teardown(
	ctx context.Context,
	st *state,
	instances map[string]oxide.Instance,
	disks map[string]oxide.Disk,
) {
	for name, r := range st.retired {
		s.teardownStep(ctx, st, name, r, instances, disks)
	}
}

func (s *Scaler) teardownStep(
	ctx context.Context,
	st *state,
	name string,
	r *retirement,
	instances map[string]oxide.Instance,
	disks map[string]oxide.Disk,
) {
	logger := s.logger.With(
		"runner.name", name,
		"retire.reason", r.reason,
	)

	if !r.deregistered {
		result, err := s.deregisterRunner(ctx, name)
		if err != nil {
			logger.Error("deregistering runner failed; will retry",
				"error", err,
			)
			return
		}
		switch result {
		case deregisterBusy:
			// GitHub refused because the runner's job is still
			// running, so retirement was premature: a stale event or
			// a scale-down race. Stay retired and keep retrying;
			// deregistration succeeds once the job ends. A scale-down
			// retirement is canceled sooner, by its JobStarted fact.
			st.started[name] = true
			logger.Info("runner is still running a job; waiting")
			return
		case deregisterNotFound:
			// A missing registration doesn't prove the runner is
			// idle: a single lookup could be transiently inconsistent
			// while a job runs. Before shutting down an alive
			// instance, require the absence to hold across two passes
			// — unless the job's end was itself observed.
			instance, alive := instances[name]
			if alive && !instanceHalted(instance.RunState) &&
				r.reason != retiredJobCompleted &&
				!r.sawNoRegistration {
				r.sawNoRegistration = true
				return
			}
		}
		r.deregistered = true
	}

	if instance, ok := instances[name]; ok {
		switch instance.RunState {
		case oxide.InstanceStateStopped, oxide.InstanceStateFailed:
			if err := s.deleteInstance(ctx, name); err != nil {
				logger.Error("deleting instance failed; will retry",
					"error", err,
				)
				return
			}
			logger.Info("deleted instance")
		case oxide.InstanceStateStopping, oxide.InstanceStateDestroyed:
			// Wait for the instance to settle.
		default:
			if err := s.stopInstance(ctx, name); err != nil {
				logger.Error("stopping instance failed; will retry",
					"error", err,
				)
			}
		}
		return
	}

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

	delete(st.retired, name)
	logger.Info("runner cleaned up")
}

// deregisterResult reports what deregisterRunner observed.
type deregisterResult int

const (
	// deregisterRemoved means the registration was removed.
	deregisterRemoved deregisterResult = iota
	// deregisterBusy means GitHub refused because the runner's job is
	// still running.
	deregisterBusy
	// deregisterNotFound means no registration owned by this scale
	// set exists. A registration belonging to another scale set is
	// left alone and reported as not found.
	deregisterNotFound
)

// deregisterRunner removes the runner's GitHub registration.
func (s *Scaler) deregisterRunner(ctx context.Context, name string) (deregisterResult, error) {
	ref, err := s.scalesetClient.GetRunnerByName(ctx, name)
	if err != nil {
		return 0, err
	}
	if ref == nil {
		return deregisterNotFound, nil
	}
	if ref.RunnerScaleSetID != s.scalesetID {
		s.logger.Warn(
			"runner registration belongs to another scale set; leaving it",
			"runner.name", name,
			"runner.scale_set_id", ref.RunnerScaleSetID,
		)
		return deregisterNotFound, nil
	}

	err = s.scalesetClient.RemoveRunner(ctx, int64(ref.ID))
	if errors.Is(err, scaleset.JobStillRunningError) {
		return deregisterBusy, nil
	}
	if err != nil {
		return 0, err
	}

	s.logger.Info("deregistered runner", "runner.name", name)
	return deregisterRemoved, nil
}

// prune drops bookkeeping for runner names that have no instance and
// no teardown in progress, such as stale job events for runners of a
// previous process.
func (s *Scaler) prune(st *state, instances map[string]oxide.Instance) {
	for name := range st.started {
		if _, ok := instances[name]; !ok && st.retired[name] == nil {
			delete(st.started, name)
		}
	}
	for name := range st.discovered {
		if _, ok := instances[name]; !ok && st.retired[name] == nil {
			delete(st.discovered, name)
		}
	}
}

// scale converges the active runner count toward the desired count and
// returns how many runners it created, plus whether a failure left a
// deficit to retry. The desired count is bounded below by the
// configured idle minimum and above by the configured maximum; the
// maximum also caps total owned instances, including halted ones whose
// teardown hasn't finished.
func (s *Scaler) scale(
	ctx context.Context,
	st *state,
	instances map[string]oxide.Instance,
) (created int, retry bool) {
	if st.desired < 0 {
		// GitHub hasn't reported a desired count yet; scaling now
		// could tear down runners that are about to be needed.
		return 0, false
	}

	active := activeRunnerCount(st, instances)
	target := min(st.desired+s.minRunners, s.maxRunners)

	switch {
	case active < target:
		capacity := s.maxRunners - len(instances)
		return s.scaleUp(ctx, st, min(target-active, capacity))
	case active > target:
		s.scaleDown(st, active-target, instances)
	}

	return 0, false
}

// scaleDown retires excess runners. Only runners that never started a
// job are candidates, oldest first; if GitHub assigned a job to one in
// the meantime, deregistration fails with a job-still-running error
// and teardown backs off.
func (s *Scaler) scaleDown(
	st *state,
	excess int,
	instances map[string]oxide.Instance,
) {
	candidates := make([]oxide.Instance, 0, len(instances))
	for name, instance := range instances {
		if instanceHalted(instance.RunState) ||
			st.retired[name] != nil ||
			st.started[name] {
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
		s.retire(st, string(instance.Name), retiredScaleDown)
	}
}

// activeRunnerCount counts instances that could run a job now or once
// provisioned: alive and not retired.
func activeRunnerCount(st *state, instances map[string]oxide.Instance) int {
	active := 0
	for name, instance := range instances {
		if !instanceHalted(instance.RunState) && st.retired[name] == nil {
			active++
		}
	}
	return active
}

// instanceHalted reports whether an instance can never host a working
// runner again. Runner instances never restart once stopped: the
// runner script shuts the instance down when it exits and the auto
// restart policy is never.
func instanceHalted(state oxide.InstanceState) bool {
	switch state {
	case oxide.InstanceStateStopped,
		oxide.InstanceStateFailed,
		oxide.InstanceStateDestroyed:
		return true
	}
	return false
}

// pastGracePeriod reports whether a resource is old enough for
// cleanup. The Oxide API always sets time_created, so a missing value
// is malformed; it counts as past grace so such a resource stays
// cleanable instead of gaining permanent protection.
func (s *Scaler) pastGracePeriod(created *time.Time) bool {
	return created == nil || time.Since(*created) >= s.gracePeriod
}

// provisioningTimedOut reports whether an instance has remained in its
// initial creating or starting state for too long. The run-state timestamp
// avoids reaping an older instance merely because it recently entered a
// transitional state. TimeCreated is a fallback for malformed API responses.
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
	return since == nil || time.Since(*since) >= s.provisioningTimeout
}

func timeCreatedOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// listInstances returns the owned instances in the project, keyed by
// name.
func (s *Scaler) listInstances(ctx context.Context) (map[string]oxide.Instance, error) {
	instances, err := s.oxideClient.InstanceListAllPages(
		ctx,
		oxide.InstanceListParams{
			Project: oxide.NameOrId(s.instance.Project),
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

// listDisks returns the owned boot disks in the project, keyed by
// name. Boot disks share their instance's name.
func (s *Scaler) listDisks(ctx context.Context) (map[string]oxide.Disk, error) {
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
