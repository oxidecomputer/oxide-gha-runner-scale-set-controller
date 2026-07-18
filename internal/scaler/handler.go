// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"context"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

var _ listener.Scaler = (*Scaler)(nil)

// HandleDesiredRunnerCount handles messages from GitHub that request a
// desired number of runners. Since [Scaler] uses a reconciliation pattern,
// this method simply records the requested number of runners and triggers the
// reconciliation loop to wake up and run.
//
// The returned count is the number of active runners observed by the most
// recent reconciliation. The listener records it as a metric only. It is never
// reported back to GitHub.
func (s *Scaler) HandleDesiredRunnerCount(
	_ context.Context,
	count int,
) (int, error) {
	s.logger.Info("desired runner count received",
		"runner.count.desired", count,
		"runner.count.active", s.activeRunnerCount.Load(),
	)
	s.mu.Lock()
	s.desiredRunnerCount = count
	s.mu.Unlock()
	s.wake()

	return int(s.activeRunnerCount.Load()), nil
}

// HandleJobStarted handles messages from GitHub that signal when a job
// has started on a specific runner in the scale set. Since [Scaler] uses a
// reconciliation pattern, this method simply records the job started event and
// triggers the reconciliation loop to wake up and run.
func (s *Scaler) HandleJobStarted(
	_ context.Context,
	jobInfo *scaleset.JobStarted,
) error {
	s.logger.Info("job started",
		"job.id", jobInfo.JobID,
		"runner.name", jobInfo.RunnerName,
	)

	if jobInfo.RunnerName == "" {
		s.logger.Warn("ignoring job started event without a runner name",
			"job.id", jobInfo.JobID,
		)
		return nil
	}

	s.enqueueJobEvent(jobEvent{
		runnerName: jobInfo.RunnerName,
		jobStatus:  jobStatusStarted,
	})

	return nil
}

// HandleJobCompleted handles messages from GitHub that signal when a job
// has completed on a specific runner in the scale set. Since [Scaler] uses a
// reconciliation pattern, this method simply records the job completed event
// and triggers the reconciliation loop to wake up and run.
func (s *Scaler) HandleJobCompleted(
	_ context.Context,
	jobInfo *scaleset.JobCompleted,
) error {
	s.logger.Info("job completed",
		"job.id", jobInfo.JobID,
		"runner.name", jobInfo.RunnerName,
	)

	if jobInfo.RunnerName == "" {
		s.logger.Warn("ignoring job completed event without a runner name",
			"job.id", jobInfo.JobID,
		)
		return nil
	}

	s.enqueueJobEvent(jobEvent{
		runnerName: jobInfo.RunnerName,
		jobStatus:  jobStatusCompleted,
	})

	return nil
}

// enqueueJobEvent appends a job event to the inbox and wakes the
// reconciliation loop.
func (s *Scaler) enqueueJobEvent(event jobEvent) {
	s.mu.Lock()
	s.jobEvents = append(s.jobEvents, event)
	s.mu.Unlock()
	s.wake()
}

// jobEvent holds the details of a job event received from GitHub.
type jobEvent struct {
	runnerName string
	jobStatus  jobStatus
}

// jobStatus represents the various job statuses.
type jobStatus string

const (
	jobStatusStarted   jobStatus = "started"
	jobStatusCompleted jobStatus = "completed"
)

// processJobEvents drains the job event inbox and applies each event to the
// unified runner view.
func (s *Scaler) processJobEvents(state *reconcileState) {
	// Hold onto the current demand and empty the job events inbox.
	s.mu.Lock()
	state.desiredRunnerCount = s.desiredRunnerCount
	jobEvents := s.jobEvents
	s.jobEvents = nil
	s.mu.Unlock()

	for _, event := range jobEvents {
		switch event.jobStatus {
		case jobStatusStarted:
			runner := state.runner(event.runnerName)
			runner.busy = true

			// A job may start after its runner was marked with a cancelable teardown.
			// Cancel that teardown and let the job run.
			switch {
			case runner.teardown.canCancelForJob():
				runner.teardown = nil
				s.logger.Info("scale down canceled; job started",
					"runner.name", runner.name,
				)
			case runner.teardown != nil &&
				runner.teardown.phase == teardownPhaseDeprovisioning:
				s.logger.Info(
					"scale down can no longer be canceled; registration removed",
					"runner.name", runner.name,
				)
			}
		case jobStatusCompleted:
			s.markForTeardown(
				state.runner(event.runnerName),
				teardownPolicyPermanent,
				"job completed",
			)
		default:
			s.logger.Warn("ignoring job event with unknown status",
				"job.status", event.jobStatus,
				"runner.name", event.runnerName,
			)
		}
	}
}

// wake triggers the reconciliation loop to wake up and execute.
func (s *Scaler) wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}
