package config

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/oxidecomputer/oxide.go/oxide"
	"gopkg.in/yaml.v3"
)

const (
	defaultShutdownTimeout = 30 * time.Minute

	// maxScaleSetNameLength keeps instance names of the form
	// "gha-runner-<scale set name>-<16 hex characters>" within Oxide's
	// 63-character name limit.
	maxScaleSetNameLength = 35
)

// Config is the application configuration.
type Config struct {
	Log             Log           `yaml:"log"`
	GitHub          GitHub        `yaml:"github"`
	Oxide           Oxide         `yaml:"oxide"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	ScaleSet        ScaleSet      `yaml:"scale_set"`
}

// Log configures application logging.
type Log struct {
	// Level is the logging level. When provided, it must be one of `debug`,
	// `info`, `warn`, or `error`. Defaults to `info` when unset.
	Level string `yaml:"level"`

	// Format is the logging format. When provided, it must be one of `json` or
	// `text`. Defaults to `json` when unset.
	Format string `yaml:"format"`

	// level and text hold the values parsed by [Log.Validate]. Their zero values
	// match the slog defaults so a config that has not been validated still
	// produces a sane logger.
	level slog.Level
	text  bool
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
	// Name configures the runner scale set name. This is also used in Oxide
	// instance and boot disk names and as the only runner label when
	// [ScaleSet.Labels] is unset.
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
	// unset, [ScaleSet.Name] is used as the only label. If set, the configured
	// labels are used exactly. Include [ScaleSet.Name] explicitly if workflows
	// should still target the scale set by name.
	//
	// Workflows can target labels like this:
	//
	//   runs-on:
	//     labels:
	//       - <label_a>
	//       - <label_b>
	Labels []string `yaml:"labels"`

	// MinRunners is the minimum amount of runners to keep idle waiting for jobs.
	// Set both MinRunners and MaxRunners to zero to drain the scale set.
	MinRunners uint `yaml:"min_runners"`

	// MaxRunners is the maximum amount of runners to spawn to execute jobs. Zero
	// is only valid when [ScaleSet.MinRunners] is also zero and puts the scale
	// set in drain mode where no new jobs are acquired, idle runners are removed,
	// and busy runners are removed after their jobs finish. The process exits
	// successfully once no owned instances or boot disks remain. The GitHub scale
	// set is retained.
	MaxRunners uint `yaml:"max_runners"`

	// Instance configures the Oxide instance the scale set launches.
	Instance Instance `yaml:"instance"`
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

	// Subnet is the VPC subnet the instance uses.
	Subnet string `yaml:"subnet"`
}

// LoadFile reads, parses, and validates the config file at path.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening config: %w", err)
	}
	defer f.Close()
	return Load(f)
}

// Load parses and validates the config read from r.
func Load(r io.Reader) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
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

// Logger builds a [slog.Logger] from the logging configuration parsed by
// [Log.Validate]. Defaults to info level and JSON format when the
// configuration has not been validated.
func (c *Config) Logger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.Log.level}
	if c.Log.text {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// ShutdownTimeoutDuration returns how long shutdown cleanup may run before
// being interrupted. Defaults to 30 minutes when unset.
func (c *Config) ShutdownTimeoutDuration() time.Duration {
	if c.ShutdownTimeout <= 0 {
		return defaultShutdownTimeout
	}
	return c.ShutdownTimeout
}

// ScaleSetClient builds a GitHub scale set API client.
func (c *Config) ScaleSetClient(
	info scaleset.SystemInfo,
) (*scaleset.Client, error) {
	// Prefer GitHub App auth over PAT when both are present.
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
// unset, labels default to the scale set name.
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

// Validate validates the application configuration.
func (c *Config) Validate() error {
	if c.GitHub.ConfigURL == "" {
		return fmt.Errorf("github.config_url is required")
	}
	if _, err := url.ParseRequestURI(c.GitHub.ConfigURL); err != nil {
		return fmt.Errorf(
			"github.config_url is invalid: %w (expected a full URL "+
				"like https://github.com/org or "+
				"https://github.com/org/repo)", err,
		)
	}
	if err := c.GitHub.Auth.Validate(); err != nil {
		return err
	}
	if err := c.Oxide.Validate(); err != nil {
		return err
	}
	if err := c.Log.Validate(); err != nil {
		return err
	}
	if c.ShutdownTimeout < 0 {
		return fmt.Errorf("shutdown_timeout must be >= 0")
	}

	if err := c.ScaleSet.Validate(); err != nil {
		return fmt.Errorf("scale_set: %w", err)
	}

	return nil
}

// Validate validates the GitHub authentication configuration.
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

// Validate validates the GitHub App authentication configuration.
func (a *AppAuth) Validate() error {
	if a.PrivateKey == "" {
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

// Validate validates the GitHub personal access token configuration.
func (p *PATAuth) Validate() error {
	if p.Token == "" {
		return fmt.Errorf(
			"github.auth.pat.token is required (or set GITHUB_TOKEN)",
		)
	}
	return nil
}

// Validate validates the logging configuration and stores the parsed level
// and format for [Config.Logger].
func (l *Log) Validate() error {
	switch strings.ToLower(l.Level) {
	case "", "info":
		l.level = slog.LevelInfo
	case "debug":
		l.level = slog.LevelDebug
	case "warn":
		l.level = slog.LevelWarn
	case "error":
		l.level = slog.LevelError
	default:
		return fmt.Errorf(
			"log.level %q is invalid (debug, info, warn, error)",
			l.Level,
		)
	}
	switch strings.ToLower(l.Format) {
	case "", "json":
		l.text = false
	case "text":
		l.text = true
	default:
		return fmt.Errorf(
			"log.format %q is invalid (json, text)", l.Format,
		)
	}
	return nil
}

// Validate validates the Oxide configuration.
func (o *Oxide) Validate() error {
	if o.Host == "" {
		return fmt.Errorf("oxide.host is required (or set OXIDE_HOST)")
	}
	if o.Token == "" {
		return fmt.Errorf("oxide.token is required (or set OXIDE_TOKEN)")
	}
	return nil
}

// Validate validates the runner scale set configuration.
func (s *ScaleSet) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(s.Name) > maxScaleSetNameLength {
		return fmt.Errorf("name %q is invalid: must be at most %d characters", s.Name, maxScaleSetNameLength)
	}
	for i, label := range s.Labels {
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("labels[%d] is required", i)
		}
	}
	if s.MinRunners > s.MaxRunners {
		return fmt.Errorf("min_runners must be <= max_runners")
	}
	if s.MaxRunners > math.MaxInt32 {
		return fmt.Errorf("max_runners must be <= %d", math.MaxInt32)
	}
	return s.Instance.Validate()
}

// Validate validates the Oxide instance configuration.
func (i *Instance) Validate() error {
	switch {
	case i.Project == "":
		return fmt.Errorf("instance.project is required")
	case i.VPC == "":
		return fmt.Errorf("instance.vpc is required")
	case i.Subnet == "":
		return fmt.Errorf("instance.subnet is required")
	case i.Image == "":
		return fmt.Errorf("instance.image is required")
	case i.CPUs == 0:
		return fmt.Errorf("instance.cpus must be > 0")
	case i.MemoryGiB == 0:
		return fmt.Errorf("instance.memory_gib must be > 0")
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
