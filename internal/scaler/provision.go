// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"text/template"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide.go/oxide"
)

//go:embed userdata.sh.tmpl
var userDataTemplateText string

// userDataTemplate renders cloud-init user data containing the JIT config.
var userDataTemplate = template.Must(
	template.New("userdata").Parse(userDataTemplateText),
)

// provisionConcurrency bounds how many runners are provisioned at once. Each
// provision is a GitHub JIT config request followed by an Oxide instance
// create, so this bounds concurrent load on both APIs while keeping burst
// scale-up latency close to that of a single provision.
const provisionConcurrency = 5

// provisionRunners provisions count runners concurrently, bounded by
// [provisionConcurrency]. Failed attempts are marked for permanent teardown.
// Successful runner names are returned.
func (s *Scaler) provisionRunners(
	ctx context.Context,
	state *reconcileState,
	image *oxide.Image,
	count int,
) []string {
	runners := make([]*runner, count)
	errs := make([]error, count)
	semaphore := make(chan struct{}, provisionConcurrency)
	var wg sync.WaitGroup

	for i := range runners {
		runners[i] = &runner{name: s.newRunnerName()}
		wg.Go(func() {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			errs[i] = s.provisionRunner(ctx, image, runners[i])
		})
	}
	wg.Wait()

	created := make([]string, 0, count)
	for i, runner := range runners {
		tracked := state.runner(runner.name)
		if errs[i] != nil {
			s.logger.Error("provisioning runner failed",
				"runner.name", runner.name,
				"error", errs[i],
			)
			s.markForTeardown(
				tracked,
				teardownPolicyPermanent,
				"runner provisioning failed",
			)
			continue
		}

		tracked.instance = runner.instance
		created = append(created, runner.name)
	}

	return created
}

// provisionRunner registers and provisions runner. It mutates only the supplied
// runner, allowing independent runners to be provisioned concurrently.
func (s *Scaler) provisionRunner(
	ctx context.Context,
	image *oxide.Image,
	runner *runner,
) error {
	jitConfig, err := s.scalesetClient.GenerateJitRunnerConfig(
		ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{
			Name: runner.name,
		},
		s.scaleSet.ID,
	)
	if err != nil {
		return fmt.Errorf("generating jit config: %w", err)
	}

	instance, err := s.createInstance(ctx, runner.name, image, jitConfig)
	if err != nil {
		return fmt.Errorf("creating instance: %w", err)
	}

	runner.instance = instance
	return nil
}

// fetchImage resolves the configured image, checking for a project image first
// and falling back to a silo image when the project has none.
func (s *Scaler) fetchImage(ctx context.Context) (*oxide.Image, error) {
	image, err := s.oxideClient.ImageView(ctx, oxide.ImageViewParams{
		Image:   oxide.NameOrId(s.instanceConfig.Image),
		Project: oxide.NameOrId(s.instanceConfig.Project),
	})
	if err != nil && errors.Is(err, oxide.ErrObjectNotFound) {
		// Not a project image; fall back to a silo image.
		image, err = s.oxideClient.ImageView(ctx, oxide.ImageViewParams{
			Image: oxide.NameOrId(s.instanceConfig.Image),
		})
	}

	if err != nil {
		return nil, err
	}

	return image, nil
}

// createInstance creates the Oxide instance for the runner.
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
	bootDiskGiB := max(oxide.ByteCount(s.instanceConfig.BootDiskGiB), imageGiB)
	description := fmt.Sprintf(
		"GitHub Actions runner managed by scale set ID %d within %s.",
		s.scaleSet.ID,
		s.scaleSet.Namespace,
	)

	instance, err := s.oxideClient.InstanceCreate(ctx, oxide.InstanceCreateParams{
		Project: oxide.NameOrId(s.instanceConfig.Project),
		Body: &oxide.InstanceCreate{
			AutoRestartPolicy: oxide.InstanceAutoRestartPolicyNever,
			BootDisk: oxide.InstanceDiskAttachment{
				Value: oxide.InstanceDiskAttachmentCreate{
					Name:        oxide.Name(name),
					Description: description,
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
			Description: description,
			Hostname:    oxide.Hostname(name),
			Memory:      oxide.ByteCount(s.instanceConfig.MemoryGiB * 1024 * 1024 * 1024),
			Name:        oxide.Name(name),
			Ncpus:       oxide.InstanceCpuCount(s.instanceConfig.CPUs),
			NetworkInterfaces: oxide.InstanceNetworkInterfaceAttachment{
				Value: oxide.InstanceNetworkInterfaceAttachmentCreate{
					Params: []oxide.InstanceNetworkInterfaceCreate{
						{
							Name:        oxide.Name(name),
							Description: description,
							IpConfig: oxide.PrivateIpStackCreate{
								Value: oxide.PrivateIpStackCreateDualStack{
									Value: oxide.PrivateIpStackCreateDualStackValue{
										V4: oxide.PrivateIpv4StackCreate{
											Ip: oxide.Ipv4Assignment{
												Value: oxide.Ipv4AssignmentAuto{},
											},
										},
										V6: oxide.PrivateIpv6StackCreate{
											Ip: oxide.Ipv6Assignment{
												Value: oxide.Ipv6AssignmentAuto{},
											},
										},
									},
								},
							},
							SubnetName: oxide.Name(s.instanceConfig.Subnet),
							VpcName:    oxide.Name(s.instanceConfig.VPC),
						},
					},
				},
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
