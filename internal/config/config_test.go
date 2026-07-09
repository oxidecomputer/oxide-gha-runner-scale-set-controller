package config

import (
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	cfg, err := Load(strings.NewReader(validConfigYAML))
	requireNoError(t, err)

	if cfg.GitHub.ConfigURL != "https://github.com/oxidecomputer/runner-test" {
		t.Fatalf("unexpected github config URL: %q", cfg.GitHub.ConfigURL)
	}
	if cfg.GitHub.Auth.PAT == nil {
		t.Fatal("expected PAT auth to be configured")
	}
	if cfg.ScaleSet.Name != "linux-x64" {
		t.Fatalf("unexpected scale set name: %q", cfg.ScaleSet.Name)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := Load(strings.NewReader(validConfigYAML + "\nunknown: true\n"))
	requireErrorContains(t, err, "parsing config")
	requireErrorContains(t, err, "field unknown not found")
}

func TestLoadRejectsInstancePrefix(t *testing.T) {
	yaml := strings.Replace(validConfigYAML,
		"  instance:\n",
		"  instance:\n    prefix: custom-prefix\n",
		1,
	)
	_, err := Load(strings.NewReader(yaml))
	requireErrorContains(t, err, "field prefix not found")
}

func TestLoadEnvFallbacksOxide(t *testing.T) {
	t.Setenv("OXIDE_HOST", "https://oxide-env.example.com")
	t.Setenv("OXIDE_TOKEN", "oxide-env-token")

	yaml := strings.ReplaceAll(validConfigYAML,
		"  host: https://oxide.example.com\n  token: oxide-token\n",
		"  host: \"\"\n  token: \"\"\n",
	)
	cfg, err := Load(strings.NewReader(yaml))
	requireNoError(t, err)

	if cfg.Oxide.Host != "https://oxide-env.example.com" {
		t.Fatalf("unexpected oxide host: %q", cfg.Oxide.Host)
	}
	if cfg.Oxide.Token != "oxide-env-token" {
		t.Fatalf("unexpected oxide token: %q", cfg.Oxide.Token)
	}

	cfg, err = Load(strings.NewReader(validConfigYAML))
	requireNoError(t, err)
	if cfg.Oxide.Host != "https://oxide.example.com" {
		t.Fatalf("expected YAML oxide host to win, got %q", cfg.Oxide.Host)
	}
	if cfg.Oxide.Token != "oxide-token" {
		t.Fatalf("expected YAML oxide token to win, got %q", cfg.Oxide.Token)
	}
}

func TestLoadEnvFallbacksGitHubToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_env")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "env-private-key")

	yaml := strings.Replace(validConfigYAML,
		"      token: ghp_token\n",
		"      token: \"\"\n",
		1,
	)
	cfg, err := Load(strings.NewReader(yaml))
	requireNoError(t, err)
	if cfg.GitHub.Auth.PAT == nil || cfg.GitHub.Auth.PAT.Token != "ghp_env" {
		t.Fatalf("expected PAT token from env, got %#v", cfg.GitHub.Auth.PAT)
	}

	yaml = strings.Replace(validConfigYAML,
		"  auth:\n    pat:\n      token: ghp_token\n",
		"",
		1,
	)
	cfg, err = Load(strings.NewReader(yaml))
	requireNoError(t, err)
	if cfg.GitHub.Auth.PAT == nil || cfg.GitHub.Auth.PAT.Token != "ghp_env" {
		t.Fatalf("expected synthesized PAT from env, got %#v", cfg.GitHub.Auth.PAT)
	}

	cfg, err = Load(strings.NewReader(validAppConfigYAML("")))
	requireNoError(t, err)
	if cfg.GitHub.Auth.PAT != nil {
		t.Fatalf("expected GITHUB_TOKEN not to add PAT with App auth")
	}
}

func TestLoadEnvFallbacksGitHubAppPrivateKey(t *testing.T) {
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "env-private-key")

	cfg, err := Load(strings.NewReader(validAppConfigYAML("")))
	requireNoError(t, err)

	if cfg.GitHub.Auth.App == nil {
		t.Fatal("expected App auth to be configured")
	}
	if cfg.GitHub.Auth.App.PrivateKey != "env-private-key" {
		t.Fatalf("unexpected app private key: %q", cfg.GitHub.Auth.App.PrivateKey)
	}
}

func TestLoadEnvFallbacksMissingSecretsStillFail(t *testing.T) {
	t.Setenv("OXIDE_HOST", "")
	t.Setenv("OXIDE_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "")

	yaml := strings.Replace(validConfigYAML,
		"      token: ghp_token\n",
		"      token: \"\"\n",
		1,
	)
	_, err := Load(strings.NewReader(yaml))
	requireErrorContains(t, err, "github.auth.pat.token is required")

	yaml = strings.ReplaceAll(validConfigYAML,
		"  host: https://oxide.example.com\n  token: oxide-token\n",
		"  host: \"\"\n  token: oxide-token\n",
	)
	_, err = Load(strings.NewReader(yaml))
	requireErrorContains(t, err, "oxide.host is required")

	yaml = strings.ReplaceAll(validConfigYAML,
		"  host: https://oxide.example.com\n  token: oxide-token\n",
		"  host: https://oxide.example.com\n  token: \"\"\n",
	)
	_, err = Load(strings.NewReader(yaml))
	requireErrorContains(t, err, "oxide.token is required")

	_, err = Load(strings.NewReader(validAppConfigYAML("")))
	requireErrorContains(t, err, "github.auth.app: app private key is required")
}

func TestLoadPreservesBootDiskGiB(t *testing.T) {
	cfg, err := Load(strings.NewReader(validConfigYAML))
	requireNoError(t, err)

	if got := cfg.ScaleSet.Instance.BootDiskGiB; got != 50 {
		t.Fatalf("expected configured boot disk size, got %d", got)
	}
}

func TestLoadRejectsNegativeBootDiskGiB(t *testing.T) {
	yaml := strings.Replace(validConfigYAML,
		"    boot_disk_gib: 50\n",
		"    boot_disk_gib: -1\n",
		1,
	)
	_, err := Load(strings.NewReader(yaml))
	requireErrorContains(t, err, "cannot unmarshal !!int `-1` into uint")
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "valid minimal config",
		},
		{
			name: "requires github config url",
			mutate: func(c *Config) {
				c.GitHub.ConfigURL = ""
			},
			wantErr: "github.config_url is required",
		},
		{
			name: "requires valid github config url",
			mutate: func(c *Config) {
				c.GitHub.ConfigURL = "://github.com/oxidecomputer"
			},
			wantErr: "github.config_url is invalid",
		},
		{
			name: "requires github auth",
			mutate: func(c *Config) {
				c.GitHub.Auth = Auth{}
			},
			wantErr: "github.auth: one of app or pat is required",
		},
		{
			name: "requires oxide config",
			mutate: func(c *Config) {
				c.Oxide.Host = ""
			},
			wantErr: "oxide.host is required",
		},
		{
			name: "requires a scale set",
			mutate: func(c *Config) {
				c.ScaleSet = ScaleSet{}
			},
			wantErr: "scale_set: name is required",
		},
		{
			name: "rejects negative shutdown timeout",
			mutate: func(c *Config) {
				c.ShutdownTimeout = -time.Second
			},
			wantErr: "shutdown_timeout must be >= 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}

			err := cfg.Validate()
			if tt.wantErr == "" {
				requireNoError(t, err)
				return
			}
			requireErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestShutdownTimeoutDuration(t *testing.T) {
	if got := (&Config{}).ShutdownTimeoutDuration(); got != 30*time.Minute {
		t.Fatalf("expected default shutdown timeout, got %s", got)
	}
	if got := (&Config{
		ShutdownTimeout: 45 * time.Second,
	}).ShutdownTimeoutDuration(); got != 45*time.Second {
		t.Fatalf("expected configured shutdown timeout, got %s", got)
	}
}

func TestLoadParsesShutdownTimeout(t *testing.T) {
	cfg, err := Load(strings.NewReader(
		"shutdown_timeout: 45s\n" + validConfigYAML,
	))
	requireNoError(t, err)

	if cfg.ShutdownTimeout != 45*time.Second {
		t.Fatalf("unexpected shutdown timeout: %s", cfg.ShutdownTimeout)
	}
}

func TestAuthValidate(t *testing.T) {
	validApp := &AppAuth{
		ClientID:       "Iv1.0123456789abcdef",
		InstallationID: 1234,
		PrivateKey:     "private-key",
	}

	tests := []struct {
		name    string
		auth    Auth
		wantErr string
	}{
		{
			name: "accepts pat auth",
			auth: Auth{PAT: &PATAuth{Token: "ghp_token"}},
		},
		{
			name: "accepts app auth",
			auth: Auth{App: validApp},
		},
		{
			name: "rejects both auth methods",
			auth: Auth{
				App: validApp,
				PAT: &PATAuth{Token: "ghp_token"},
			},
			wantErr: "github.auth: set only one of app or pat",
		},
		{
			name:    "rejects no auth methods",
			auth:    Auth{},
			wantErr: "github.auth: one of app or pat is required",
		},
		{
			name:    "rejects missing pat token",
			auth:    Auth{PAT: &PATAuth{}},
			wantErr: "github.auth.pat.token is required",
		},
		{
			name: "rejects missing app client id",
			auth: Auth{App: &AppAuth{
				InstallationID: 1234,
				PrivateKey:     "private-key",
			}},
			wantErr: "github.auth.app: client ID is required",
		},
		{
			name: "rejects missing app installation id",
			auth: Auth{App: &AppAuth{
				ClientID:   "Iv1.0123456789abcdef",
				PrivateKey: "private-key",
			}},
			wantErr: "github.auth.app: app installation ID is required",
		},
		{
			name: "rejects missing app private key",
			auth: Auth{App: &AppAuth{
				ClientID:       "Iv1.0123456789abcdef",
				InstallationID: 1234,
			}},
			wantErr: "github.auth.app: app private key is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.auth.Validate()
			if tt.wantErr == "" {
				requireNoError(t, err)
				return
			}
			requireErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestLogValidate(t *testing.T) {
	tests := []struct {
		name      string
		log       Log
		wantErr   string
		wantLevel slog.Level
		wantText  bool
	}{
		{
			name:      "accepts defaults",
			log:       Log{},
			wantLevel: slog.LevelInfo,
		},
		{
			name:      "accepts valid values",
			log:       Log{Level: "debug", Format: "json"},
			wantLevel: slog.LevelDebug,
		},
		{
			name:      "accepts values case insensitively",
			log:       Log{Level: "WARN", Format: "TEXT"},
			wantLevel: slog.LevelWarn,
			wantText:  true,
		},
		{
			name:    "rejects invalid level",
			log:     Log{Level: "trace"},
			wantErr: `log.level "trace" is invalid`,
		},
		{
			name:    "rejects invalid format",
			log:     Log{Format: "pretty"},
			wantErr: `log.format "pretty" is invalid`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.log.Validate()
			if tt.wantErr == "" {
				requireNoError(t, err)
				if tt.log.level != tt.wantLevel {
					t.Fatalf(
						"expected level %v, got %v",
						tt.wantLevel, tt.log.level,
					)
				}
				if tt.log.text != tt.wantText {
					t.Fatalf(
						"expected text %v, got %v",
						tt.wantText, tt.log.text,
					)
				}
				return
			}
			requireErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestOxideValidate(t *testing.T) {
	tests := []struct {
		name    string
		oxide   Oxide
		wantErr string
	}{
		{
			name:  "accepts valid config",
			oxide: Oxide{Host: "https://oxide.example.com", Token: "oxide-token"},
		},
		{
			name:    "requires host",
			oxide:   Oxide{Token: "oxide-token"},
			wantErr: "oxide.host is required",
		},
		{
			name:    "requires token",
			oxide:   Oxide{Host: "https://oxide.example.com"},
			wantErr: "oxide.token is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.oxide.Validate()
			if tt.wantErr == "" {
				requireNoError(t, err)
				return
			}
			requireErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestScaleSetValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ScaleSet)
		wantErr string
	}{
		{
			name: "accepts valid config",
		},
		{
			name: "requires name",
			mutate: func(s *ScaleSet) {
				s.Name = ""
			},
			wantErr: "name is required",
		},
		{
			name: "rejects empty labels",
			mutate: func(s *ScaleSet) {
				s.Labels = []string{"linux-x64", ""}
			},
			wantErr: "labels[1] is required",
		},
		{
			name: "rejects whitespace labels",
			mutate: func(s *ScaleSet) {
				s.Labels = []string{"\t"}
			},
			wantErr: "labels[0] is required",
		},
		{
			name: "accepts drain mode",
			mutate: func(s *ScaleSet) {
				s.MinRunners = 0
				s.MaxRunners = 0
			},
		},
		{
			name: "accepts maximum runner limit",
			mutate: func(s *ScaleSet) {
				s.MinRunners = 0
				s.MaxRunners = math.MaxInt32
			},
		},
		{
			name: "rejects runners above maximum limit",
			mutate: func(s *ScaleSet) {
				s.MinRunners = 0
				s.MaxRunners = math.MaxInt32 + 1
			},
			wantErr: "max_runners must be <= 2147483647",
		},
		{
			name: "requires minimum runners at most maximum runners",
			mutate: func(s *ScaleSet) {
				s.MinRunners = 2
				s.MaxRunners = 1
			},
			wantErr: "min_runners must be <= max_runners",
		},
		{
			name: "validates instance",
			mutate: func(s *ScaleSet) {
				s.Instance.Project = ""
			},
			wantErr: "instance.project is required",
		},
		{
			name: "accepts name starting with a digit",
			mutate: func(s *ScaleSet) {
				s.Name = "1runner"
			},
		},
		{
			name: "accepts name ending with a hyphen",
			mutate: func(s *ScaleSet) {
				s.Name = "runner-"
			},
		},
		{
			name: "rejects name longer than 35 characters",
			mutate: func(s *ScaleSet) {
				s.Name = strings.Repeat("a", maxScaleSetNameLength+1)
			},
			wantErr: "name",
		},
		{
			name: "accepts name of exactly 35 characters",
			mutate: func(s *ScaleSet) {
				s.Name = strings.Repeat("a", maxScaleSetNameLength)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ss := validScaleSet("linux-x64")
			if tt.mutate != nil {
				tt.mutate(&ss)
			}

			err := ss.Validate()
			if tt.wantErr == "" {
				requireNoError(t, err)
				return
			}
			requireErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestScaleSetRunnerLabels(t *testing.T) {
	ss := validScaleSet("linux-x64")
	ss.Labels = nil
	labels := ss.RunnerLabels()
	if len(labels) != 1 {
		t.Fatalf("expected 1 default label, got %d", len(labels))
	}
	if labels[0].Name != "linux-x64" {
		t.Fatalf("expected default label to be scale set name, got %q", labels[0].Name)
	}

	ss.Labels = []string{" linux-x64 ", "self-hosted"}
	labels = ss.RunnerLabels()
	if len(labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(labels))
	}
	if labels[0].Name != "linux-x64" {
		t.Fatalf("expected first label to be trimmed, got %q", labels[0].Name)
	}
	if labels[1].Name != "self-hosted" {
		t.Fatalf("unexpected second label: %q", labels[1].Name)
	}
}

func TestInstanceValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Instance)
		wantErr string
	}{
		{
			name: "accepts valid config",
		},
		{
			name: "requires project",
			mutate: func(i *Instance) {
				i.Project = ""
			},
			wantErr: "instance.project is required",
		},
		{
			name: "requires vpc",
			mutate: func(i *Instance) {
				i.VPC = ""
			},
			wantErr: "instance.vpc is required",
		},
		{
			name: "requires subnet",
			mutate: func(i *Instance) {
				i.Subnet = ""
			},
			wantErr: "instance.subnet is required",
		},
		{
			name: "requires image",
			mutate: func(i *Instance) {
				i.Image = ""
			},
			wantErr: "instance.image is required",
		},
		{
			name: "requires positive cpus",
			mutate: func(i *Instance) {
				i.CPUs = 0
			},
			wantErr: "instance.cpus must be > 0",
		},
		{
			name: "requires positive memory",
			mutate: func(i *Instance) {
				i.MemoryGiB = 0
			},
			wantErr: "instance.memory_gib must be > 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := validInstance()
			if tt.mutate != nil {
				tt.mutate(&instance)
			}

			err := instance.Validate()
			if tt.wantErr == "" {
				requireNoError(t, err)
				return
			}
			requireErrorContains(t, err, tt.wantErr)
		})
	}
}

func validConfig() Config {
	return Config{
		GitHub: GitHub{
			ConfigURL: "https://github.com/oxidecomputer/runner-test",
			Auth: Auth{
				PAT: &PATAuth{Token: "ghp_token"},
			},
		},
		Oxide: Oxide{
			Host:  "https://oxide.example.com",
			Token: "oxide-token",
		},
		ScaleSet: validScaleSet("linux-x64"),
	}
}

func validScaleSet(name string) ScaleSet {
	return ScaleSet{
		Name:       name,
		Labels:     []string{name, "self-hosted"},
		MinRunners: 1,
		MaxRunners: 2,
		Instance:   validInstance(),
	}
}

func validInstance() Instance {
	return Instance{
		Project:     "actions-runners",
		Image:       "ubuntu-22.04",
		BootDiskGiB: 50,
		CPUs:        2,
		MemoryGiB:   8,
		VPC:         "default",
		Subnet:      "default",
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected error containing %q, got %q", want, err.Error())
	}
}

func validAppConfigYAML(privateKey string) string {
	return strings.Replace(validConfigYAML,
		"  auth:\n    pat:\n      token: ghp_token\n",
		"  auth:\n    app:\n"+
			"      client_id: Iv1.0123456789abcdef\n"+
			"      installation_id: 1234\n"+
			"      private_key: \""+privateKey+"\"\n",
		1,
	)
}

const validConfigYAML = `
github:
  config_url: https://github.com/oxidecomputer/runner-test
  auth:
    pat:
      token: ghp_token
oxide:
  host: https://oxide.example.com
  token: oxide-token
scale_set:
  name: linux-x64
  labels:
    - linux-x64
    - self-hosted
  min_runners: 1
  max_runners: 2
  instance:
    project: actions-runners
    vpc: default
    subnet: default
    image: ubuntu-22.04
    cpus: 2
    memory_gib: 8
    boot_disk_gib: 50
`
