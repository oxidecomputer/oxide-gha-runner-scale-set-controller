// Package scaler provisions GitHub Actions runners on Oxide instances
// for a runner scale set.
//
// The design is level triggered. The listener handlers never touch
// cloud resources; they only record facts (the latest desired runner
// count and job lifecycle events) and wake the reconcile loop. A
// single goroutine, [Scaler.Run], owns all state and performs every
// cloud mutation by repeatedly converging observed state (Oxide
// instances and disks, GitHub runner registrations) with desired
// state.
//
// Every convergence step is idempotent and crash resumable: resources
// are discovered by listing the Oxide project for names carrying the
// scale-set-derived prefix, so a restarted process adopts whatever a
// previous process left behind on its first pass. The one exception is
// a GitHub runner registration whose instance was never created (a
// crash between registration and instance creation): its random name
// cannot be rediscovered, and GitHub removes such never-used JIT
// runners automatically.
package scaler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/oxidecomputer/oxide-actions-scaleset/internal/config"
	"github.com/oxidecomputer/oxide.go/oxide"
)

const (
	// defaultAuditInterval is how often a reconcile pass additionally
	// verifies GitHub runner registrations and sweeps for orphaned
	// disks.
	defaultAuditInterval = 1 * time.Minute

	// defaultRetryInterval is how soon the reconcile loop runs another
	// pass while runner teardowns are in progress or a pass failed.
	defaultRetryInterval = 5 * time.Second

	// defaultReconcileTimeout bounds a complete pass so one stalled API
	// call cannot prevent later audits from running indefinitely.
	defaultReconcileTimeout = time.Minute

	// defaultProvisioningTimeout bounds how long an instance may remain
	// in an initial creating or starting state.
	defaultProvisioningTimeout = 10 * time.Minute

	// defaultGracePeriod is how long an instance or disk must exist
	// before reconciliation may clean it up for reasons other than an
	// explicit job event. It protects resources that are still
	// provisioning from being mistaken for garbage.
	defaultGracePeriod = 1 * time.Minute

	instanceNamePrefix = "gha-runner-"
)

// OxideClient is the subset of [oxide.Client] the scaler uses.
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

// Scaler implements [listener.Scaler]. The handler methods record
// facts for the reconcile loop; [Scaler.Run] does all the work.
type Scaler struct {
	oxideClient    OxideClient
	scalesetClient ScaleSetClient
	instance       *config.Instance
	logger         *slog.Logger

	scaleSetName string
	scalesetID   int
	minRunners   int
	maxRunners   int

	auditInterval       time.Duration
	retryInterval       time.Duration
	passTimeout         time.Duration
	provisioningTimeout time.Duration
	gracePeriod         time.Duration

	// The inbox holds facts recorded by the listener handlers for the
	// reconcile loop to consume. It is the only state the handlers
	// touch.
	mu       sync.Mutex
	desired  int // latest desired runner count; -1 until first report
	events   []jobEvent
	wake     chan struct{} // buffered with capacity 1; coalesces wakeups
	stop     chan struct{} // closed to stop after the current transaction
	done     chan struct{} // closed when Run returns
	stopOnce sync.Once

	// active is the runner count observed by the most recent reconcile
	// pass, published for HandleDesiredRunnerCount to report back.
	active atomic.Int64
}

type jobEvent struct {
	runnerName string
	completed  bool
}

type Config struct {
	OxideClient    OxideClient
	ScaleSetClient ScaleSetClient
	Instance       *config.Instance
	Logger         *slog.Logger

	ScaleSetName string
	ScaleSetID   int
	MinRunners   int
	MaxRunners   int
}

func New(cfg Config) (*Scaler, error) {
	if cfg.OxideClient == nil {
		return nil, errors.New("oxide client is required")
	}

	if cfg.ScaleSetClient == nil {
		return nil, errors.New("scaleset client is required")
	}

	if cfg.Instance == nil {
		return nil, errors.New("instance configuration is required")
	}

	if cfg.ScaleSetName == "" {
		return nil, errors.New("scale set name is required")
	}

	if cfg.Logger == nil {
		return nil, errors.New("logger is required")
	}

	if cfg.MinRunners < 0 {
		return nil, errors.New("min runners must not be negative")
	}

	if cfg.MaxRunners < 0 {
		return nil, errors.New("max runners must not be negative")
	}

	if cfg.MinRunners > cfg.MaxRunners {
		return nil, errors.New(
			"min runners must be less than or equal to max runners",
		)
	}

	return &Scaler{
		oxideClient:         cfg.OxideClient,
		scalesetClient:      cfg.ScaleSetClient,
		instance:            cfg.Instance,
		logger:              cfg.Logger,
		scaleSetName:        cfg.ScaleSetName,
		scalesetID:          cfg.ScaleSetID,
		minRunners:          cfg.MinRunners,
		maxRunners:          cfg.MaxRunners,
		auditInterval:       defaultAuditInterval,
		retryInterval:       defaultRetryInterval,
		passTimeout:         defaultReconcileTimeout,
		provisioningTimeout: defaultProvisioningTimeout,
		gracePeriod:         defaultGracePeriod,
		desired:             -1,
		wake:                make(chan struct{}, 1),
		stop:                make(chan struct{}),
		done:                make(chan struct{}),
	}, nil
}

// Run drives the reconcile loop until ctx is done. It must be running
// for the [listener.Scaler] handlers to have any effect.
//
// The first pass is an audit: it discovers the instances, disks, and
// runner registrations a previous process left behind, before any
// scaling decisions are made. Later passes run when a handler records
// a fact, shortly after a pass that left work in progress, and at
// every audit interval.
//
// Run returns nil once a zero-capacity scale set is fully drained or graceful
// shutdown is requested with [Scaler.Shutdown]. It returns ctx.Err() when hard
// cancellation interrupts a pass. Teardowns interrupted by hard cancellation
// are resumed by the first pass of the next process.
func (s *Scaler) Run(ctx context.Context) error {
	defer close(s.done)

	if err := ctx.Err(); err != nil {
		return err
	}
	if s.stopping() {
		return nil
	}

	audits := time.NewTicker(s.auditInterval)
	defer audits.Stop()

	st := newState()
	requeue := s.runPass(ctx, st, true, "startup")
	if st.drained {
		s.logger.Info("scale set drain completed")
		return nil
	}
	if s.stopping() {
		return nil
	}

	for {
		var retry <-chan time.Time
		if requeue {
			retry = time.After(s.retryInterval)
		}

		audit := false
		trigger := "wakeup"
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stop:
			return nil
		case <-s.wake:
		case <-audits.C:
			audit = true
			trigger = "audit"
		case <-retry:
			trigger = "retry"
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.stopping() {
			return nil
		}

		requeue = s.runPass(ctx, st, audit, trigger)
		if st.drained {
			s.logger.Info("scale set drain completed")
			return nil
		}
	}
}

// Shutdown requests graceful shutdown and waits for Run to finish. A runner
// transaction already in progress is allowed to complete, but no additional
// runners are created. The caller may hard-cancel Run's context if ctx expires.
func (s *Scaler) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() {
		close(s.stop)
	})

	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scaler) stopping() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func (s *Scaler) runPass(
	ctx context.Context,
	st *state,
	audit bool,
	trigger string,
) bool {
	started := time.Now()
	logger := s.logger.With(
		"reconcile.trigger", trigger,
		"reconcile.audit", audit,
	)
	logger.Info("reconcile pass started")

	passCtx, cancel := context.WithTimeout(ctx, s.passTimeout)
	defer cancel()

	requeue := s.reconcile(passCtx, st, audit)
	if errors.Is(passCtx.Err(), context.DeadlineExceeded) {
		st.drained = false
		logger.Warn("reconcile pass timed out",
			"reconcile.duration", time.Since(started),
			"reconcile.timeout", s.passTimeout,
		)
		return true
	}
	logger.Info("reconcile pass completed",
		"reconcile.duration", time.Since(started),
		"reconcile.requeue", requeue,
	)
	return requeue
}

// HandleDesiredRunnerCount implements [listener.Scaler]. It records
// the desired count and reports the runner count observed by the most
// recent reconcile pass.
func (s *Scaler) HandleDesiredRunnerCount(_ context.Context, count int) (int, error) {
	s.mu.Lock()
	s.desired = max(count, 0)
	s.mu.Unlock()
	s.wakeup()

	return int(s.active.Load()), nil
}

// HandleJobStarted implements [listener.Scaler].
func (s *Scaler) HandleJobStarted(_ context.Context, jobInfo *scaleset.JobStarted) error {
	if !s.ownsRunnerName(jobInfo.RunnerName) {
		s.logger.Warn("ignoring job started for runner not owned by this scaler",
			"job.id", jobInfo.JobID,
			"job.runner_name", jobInfo.RunnerName,
		)
		return nil
	}

	s.logger.Info("job started",
		"job.id", jobInfo.JobID,
		"job.runner_name", jobInfo.RunnerName,
	)
	s.recordJobEvent(jobEvent{runnerName: jobInfo.RunnerName})

	return nil
}

// HandleJobCompleted implements [listener.Scaler].
func (s *Scaler) HandleJobCompleted(_ context.Context, jobInfo *scaleset.JobCompleted) error {
	if !s.ownsRunnerName(jobInfo.RunnerName) {
		s.logger.Warn("ignoring job completed for runner not owned by this scaler",
			"job.id", jobInfo.JobID,
			"job.runner_name", jobInfo.RunnerName,
		)
		return nil
	}

	s.logger.Info("job completed",
		"job.id", jobInfo.JobID,
		"job.runner_name", jobInfo.RunnerName,
		"job.result", jobInfo.Result,
	)
	s.recordJobEvent(jobEvent{runnerName: jobInfo.RunnerName, completed: true})

	return nil
}

func (s *Scaler) recordJobEvent(event jobEvent) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	s.wakeup()
}

func (s *Scaler) wakeup() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ownsRunnerName reports whether a runner name belongs to this scaler.
// Job events for foreign names are ignored so that a stale or
// misrouted message can never delete resources this scaler doesn't
// own.
func (s *Scaler) ownsRunnerName(name string) bool {
	return len(name) > len(s.namePrefix()) &&
		name[:len(s.namePrefix())] == s.namePrefix()
}

// namePrefix returns the name prefix identifying the instances and
// boot disks this scaler owns. Reconciliation adopts and cleans up
// every resource in the project matching it, so the prefix must be
// unique among scalers sharing a project; config validation bounds
// the scale set name so full names stay within Oxide's 63-character
// limit.
func (s *Scaler) namePrefix() string {
	return instanceNamePrefix + s.scaleSetName + "-"
}
