// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/actions/scaleset"
)

// defaultRunnerGroupID is the ID of the default runner group, which always
// exists at every scope (i.e., repository, organization, enterprise).
const defaultRunnerGroupID = 1

// ensureScaleSet adopts the configured runner scale set by name and runner
// group, reconciling its labels. It creates the scale set when none exists.
func (c *controller) ensureScaleSet(
	ctx context.Context,
) (*scaleset.RunnerScaleSet, error) {
	logger := c.logger
	scaleSetClient := c.scaleSetClient
	scaleSetConfig := c.config.ScaleSet

	runnerGroupID := defaultRunnerGroupID
	runnerGroupName := scaleset.DefaultRunnerGroup
	if scaleSetConfig.RunnerGroup != "" &&
		scaleSetConfig.RunnerGroup != scaleset.DefaultRunnerGroup {
		group, err := scaleSetClient.GetRunnerGroupByName(
			ctx, scaleSetConfig.RunnerGroup,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"scale set %q: resolving runner group %q: %w",
				scaleSetConfig.Name, scaleSetConfig.RunnerGroup, err,
			)
		}
		runnerGroupID = group.ID
		runnerGroupName = group.Name
	}

	existing, err := scaleSetClient.GetRunnerScaleSet(
		ctx, runnerGroupID, scaleSetConfig.Name,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"scale set %q: checking existence: %w", scaleSetConfig.Name, err,
		)
	}
	if existing != nil {
		desiredLabels := scaleSetConfig.RunnerLabels()
		if !sameLabelNames(desiredLabels, existing.Labels) {
			updated, err := scaleSetClient.UpdateRunnerScaleSet(
				ctx,
				existing.ID,
				&scaleset.RunnerScaleSet{
					Labels: desiredLabels,
				},
			)
			if err != nil {
				return nil, fmt.Errorf(
					"scale set %q: updating labels: %w",
					scaleSetConfig.Name, err,
				)
			}

			logger.Info("scale set labels updated",
				"scale_set.name", updated.Name,
				"scale_set.id", updated.ID,
				"runner_group.name", runnerGroupName,
				"runner_group.id", runnerGroupID,
				"labels.previous", labelNames(existing.Labels),
				"labels.updated", labelNames(updated.Labels),
			)
			return updated, nil
		}

		logger.Info("scale set adopted",
			"scale_set.name", existing.Name,
			"scale_set.id", existing.ID,
			"runner_group.name", runnerGroupName,
			"runner_group.id", runnerGroupID,
		)
		return existing, nil
	}

	created, err := scaleSetClient.CreateRunnerScaleSet(
		ctx,
		&scaleset.RunnerScaleSet{
			Name:          scaleSetConfig.Name,
			RunnerGroupID: runnerGroupID,
			Labels:        scaleSetConfig.RunnerLabels(),
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"scale set %q: creating: %w", scaleSetConfig.Name, err,
		)
	}

	logger.Info("scale set created",
		"scale_set.name", created.Name,
		"scale_set.id", created.ID,
		"runner_group.name", runnerGroupName,
		"runner_group.id", runnerGroupID,
	)
	return created, nil
}

func sameLabelNames(a, b []scaleset.Label) bool {
	aNames := labelNames(a)
	bNames := labelNames(b)
	slices.Sort(aNames)
	slices.Sort(bNames)
	return slices.Equal(aNames, bNames)
}

func labelNames(labels []scaleset.Label) []string {
	names := make([]string, len(labels))
	for i, label := range labels {
		names[i] = label.Name
	}
	return names
}
