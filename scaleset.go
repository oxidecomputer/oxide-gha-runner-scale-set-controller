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

// ensureScaleSet reconciles the configured runner scale set. An existing
// scale set is adopted by name within the configured runner group, with
// labels updated when needed. A missing scale set is created.
func (c *controller) ensureScaleSet(
	ctx context.Context,
) (*scaleset.RunnerScaleSet, error) {
	logger := c.logger
	scalesetClient := c.scaleSetClient
	ss := c.config.ScaleSet

	// Fetch the runner group ID and name.
	runnerGroupID := defaultRunnerGroupID
	runnerGroupName := scaleset.DefaultRunnerGroup
	if ss.RunnerGroup != "" &&
		ss.RunnerGroup != scaleset.DefaultRunnerGroup {
		group, err := scalesetClient.GetRunnerGroupByName(ctx, ss.RunnerGroup)
		if err != nil {
			return nil, fmt.Errorf(
				"scale set %q: resolving runner group %q: %w",
				ss.Name, ss.RunnerGroup, err,
			)
		}
		runnerGroupID = group.ID
		runnerGroupName = group.Name
	}

	// Check for an existing scale set. [scaleset.Client.GetRunnerScaleSet] returns
	// (nil, nil) when a scale set doesn't exist.
	existing, err := scalesetClient.GetRunnerScaleSet(
		ctx, runnerGroupID, ss.Name,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"scale set %q: checking existence: %w", ss.Name, err,
		)
	}
	if existing != nil {
		desiredLabels := ss.RunnerLabels()
		if !sameLabelNames(desiredLabels, existing.Labels) {
			updated, err := scalesetClient.UpdateRunnerScaleSet(
				ctx,
				existing.ID,
				&scaleset.RunnerScaleSet{
					Labels: desiredLabels,
				},
			)
			if err != nil {
				return nil, fmt.Errorf(
					"scale set %q: updating labels: %w", ss.Name, err,
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

	created, err := scalesetClient.CreateRunnerScaleSet(
		ctx,
		&scaleset.RunnerScaleSet{
			Name:          ss.Name,
			RunnerGroupID: runnerGroupID,
			Labels:        ss.RunnerLabels(),
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"scale set %q: creating: %w", ss.Name, err,
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
