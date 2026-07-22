// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import "runtime/debug"

const applicationName = "oxide-gha-runner-scale-set-controller"

// version and commit are populated by release builds using linker flags.
var (
	version string
	commit  string
)

type buildInfo struct {
	version string
	commit  string
}

func getBuildInfo() buildInfo {
	buildVersion := version
	buildCommit := commit
	info, ok := debug.ReadBuildInfo()
	if ok {
		if buildVersion == "" {
			buildVersion = info.Main.Version
		}
		if buildCommit == "" {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					buildCommit = setting.Value
					break
				}
			}
		}
	}

	if buildVersion == "" {
		buildVersion = "unknown"
	}
	if buildCommit == "" {
		buildCommit = "unknown"
	}

	return buildInfo{
		version: buildVersion,
		commit:  buildCommit,
	}
}
