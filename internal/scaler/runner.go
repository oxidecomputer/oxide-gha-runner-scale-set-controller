// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"time"

	"github.com/oxidecomputer/oxide.go/oxide"
)

// runner is the unified view of a single GitHub Actions runner. It holds the
// Oxide instance and boot disk, whether GitHub assigned a job to the runner,
// and the teardown state for the runner. All of this state is rebuilt from
// Oxide and GitHub during reconciliation.
type runner struct {
	// name is the GitHub Actions runner name. It is also the name of the Oxide
	// instance, boot disk, and network interface. With no resource tags on Oxide,
	// the name is the only way to join all the resources together.
	name string

	// instance is the Oxide instance for the runner, or nil when none exists.
	instance *oxide.Instance

	// disk is the Oxide boot disk for the runner, or nil when none exists.
	disk *oxide.Disk

	// busy records whether GitHub assigned a job to this runner.
	busy bool

	// teardown is the teardown state of the runner, or nil when the runner is not
	// being torn down.
	teardown *teardownState
}

// active reports whether the runner counts towards scale set demand.
func (r *runner) active() bool {
	return r.instance != nil &&
		!instanceHalted(r.instance.RunState) &&
		r.teardown == nil
}

// gone reports whether nothing remains of the runner.
func (r *runner) gone() bool {
	return r.instance == nil && r.disk == nil && r.teardown == nil
}

// instanceHalted reports whether an instance is in a state where it will
// never run a job again.
func instanceHalted(state oxide.InstanceState) bool {
	switch state {
	case oxide.InstanceStateStopped,
		oxide.InstanceStateFailed,
		oxide.InstanceStateDestroyed:
		return true
	}
	return false
}

// reconcileState is the in-memory state that persists across reconciliations.
// It's rebuilt on restart by querying GitHub and Oxide during reconciliation.
type reconcileState struct {
	// desiredRunnerCount is the demand most recently reported by GitHub, or -1
	// before the first report. Scaling is suspended until demand is known.
	desiredRunnerCount int

	// runners is the unified view of every runner known to the scaler, keyed by
	// runner name.
	runners map[string]*runner

	// lastGitHubAudit is when GitHub runner registrations were last reconciled
	// against Oxide instances.
	lastGitHubAudit time.Time
}

func newReconcileState() *reconcileState {
	return &reconcileState{
		desiredRunnerCount: -1,
		runners:            make(map[string]*runner),
	}
}

// runner returns the tracked runner with the given name, creating an empty
// entry when the name is unknown.
func (s *reconcileState) runner(name string) *runner {
	r, ok := s.runners[name]
	if !ok {
		r = &runner{name: name}
		s.runners[name] = r
	}
	return r
}

// activeRunnerCount counts runners that can accept or run a job.
func (s *reconcileState) activeRunnerCount() int {
	active := 0
	for _, runner := range s.runners {
		if runner.active() {
			active++
		}
	}
	return active
}

// instanceCount counts runners backed by an Oxide instance, including halted
// instances and instances marked for teardown because they still consume Oxide
// capacity.
func (s *reconcileState) instanceCount() int {
	count := 0
	for _, runner := range s.runners {
		if runner.instance != nil {
			count++
		}
	}
	return count
}

// teardownCount counts runners with a teardown in progress.
func (s *reconcileState) teardownCount() int {
	count := 0
	for _, runner := range s.runners {
		if runner.teardown != nil {
			count++
		}
	}
	return count
}

// prune drops runners that are gone so the runner view tracks only live
// state and a drained scale set becomes detectable.
func (s *reconcileState) prune() {
	for name, runner := range s.runners {
		if runner.gone() {
			delete(s.runners, name)
		}
	}
}
