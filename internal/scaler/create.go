package scaler

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"text/template"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide.go/oxide"
)

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
func (s *Scaler) scaleUp(ctx context.Context, st *state, count int) (created int, retry bool) {
	if count <= 0 || s.stopping() {
		return 0, false
	}

	image, err := s.fetchImage(ctx)
	if err != nil {
		s.logger.Error("fetching image failed", "error", err)
		return 0, true
	}

	for range count {
		if s.stopping() {
			return created, false
		}

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
			// The registration has no instance behind it; teardown
			// removes it. If the create actually committed despite the
			// error, teardown deletes the instance too.
			s.retire(st, name, retiredCreateFailed)
			return created, true
		}

		st.discovered[name] = true
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
