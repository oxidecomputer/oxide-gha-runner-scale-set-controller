// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide.go/oxide"
	"gopkg.in/yaml.v3"
)

const (
	maxByteCountGiB = math.MaxUint64 / (1024 * 1024 * 1024)
)

// Config is the application configuration.
type Config struct {
	Log      Log      `yaml:"log"`
	GitHub   GitHub   `yaml:"github"`
	Oxide    Oxide    `yaml:"oxide"`
	ScaleSet ScaleSet `yaml:"scale_set"`
}

// Log configures application logging.
type Log struct {
	// Level is the logging level. When provided, it must be one of `debug`,
	// `info`, `warn`, or `error`. Defaults to `info` when unset.
	Level string `yaml:"level"`

	// Format is the logging format. When provided, it must be one of `json` or
	// `text`. Defaults to `json` when unset.
	Format string `yaml:"format"`
}

// GitHub configures how each scale set registers itself with GitHub.
type GitHub struct {
	// ConfigURL is the GitHub configuration URL (i.e., registration URL) that each
	// scale set calls to register itself. The value differs whether the application
	// runs for a GitHub enterprise, organization, or repository.
	//
	// - Enterprise: https://github.com/enterprises/<enterprise-slug>
	// - Organization: https://github.com/<org>
	// - Repository: https://github.com/<org>/<repo>
	//
	// The canonical URL also namespaces the Oxide resources managed for the
	// scale set. Changing scopes leaves resources from the previous scope
	// unmanaged, so drain the scale set before changing it.
	ConfigURL string `yaml:"config_url"`

	// Auth configures how to authenticate to GitHub. Exactly one of the nested
	// [AppAuth] or [PATAuth] must be provided. If no auth is configured,
	// GITHUB_TOKEN synthesizes PAT auth. YAML values take precedence.
	Auth Auth `yaml:"auth"`
}

// Auth configures how to authenticate to GitHub.
type Auth struct {
	// App configures GitHub App authentication.
	App *AppAuth `yaml:"app,omitempty"`

	// PAT configures GitHub personal access token (PAT) authentication.
	PAT *PATAuth `yaml:"pat,omitempty"`
}

// AppAuth configures GitHub App authentication.
type AppAuth struct {
	// ClientID is the client ID for your GitHub App.
	ClientID string `yaml:"client_id"`

	// InstallationID is the installation ID for your GitHub App.
	InstallationID int64 `yaml:"installation_id"`

	// PrivateKey is the GitHub App private key. If empty, it falls back to
	// GITHUB_APP_PRIVATE_KEY. YAML values take precedence.
	PrivateKey string `yaml:"private_key"`
}

// PATAuth configures personal access token (PAT) authentication.
type PATAuth struct {
	// Token is the GitHub personal access token. If empty, it falls back to
	// GITHUB_TOKEN. YAML values take precedence.
	Token string `yaml:"token"`
}

// Oxide configures how each scale set connects to Oxide.
type Oxide struct {
	// Host is the Oxide silo host (e.g., https://oxide.sys.example.com). If
	// empty, it falls back to OXIDE_HOST. YAML values take precedence.
	Host string `yaml:"host"`

	// Token is the Oxide API token. If empty, it falls back to OXIDE_TOKEN.
	// YAML values take precedence.
	Token string `yaml:"token"`
}

// ScaleSet configures the runner scale set and the Oxide instances that it
// launches to execute GitHub Actions workflow jobs. Each process manages
// exactly one scale set; run one process per scale set.
type ScaleSet struct {
	// Name configures the runner scale set name. It is also used as the only
	// runner label when [ScaleSet.Labels] is unset.
	//
	// Workflows can target the name like this:
	//
	//   runs-on: <name>
	Name string `yaml:"name"`

	// RunnerGroup configures the GitHub runner group the scale set is created
	// under. If unset, the `default` runner group is used.
	//
	// Scale sets are looked up by name within this group. Changing the group
	// for an existing scale set name creates a new scale set in the new group
	// and leaves the old one behind to be manually deleted.
	//
	// Workflows can target the runner group like this:
	//
	//   runs-on:
	//     group: <runner-group>
	RunnerGroup string `yaml:"runner_group"`

	// Labels configures the GitHub runner labels used to match workflow jobs. If
	// unset or empty, [ScaleSet.Name] is used as the only label. Otherwise, the
	// configured labels are used exactly. Include [ScaleSet.Name] explicitly if
	// workflows should still target the scale set by name.
	//
	// Workflows can target labels like this:
	//
	//   runs-on:
	//     labels:
	//       - <label_a>
	//       - <label_b>
	Labels []string `yaml:"labels"`

	// Runner configures the GitHub Actions runner installed on each instance.
	Runner Runner `yaml:"runner"`

	// MinRunners is the minimum amount of runners to keep idle waiting for jobs.
	// Set both MinRunners and MaxRunners to zero to drain the scale set.
	MinRunners uint `yaml:"min_runners"`

	// MaxRunners is the maximum amount of runners to spawn to execute jobs. Zero
	// is only valid when [ScaleSet.MinRunners] is also zero and puts the scale
	// set in drain mode where no new jobs are acquired, idle runners are removed,
	// and busy runners are removed after their jobs finish. Once no owned
	// instances or boot disks remain, the GitHub scale set is deleted and the
	// process exits successfully.
	MaxRunners uint `yaml:"max_runners"`

	// Instance configures the Oxide instance the scale set launches.
	Instance Instance `yaml:"instance"`
}

// Runner configures the GitHub Actions runner installed on each instance.
type Runner struct {
	// Version is the GitHub Actions runner release to install. It must be a
	// version in X.Y.Z format.
	Version string `yaml:"version"`

	// SHA256 is the SHA-256 checksum of the Linux x64 runner archive for
	// [Runner.Version].
	SHA256 string `yaml:"sha256"`
}

// Instance configures the Oxide instance the scale set launches.
type Instance struct {
	// Project specifies the Oxide project the instance is created in.
	Project string `yaml:"project"`

	// Image specifies the Oxide image the instance boots from. This looks for a
	// project image first, falling back to a silo image if not found.
	Image string `yaml:"image"`

	// BootDiskGiB specifies the size of the instance's boot disk. When less than
	// the size of [Instance.Image], it'll be set to the size of the image.
	BootDiskGiB uint `yaml:"boot_disk_gib"`

	// CPUs specifies the number of CPUs the instance uses.
	CPUs uint `yaml:"cpus"`

	// MemoryGiB specifies the amount of memory the instance uses.
	MemoryGiB uint `yaml:"memory_gib"`

	// VPC is the VPC name the instance uses for its network interface.
	VPC string `yaml:"vpc"`

	// Subnet is the VPC subnet name the instance uses for its network interface.
	Subnet string `yaml:"subnet"`
}

// LoadFile reads, parses, normalizes, and validates the config file at path.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening config: %w", err)
	}
	defer f.Close()
	return Load(f)
}

// Load parses, normalizes, and validates the config read from r.
func Load(r io.Reader) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("parsing config: %w", err)
		}
		return nil, fmt.Errorf(
			"parsing config: multiple YAML documents are not supported",
		)
	}

	cfg.applyEnvFallbacks()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) applyEnvFallbacks() {
	if c.Oxide.Host == "" {
		c.Oxide.Host = os.Getenv("OXIDE_HOST")
	}
	if c.Oxide.Token == "" {
		c.Oxide.Token = os.Getenv("OXIDE_TOKEN")
	}

	if c.GitHub.Auth.App != nil {
		if c.GitHub.Auth.App.PrivateKey == "" {
			c.GitHub.Auth.App.PrivateKey = os.Getenv("GITHUB_APP_PRIVATE_KEY")
		}
		return
	}

	githubToken := os.Getenv("GITHUB_TOKEN")
	if c.GitHub.Auth.PAT != nil {
		if c.GitHub.Auth.PAT.Token == "" {
			c.GitHub.Auth.PAT.Token = githubToken
		}
		return
	}
	if githubToken != "" {
		c.GitHub.Auth.PAT = &PATAuth{Token: githubToken}
	}
}

// Logger builds a [slog.Logger] from the logging configuration. Invalid or
// unset values use the info level and JSON format defaults.
func (c *Config) Logger() *slog.Logger {
	level, _ := c.Log.slogLevel()
	text, _ := c.Log.textFormat()
	opts := &slog.HandlerOptions{Level: level}
	if text {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// ScaleSetClient builds a GitHub scale set API client.
func (c *Config) ScaleSetClient(
	info scaleset.SystemInfo,
) (*scaleset.Client, error) {
	if err := c.GitHub.Auth.Validate(); err != nil {
		return nil, err
	}
	if c.GitHub.Auth.App != nil {
		return scaleset.NewClientWithGitHubApp(
			scaleset.ClientWithGitHubAppConfig{
				GitHubConfigURL: c.GitHub.ConfigURL,
				GitHubAppAuth:   c.GitHub.Auth.App.toGitHubAppAuth(),
				SystemInfo:      info,
			},
		)
	}

	return scaleset.NewClientWithPersonalAccessToken(
		scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     c.GitHub.ConfigURL,
			PersonalAccessToken: c.GitHub.Auth.PAT.Token,
			SystemInfo:          info,
		},
	)
}

// OxideClient builds an Oxide API client.
func (c *Config) OxideClient() (*oxide.Client, error) {
	return oxide.NewClient(
		oxide.WithHost(c.Oxide.Host),
		oxide.WithToken(c.Oxide.Token),
	)
}

// RunnerLabels returns the labels configured for the scale set. If labels are
// unset or empty, labels default to the scale set name.
func (s *ScaleSet) RunnerLabels() []scaleset.Label {
	if len(s.Labels) == 0 {
		return []scaleset.Label{{Name: s.Name}}
	}

	labels := make([]scaleset.Label, len(s.Labels))
	for i, label := range s.Labels {
		labels[i] = scaleset.Label{Name: strings.TrimSpace(label)}
	}
	return labels
}

// Validate validates and normalizes the application configuration.
func (c *Config) Validate() error {
	if c.GitHub.ConfigURL == "" {
		return fmt.Errorf("github.config_url is required")
	}
	configURL, err := canonicalGitHubScope(c.GitHub.ConfigURL)
	if err != nil {
		return fmt.Errorf(
			"github.config_url is invalid: %w (expected a full URL "+
				"like https://github.com/org or "+
				"https://github.com/org/repo)", err,
		)
	}
	c.GitHub.ConfigURL = configURL
	if err := c.GitHub.Auth.Validate(); err != nil {
		return err
	}
	if err := c.Oxide.Validate(); err != nil {
		return err
	}
	if err := c.Log.Validate(); err != nil {
		return err
	}

	if err := c.ScaleSet.Validate(); err != nil {
		return fmt.Errorf("scale_set: %w", err)
	}

	return nil
}

func canonicalGitHubScope(configURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(configURL))
	if err != nil {
		return "", fmt.Errorf("parsing URL: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return "", fmt.Errorf("must be an absolute HTTPS URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf(
			"must not include user information, a query, or a fragment",
		)
	}

	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("must include a host")
	}
	if hostname == "www.github.com" {
		hostname = "github.com"
	}

	host := hostname
	port := u.Port()
	if port != "" && port != "443" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}

	path := strings.ToLower(strings.Trim(u.Path, "/"))
	if path == "" {
		return "", fmt.Errorf("must include a GitHub scope path")
	}
	parts := strings.Split(path, "/")
	if len(parts) > 2 {
		return "", fmt.Errorf(
			"must identify a GitHub enterprise, organization, or repository",
		)
	}

	return "https://" + host + "/" + path, nil
}

// Validate validates and normalizes the GitHub authentication configuration.
func (a *Auth) Validate() error {
	switch {
	case a.App != nil && a.PAT != nil:
		return fmt.Errorf("github.auth: set only one of app or pat")
	case a.App != nil:
		return a.App.Validate()
	case a.PAT != nil:
		return a.PAT.Validate()
	default:
		return fmt.Errorf("github.auth: one of app or pat is required")
	}
}

// Validate validates and normalizes the GitHub App authentication
// configuration.
func (a *AppAuth) Validate() error {
	a.ClientID = strings.TrimSpace(a.ClientID)
	if strings.TrimSpace(a.PrivateKey) == "" {
		return fmt.Errorf(
			"github.auth.app: app private key is required " +
				"(or set GITHUB_APP_PRIVATE_KEY)",
		)
	}
	// Delegate field validation to the library so we stay in sync with
	// its requirements.
	auth := a.toGitHubAppAuth()
	if err := auth.Validate(); err != nil {
		return fmt.Errorf("github.auth.app: %w", err)
	}
	return nil
}

// Validate validates and normalizes the GitHub personal access token
// configuration.
func (p *PATAuth) Validate() error {
	p.Token = strings.TrimSpace(p.Token)
	if p.Token == "" {
		return fmt.Errorf(
			"github.auth.pat.token is required (or set GITHUB_TOKEN)",
		)
	}
	return nil
}

// Validate validates the logging configuration.
func (l Log) Validate() error {
	if _, err := l.slogLevel(); err != nil {
		return err
	}
	if _, err := l.textFormat(); err != nil {
		return err
	}
	return nil
}

func (l Log) slogLevel() (slog.Level, error) {
	switch strings.ToLower(l.Level) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf(
			"log.level %q is invalid (debug, info, warn, error)",
			l.Level,
		)
	}
}

func (l Log) textFormat() (bool, error) {
	switch strings.ToLower(l.Format) {
	case "", "json":
		return false, nil
	case "text":
		return true, nil
	default:
		return false, fmt.Errorf(
			"log.format %q is invalid (json, text)", l.Format,
		)
	}
}

// Validate validates and normalizes the Oxide configuration.
func (o *Oxide) Validate() error {
	o.Host = strings.TrimSpace(o.Host)
	o.Token = strings.TrimSpace(o.Token)
	if o.Host == "" {
		return fmt.Errorf("oxide.host is required (or set OXIDE_HOST)")
	}
	if o.Token == "" {
		return fmt.Errorf("oxide.token is required (or set OXIDE_TOKEN)")
	}
	return nil
}

// Validate validates and normalizes the runner scale set configuration.
func (s *ScaleSet) Validate() error {
	s.Name = strings.TrimSpace(s.Name)
	s.RunnerGroup = strings.TrimSpace(s.RunnerGroup)
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if err := s.Runner.Validate(); err != nil {
		return err
	}
	labelIndexes := make(map[string]int, len(s.Labels))
	for i, label := range s.Labels {
		label = strings.TrimSpace(label)
		if label == "" {
			return fmt.Errorf("labels[%d] is required", i)
		}
		key := strings.ToLower(label)
		if previous, ok := labelIndexes[key]; ok {
			return fmt.Errorf(
				"labels[%d] duplicates labels[%d]", i, previous,
			)
		}
		s.Labels[i] = label
		labelIndexes[key] = i
	}
	if s.MinRunners > s.MaxRunners {
		return fmt.Errorf("min_runners must be <= max_runners")
	}
	if s.MaxRunners > math.MaxInt32 {
		return fmt.Errorf("max_runners must be <= %d", math.MaxInt32)
	}
	return s.Instance.Validate()
}

// Validate validates the GitHub Actions runner configuration.
func (r *Runner) Validate() error {
	if r.Version == "" {
		return fmt.Errorf("runner.version is required")
	}
	if !validRunnerVersion(r.Version) {
		return fmt.Errorf("runner.version must use X.Y.Z format")
	}
	if r.SHA256 == "" {
		return fmt.Errorf("runner.sha256 is required")
	}
	if len(r.SHA256) != sha256.Size*2 {
		return fmt.Errorf(
			"runner.sha256 must be a 64-character hexadecimal checksum",
		)
	}
	if _, err := hex.DecodeString(r.SHA256); err != nil {
		return fmt.Errorf(
			"runner.sha256 must be a 64-character hexadecimal checksum",
		)
	}
	return nil
}

func validRunnerVersion(version string) bool {
	components := strings.Split(version, ".")
	if len(components) != 3 {
		return false
	}
	for _, component := range components {
		if component == "" {
			return false
		}
		for _, char := range component {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

// Validate validates and normalizes the Oxide instance configuration.
func (i *Instance) Validate() error {
	i.Project = strings.TrimSpace(i.Project)
	i.VPC = strings.TrimSpace(i.VPC)
	i.Subnet = strings.TrimSpace(i.Subnet)
	i.Image = strings.TrimSpace(i.Image)
	switch {
	case i.Project == "":
		return fmt.Errorf("instance.project is required")
	case i.VPC == "":
		return fmt.Errorf("instance.vpc is required")
	case i.Subnet == "":
		return fmt.Errorf("instance.subnet is required")
	case i.Image == "":
		return fmt.Errorf("instance.image is required")
	case uint64(i.BootDiskGiB) > maxByteCountGiB:
		return fmt.Errorf(
			"instance.boot_disk_gib must be <= %d", maxByteCountGiB,
		)
	case i.CPUs == 0:
		return fmt.Errorf("instance.cpus must be > 0")
	case i.CPUs > math.MaxUint16:
		return fmt.Errorf("instance.cpus must be <= %d", math.MaxUint16)
	case i.MemoryGiB == 0:
		return fmt.Errorf("instance.memory_gib must be > 0")
	case uint64(i.MemoryGiB) > maxByteCountGiB:
		return fmt.Errorf(
			"instance.memory_gib must be <= %d", maxByteCountGiB,
		)
	}
	return nil
}

// toGitHubAppAuth converts the config into [scaleset.GitHubAppAuth].
func (a *AppAuth) toGitHubAppAuth() scaleset.GitHubAppAuth {
	return scaleset.GitHubAppAuth{
		ClientID:       a.ClientID,
		InstallationID: a.InstallationID,
		PrivateKey:     a.PrivateKey,
	}
}
