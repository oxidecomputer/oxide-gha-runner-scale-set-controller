// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package config

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/actions/scaleset"
)

func TestLoad(t *testing.T) {
	cfg, err := Load(strings.NewReader(validConfigYAML))
	requireNoError(t, err)

	if cfg.Log.Level != "debug" || cfg.Log.Format != "text" {
		t.Fatalf("unexpected log config: %#v", cfg.Log)
	}
	if cfg.GitHub.ConfigURL != "https://github.com/oxidecomputer/runner-test" {
		t.Fatalf("unexpected github config URL: %q", cfg.GitHub.ConfigURL)
	}
	if cfg.GitHub.Auth.PAT == nil {
		t.Fatal("expected PAT auth to be configured")
	}
	if cfg.ScaleSet.Name != "linux-x64" {
		t.Fatalf("unexpected scale set name: %q", cfg.ScaleSet.Name)
	}
	if cfg.ScaleSet.RunnerGroup != "oxide-runners" {
		t.Fatalf("unexpected runner group: %q", cfg.ScaleSet.RunnerGroup)
	}
	if got := cfg.ScaleSet.Labels; len(got) != 2 ||
		got[0] != "linux-x64" || got[1] != "self-hosted" {
		t.Fatalf("unexpected runner labels: %v", got)
	}
	if cfg.ScaleSet.MinRunners != 1 || cfg.ScaleSet.MaxRunners != 2 {
		t.Fatalf(
			"unexpected runner limits: min=%d max=%d",
			cfg.ScaleSet.MinRunners, cfg.ScaleSet.MaxRunners,
		)
	}
	if cfg.ScaleSet.Runner.Version != "2.335.1" {
		t.Fatalf("unexpected runner version: %q", cfg.ScaleSet.Runner.Version)
	}
	if cfg.ScaleSet.Runner.SHA256 !=
		"4ef2f25285f0ae4477f1fe1e346db76d2f3ebf03824e2ddd1973a2819bf6c8cf" {
		t.Fatalf("unexpected runner SHA-256: %q", cfg.ScaleSet.Runner.SHA256)
	}
	if cfg.ScaleSet.Instance != validInstance() {
		t.Fatalf("unexpected instance config: %#v", cfg.ScaleSet.Instance)
	}
}

func TestLoadCanonicalizesGitHubConfigURL(t *testing.T) {
	yaml := strings.Replace(
		validConfigYAML,
		"https://github.com/oxidecomputer/runner-test",
		"HTTPS://www.github.com:443/OxideComputer/Runner-Test/",
		1,
	)
	cfg, err := Load(strings.NewReader(yaml))
	requireNoError(t, err)

	want := "https://github.com/oxidecomputer/runner-test"
	if cfg.GitHub.ConfigURL != want {
		t.Fatalf("expected GitHub config URL %q, got %q", want, cfg.GitHub.ConfigURL)
	}
}

func TestCanonicalGitHubScope(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{
			name:  "organization",
			input: "https://github.com/OxideComputer",
			want:  "https://github.com/oxidecomputer",
		},
		{
			name:  "repository",
			input: "https://github.com/OxideComputer/Runner-Test",
			want:  "https://github.com/oxidecomputer/runner-test",
		},
		{
			name:  "enterprise",
			input: "https://github.com/enterprises/Oxide",
			want:  "https://github.com/enterprises/oxide",
		},
		{
			name:  "GHES with non-default port",
			input: "https://ghes.example.com:8443/Org/Repo",
			want:  "https://ghes.example.com:8443/org/repo",
		},
		{
			name:    "rejects too many path segments",
			input:   "https://github.com/org/repo/extra",
			wantErr: "must identify a GitHub enterprise, organization, or repository",
		},
		{
			name:    "rejects empty interior path segment",
			input:   "https://github.com/org//repo",
			wantErr: "must identify a GitHub enterprise, organization, or repository",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := canonicalGitHubScope(tt.input)
			if tt.wantErr != "" {
				requireErrorContains(t, err, tt.wantErr)
				return
			}
			requireNoError(t, err)
			if got != tt.want {
				t.Fatalf("expected canonical scope %q, got %q", tt.want, got)
			}
		})
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := Load(strings.NewReader(validConfigYAML + "\nunknown: true\n"))
	requireErrorContains(t, err, "parsing config")
	requireErrorContains(t, err, "field unknown not found")
}

func TestLoadRejectsMultipleYAMLDocuments(t *testing.T) {
	_, err := Load(strings.NewReader(validConfigYAML + "\n---\n{}\n"))
	requireErrorContains(t, err, "multiple YAML documents are not supported")
}

func TestLoadAcceptsExplicitYAMLDocumentStart(t *testing.T) {
	_, err := Load(strings.NewReader("---\n" + validConfigYAML))
	requireNoError(t, err)
}

func TestLoadFile(t *testing.T) {
	t.Run("loads config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(validConfigYAML), 0o600); err != nil {
			t.Fatalf("writing config: %v", err)
		}

		cfg, err := LoadFile(path)
		requireNoError(t, err)
		if cfg.ScaleSet.Name != "linux-x64" {
			t.Fatalf("unexpected scale set name: %q", cfg.ScaleSet.Name)
		}
	})

	t.Run("reports open failure", func(t *testing.T) {
		_, err := LoadFile(filepath.Join(t.TempDir(), "missing.yaml"))
		requireErrorContains(t, err, "opening config")
	})
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
	t.Setenv("OXIDE_TOKEN", " oxide-env-token\n")

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
	t.Setenv("GITHUB_TOKEN", " ghp_env\n")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "env-private-key")

	cfg, err := Load(strings.NewReader(validConfigYAML))
	requireNoError(t, err)
	if cfg.GitHub.Auth.PAT == nil ||
		cfg.GitHub.Auth.PAT.Token != "ghp_token" {
		t.Fatalf("expected YAML PAT to win, got %#v", cfg.GitHub.Auth.PAT)
	}

	yaml := strings.Replace(validConfigYAML,
		"      token: ghp_token\n",
		"      token: \"\"\n",
		1,
	)
	cfg, err = Load(strings.NewReader(yaml))
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

	cfg, err := Load(strings.NewReader(validAppConfigYAML("yaml-private-key")))
	requireNoError(t, err)
	if cfg.GitHub.Auth.App == nil ||
		cfg.GitHub.Auth.App.PrivateKey != "yaml-private-key" {
		t.Fatalf("expected YAML private key to win, got %#v", cfg.GitHub.Auth.App)
	}

	cfg, err = Load(strings.NewReader(validAppConfigYAML("")))
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
			name: "requires https github config url",
			mutate: func(c *Config) {
				c.GitHub.ConfigURL = "http://github.com/oxidecomputer"
			},
			wantErr: "must be an absolute HTTPS URL",
		},
		{
			name: "requires github scope path",
			mutate: func(c *Config) {
				c.GitHub.ConfigURL = "https://github.com"
			},
			wantErr: "must include a GitHub scope path",
		},
		{
			name: "rejects github config url query",
			mutate: func(c *Config) {
				c.GitHub.ConfigURL = "https://github.com/oxidecomputer?x=1"
			},
			wantErr: "must not include user information",
		},
		{
			name: "rejects github config url with excessive path segments",
			mutate: func(c *Config) {
				c.GitHub.ConfigURL = "https://github.com/org/repo/extra"
			},
			wantErr: "must identify a GitHub enterprise",
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

func TestLoadRejectsNumericShutdownTimeout(t *testing.T) {
	_, err := Load(strings.NewReader(
		"shutdown_timeout: 45\n" + validConfigYAML,
	))
	requireErrorContains(t, err, "cannot unmarshal !!int `45` into time.Duration")
}

func TestScaleSetClient(t *testing.T) {
	validApp := &AppAuth{
		ClientID:       "Iv1.0123456789abcdef",
		InstallationID: 1234,
		PrivateKey:     "private-key",
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "personal access token"},
		{
			name: "GitHub App",
			mutate: func(c *Config) {
				c.GitHub.Auth = Auth{App: validApp}
			},
		},
		{
			name: "missing auth",
			mutate: func(c *Config) {
				c.GitHub.Auth = Auth{}
			},
			wantErr: "one of app or pat is required",
		},
		{
			name: "conflicting auth",
			mutate: func(c *Config) {
				c.GitHub.Auth.App = validApp
			},
			wantErr: "set only one of app or pat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}

			client, err := cfg.ScaleSetClient(scaleset.SystemInfo{})
			if tt.wantErr != "" {
				requireErrorContains(t, err, tt.wantErr)
				return
			}
			requireNoError(t, err)
			if client == nil {
				t.Fatal("expected scale set client")
			}
		})
	}
}

func TestOxideClient(t *testing.T) {
	cfg := validConfig()
	client, err := cfg.OxideClient()
	requireNoError(t, err)
	want := "https://oxide.example.com/"
	if got := client.Host(); got != want {
		t.Fatalf("expected Oxide host %q, got %q", want, got)
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
			name:    "rejects whitespace pat token",
			auth:    Auth{PAT: &PATAuth{Token: " \t "}},
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
		{
			name: "rejects whitespace app private key",
			auth: Auth{App: &AppAuth{
				ClientID:       "Iv1.0123456789abcdef",
				InstallationID: 1234,
				PrivateKey:     " \n ",
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
		name    string
		log     Log
		wantErr string
	}{
		{
			name: "accepts defaults",
			log:  Log{},
		},
		{
			name: "accepts valid values",
			log:  Log{Level: "debug", Format: "json"},
		},
		{
			name: "accepts values case insensitively",
			log:  Log{Level: "WARN", Format: "TEXT"},
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
				return
			}
			requireErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestConfigLogger(t *testing.T) {
	tests := []struct {
		name      string
		log       Log
		wantDebug bool
		wantText  bool
	}{
		{name: "defaults"},
		{
			name:      "configured debug text logger",
			log:       Log{Level: "debug", Format: "text"},
			wantDebug: true,
			wantText:  true,
		},
		{
			name: "invalid unvalidated config uses defaults",
			log:  Log{Level: "trace", Format: "pretty"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := (&Config{Log: tt.log}).Logger()
			if got := logger.Enabled(
				context.Background(), slog.LevelDebug,
			); got != tt.wantDebug {
				t.Fatalf("expected debug enabled %v, got %v", tt.wantDebug, got)
			}
			_, gotText := logger.Handler().(*slog.TextHandler)
			if gotText != tt.wantText {
				t.Fatalf("expected text handler %v, got %T", tt.wantText, logger.Handler())
			}
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
			name:    "rejects whitespace host",
			oxide:   Oxide{Host: " \t ", Token: "oxide-token"},
			wantErr: "oxide.host is required",
		},
		{
			name:    "requires token",
			oxide:   Oxide{Host: "https://oxide.example.com"},
			wantErr: "oxide.token is required",
		},
		{
			name:    "rejects whitespace token",
			oxide:   Oxide{Host: "https://oxide.example.com", Token: " \t "},
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
			name: "rejects whitespace name",
			mutate: func(s *ScaleSet) {
				s.Name = " \t "
			},
			wantErr: "name is required",
		},
		{
			name: "requires runner version",
			mutate: func(s *ScaleSet) {
				s.Runner.Version = ""
			},
			wantErr: "runner.version is required",
		},
		{
			name: "rejects invalid runner version",
			mutate: func(s *ScaleSet) {
				s.Runner.Version = "v2.335.1"
			},
			wantErr: "runner.version must use X.Y.Z format",
		},
		{
			name: "requires runner checksum",
			mutate: func(s *ScaleSet) {
				s.Runner.SHA256 = ""
			},
			wantErr: "runner.sha256 is required",
		},
		{
			name: "rejects invalid runner checksum",
			mutate: func(s *ScaleSet) {
				s.Runner.SHA256 = strings.Repeat("z", 64)
			},
			wantErr: "runner.sha256 must be a 64-character " +
				"hexadecimal checksum",
		},
		{
			name: "rejects short runner checksum",
			mutate: func(s *ScaleSet) {
				s.Runner.SHA256 = "abc123"
			},
			wantErr: "runner.sha256 must be a 64-character " +
				"hexadecimal checksum",
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
			name: "rejects duplicate normalized labels",
			mutate: func(s *ScaleSet) {
				s.Labels = []string{"linux-x64", " LINUX-X64 "}
			},
			wantErr: "labels[1] duplicates labels[0]",
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
			name: "accepts long name",
			mutate: func(s *ScaleSet) {
				s.Name = strings.Repeat("a", 100)
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

func TestScaleSetValidateNormalizesNamesAndLabels(t *testing.T) {
	ss := validScaleSet(" linux-x64 ")
	ss.RunnerGroup = " oxide-runners "
	ss.Labels = []string{" linux-x64 ", " self-hosted "}
	requireNoError(t, ss.Validate())

	if ss.Name != "linux-x64" {
		t.Fatalf("expected normalized name, got %q", ss.Name)
	}
	if ss.RunnerGroup != "oxide-runners" {
		t.Fatalf("expected normalized runner group, got %q", ss.RunnerGroup)
	}
	if ss.Labels[0] != "linux-x64" || ss.Labels[1] != "self-hosted" {
		t.Fatalf("expected normalized labels, got %v", ss.Labels)
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

	ss.Labels = []string{}
	labels = ss.RunnerLabels()
	if len(labels) != 1 || labels[0].Name != "linux-x64" {
		t.Fatalf("expected empty labels to default to scale set name, got %v", labels)
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
	tooManyGiB := uint64(maxByteCountGiB) + 1
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
			name: "rejects boot disk size that overflows bytes",
			mutate: func(i *Instance) {
				i.BootDiskGiB = uint(tooManyGiB)
			},
			wantErr: "instance.boot_disk_gib must be <= 17179869183",
		},
		{
			name: "requires positive cpus",
			mutate: func(i *Instance) {
				i.CPUs = 0
			},
			wantErr: "instance.cpus must be > 0",
		},
		{
			name: "rejects cpus that overflow the API type",
			mutate: func(i *Instance) {
				i.CPUs = math.MaxUint16 + 1
			},
			wantErr: "instance.cpus must be <= 65535",
		},
		{
			name: "requires positive memory",
			mutate: func(i *Instance) {
				i.MemoryGiB = 0
			},
			wantErr: "instance.memory_gib must be > 0",
		},
		{
			name: "rejects memory that overflows bytes",
			mutate: func(i *Instance) {
				i.MemoryGiB = uint(tooManyGiB)
			},
			wantErr: "instance.memory_gib must be <= 17179869183",
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

func TestInstanceValidateNormalizesNames(t *testing.T) {
	instance := validInstance()
	instance.Project = " actions-runners "
	instance.Image = " ubuntu-22.04 "
	instance.VPC = " default "
	instance.Subnet = " default "
	requireNoError(t, instance.Validate())

	if instance != validInstance() {
		t.Fatalf("expected normalized instance config, got %#v", instance)
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
		Name:   name,
		Labels: []string{name, "self-hosted"},
		Runner: Runner{
			Version: "2.335.1",
			SHA256:  "4ef2f25285f0ae4477f1fe1e346db76d2f3ebf03824e2ddd1973a2819bf6c8cf",
		},
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
log:
  level: debug
  format: text
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
  runner_group: oxide-runners
  runner:
    version: 2.335.1
    sha256: 4ef2f25285f0ae4477f1fe1e346db76d2f3ebf03824e2ddd1973a2819bf6c8cf
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
