// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package scaler

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/oxidecomputer/oxide.go/oxide"
)

// Invariant: the configured image may be a project image or a silo image.
// Only a definitive not-found answer triggers the silo fallback.

func TestFetchImageFallsBackToSiloImage(t *testing.T) {
	scaler, oxideClient, _ := newTestScaler(t)
	oxideClient.projectImageErr = oxide.ErrObjectNotFound

	image, err := scaler.fetchImage(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if image == nil {
		t.Fatal("expected the silo image")
	}
	calls := oxideClient.imageViewCalls
	if len(calls) != 2 {
		t.Fatalf("expected 2 image lookups, got %d", len(calls))
	}
	if calls[0].Project == "" {
		t.Fatal("expected the first lookup to be project-scoped")
	}
	if calls[1].Project != "" {
		t.Fatal("expected the fallback lookup to be silo-scoped")
	}
}

// Invariant: the boot disk always fits the image. The configured size only
// takes effect when it is at least as large as the image, and a zero
// configured size means size the disk to the image, rounded up to a
// whole GiB.

func TestCreateInstanceBootDiskSizing(t *testing.T) {
	const gib = oxide.ByteCount(1024 * 1024 * 1024)
	tests := []struct {
		name      string
		configGiB uint
		imageSize oxide.ByteCount
		wantSize  oxide.ByteCount
	}{
		{"zero config sizes to image", 0, 4 * gib, 4 * gib},
		{"partial GiB images round up", 0, 4*gib + 1, 5 * gib},
		{"config larger than image wins", 50, 1 * gib, 50 * gib},
		{"image larger than config wins", 2, 4 * gib, 4 * gib},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, oxideClient, _ := newTestScaler(t)
			scaler.instanceConfig.BootDiskGiB = tt.configGiB
			oxideClient.imageSize = tt.imageSize
			state := newReconcileState()

			deadline := scaler.scaleUp(
				context.Background(), state, 0, 1,
			)
			expectNoDeadline(t, deadline)

			if got := len(oxideClient.createCalls); got != 1 {
				t.Fatalf("expected 1 instance create, got %d", got)
			}
			bootDisk, ok := oxideClient.createCalls[0].Body.BootDisk.
				Value.(oxide.InstanceDiskAttachmentCreate)
			if !ok {
				t.Fatal("expected the boot disk to be created")
			}
			if bootDisk.Size != tt.wantSize {
				t.Fatalf("expected boot disk of %d bytes, got %d",
					tt.wantSize, bootDisk.Size)
			}
		})
	}
}

// Invariant: instances are one-shot job runners built entirely from the
// configuration. They carry the runner's name on every resource, boot with
// the JIT config in their user data, start immediately, and never restart.

func TestCreateInstanceParams(t *testing.T) {
	scaler, oxideClient, scaleSetClient := newTestScaler(t)
	config := testConfig()
	state := newReconcileState()

	deadline := scaler.scaleUp(context.Background(), state, 0, 1)
	expectNoDeadline(t, deadline)
	if got := len(oxideClient.createCalls); got != 1 {
		t.Fatalf("expected 1 instance create, got %d", got)
	}

	params := oxideClient.createCalls[0]
	if params.Project != oxide.NameOrId(config.Instance.Project) {
		t.Fatalf("expected project %q, got %q",
			config.Instance.Project, params.Project)
	}

	body := params.Body
	name := string(body.Name)
	if !strings.HasPrefix(name, scaler.namePrefix()) {
		t.Fatalf("expected instance name %q to carry the owned prefix",
			name)
	}
	if string(body.Hostname) != name {
		t.Fatalf("expected hostname %q to match the instance name %q",
			body.Hostname, name)
	}
	jitNames := scaleSetClient.jitRunnerNames()
	if len(jitNames) != 1 || jitNames[0] != name {
		t.Fatalf("expected the GitHub runner to be named %q, got %v",
			name, jitNames)
	}
	if got := uint(body.Ncpus); got != config.Instance.CPUs {
		t.Fatalf("expected %d CPUs, got %d", config.Instance.CPUs, got)
	}
	wantMemory := oxide.ByteCount(config.Instance.MemoryGiB *
		1024 * 1024 * 1024)
	if body.Memory != wantMemory {
		t.Fatalf("expected %d bytes of memory, got %d",
			wantMemory, body.Memory)
	}
	if body.AutoRestartPolicy != oxide.InstanceAutoRestartPolicyNever {
		t.Fatalf("expected auto restart never, got %q",
			body.AutoRestartPolicy)
	}
	if body.Start == nil || !*body.Start {
		t.Fatal("expected the instance to start immediately")
	}

	bootDisk, ok := body.BootDisk.
		Value.(oxide.InstanceDiskAttachmentCreate)
	if !ok {
		t.Fatal("expected the boot disk to be created")
	}
	if string(bootDisk.Name) != name {
		t.Fatalf("expected boot disk name %q to match the instance name %q",
			bootDisk.Name, name)
	}

	nics, ok := body.NetworkInterfaces.
		Value.(oxide.InstanceNetworkInterfaceAttachmentCreate)
	if !ok || len(nics.Params) != 1 {
		t.Fatal("expected one network interface to be created")
	}
	nic := nics.Params[0]
	if string(nic.Name) != name {
		t.Fatalf("expected NIC name %q to match the instance name %q",
			nic.Name, name)
	}
	if string(nic.VpcName) != config.Instance.VPC {
		t.Fatalf("expected VPC %q, got %q", config.Instance.VPC,
			nic.VpcName)
	}
	if string(nic.SubnetName) != config.Instance.Subnet {
		t.Fatalf("expected subnet %q, got %q", config.Instance.Subnet,
			nic.SubnetName)
	}

	userData, err := base64.StdEncoding.DecodeString(body.UserData)
	if err != nil {
		t.Fatalf("expected base64 user data: %v", err)
	}
	if !strings.Contains(string(userData), "encoded-jit-config") {
		t.Fatal("expected user data to contain the JIT config")
	}
}

func TestUserDataTemplateRendersRunnerInputs(t *testing.T) {
	var userData strings.Builder
	err := userDataTemplate.Execute(&userData, struct {
		JITConfig     string
		RunnerVersion string
		RunnerSHA256  string
	}{
		JITConfig:     "encoded-jit-config",
		RunnerVersion: "2.999.1",
		RunnerSHA256:  strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatalf("rendering user data: %v", err)
	}

	rendered := userData.String()
	for _, want := range []string{
		"encoded-jit-config",
		`RUNNER_VERSION="2.999.1"`,
		`RUNNER_SHA256="` + strings.Repeat("a", 64) + `"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("expected rendered user data to contain %q", want)
		}
	}
}
