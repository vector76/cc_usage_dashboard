// Package config provides configuration loading and management.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/vector76/cc_usage_dashboard/internal/slack"
)

// Config holds the application configuration.
type Config struct {
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`

	HTTP struct {
		Port int      `yaml:"port"`
		Bind []string `yaml:"bind"`
	} `yaml:"http"`

	Claude struct {
		ProjectsDir string `yaml:"projects_dir"`
		// CoworkSessionsDir is the root of the desktop app's Cowork
		// ("local agent mode") session tree. Each Cowork session nests its
		// own private .claude home several levels under this root
		// (<workspace>/<id>/local_<uuid>/.claude/projects/<encoded-cwd>/
		// <session>.jsonl), so the tailer walks it recursively rather than
		// treating it as a projects dir directly. Empty disables it (e.g.
		// non-Windows, where APPDATA isn't set).
		CoworkSessionsDir string `yaml:"cowork_sessions_dir"`
	} `yaml:"claude"`

	Pricing struct {
		TablePath string `yaml:"table_path"`
	} `yaml:"pricing"`

	Tailer struct {
		PollIntervalMs int `yaml:"poll_interval_ms"`
	} `yaml:"tailer"`

	Logging struct {
		Level string `yaml:"level"`
		File  string `yaml:"file"`
	} `yaml:"logging"`

	Slack struct {
		BaselineMaxAgeSeconds   int     `yaml:"baseline_max_age_seconds"`
		SessionSurplusThreshold float64 `yaml:"session_surplus_threshold"`
		WeeklySurplusThreshold  float64 `yaml:"weekly_surplus_threshold"`
		// Session headroom additionally passes when percent_remaining is at
		// or above this fraction (0–1). A value of 1.0 disables the
		// absolute branch.
		SessionAbsoluteThreshold float64 `yaml:"session_absolute_threshold"`
		// Weekly headroom additionally passes when percent_remaining is at
		// or above this fraction (0–1). Lets the gate fire early in the
		// week before pace-relative surplus has accumulated.
		WeeklyAbsoluteThreshold float64 `yaml:"weekly_absolute_threshold"`
		// SessionProfile / WeeklyProfile define the slack-activation
		// boundary as [time_pct, remaining_pct] points in burn-down chart
		// coordinates: at time_pct percent of the window elapsed, slack is
		// available while percent-remaining is at or above the boundary
		// (linear interpolation between points, flat beyond the endpoints).
		// Absent (nil) means "derive the boundary from the scalar
		// thresholds above", which reproduces the pre-profile behavior.
		// Validated at load by slack.ProfileFromPairs.
		SessionProfile [][]float64 `yaml:"session_profile"`
		WeeklyProfile  [][]float64 `yaml:"weekly_profile"`
	} `yaml:"slack"`

	Retention struct {
		ParseErrorsDays  int `yaml:"parse_errors_days"`
		SlackSamplesDays int `yaml:"slack_samples_days"`
	} `yaml:"retention"`

	// Uplink forwards this machine's usage events to another trayapp, so a
	// VM or secondary machine's token spend lands in the primary host's
	// database alongside its own. URL is the peer's base URL
	// (scheme://host[:port]) — the forwarder appends endpoint paths itself,
	// which keeps /healthz reachable for a future clock-skew check and
	// leaves room for an https tunnel. Empty disables forwarding entirely
	// and is the default: a host-role trayapp never sets this.
	Uplink struct {
		URL string `yaml:"url"`
	} `yaml:"uplink"`

	// OAuthUsage polls Claude Code's OAuth usage endpoint for the same
	// account-scoped quota figures the userscript scrapes from the DOM,
	// as structured JSON and without a browser tab open. See
	// internal/oauthusage and docs/data-sources.md.
	//
	// Off by default: it makes scheduled requests to Anthropic's API,
	// which is not a behavior an existing install should acquire merely
	// by upgrading. Turning it on does not disable the userscript —
	// both may run, and their snapshots are tagged with distinct
	// sources so the two can be compared rather than silently merged.
	OAuthUsage struct {
		Enabled bool `yaml:"enabled"`
		// PollIntervalSeconds is deliberately unhurried. The endpoint is
		// undocumented and its rate limits are unpublished, and the
		// figures it reports move in whole percentage points, so there
		// is nothing to gain from a tight loop. Validated against
		// minOAuthPollIntervalSeconds, but only when Enabled.
		PollIntervalSeconds int `yaml:"poll_interval_seconds"`
		// CredentialsPath overrides the location of Claude Code's
		// .credentials.json. Empty means "resolve the standard
		// location", which honors CLAUDE_CONFIG_DIR — a literal default
		// here would shadow that env var.
		CredentialsPath string `yaml:"credentials_path"`
	} `yaml:"oauth_usage"`

	EnableSlackSampling bool `yaml:"enable_slack_sampling"`
}

// minOAuthPollIntervalSeconds is a floor, not a recommendation. It exists
// to turn a typo (a value meant as minutes, say) into a startup error
// rather than a stream of requests at an undocumented endpoint.
const minOAuthPollIntervalSeconds = 30

// Load loads configuration from a YAML file, applying defaults.
func Load(path string) (*Config, error) {
	var cfg Config

	// Set defaults
	// Left empty on purpose: the database location is resolved by the
	// caller via ResolveDBPath, which knows about $USAGE_DASHBOARD_DB, an
	// existing checkout database, and the per-user data dir. A literal
	// default here would shadow all three and pin the database to the
	// process working directory.
	cfg.Database.Path = ""
	cfg.HTTP.Port = 27812
	cfg.HTTP.Bind = []string{"127.0.0.1"}
	cfg.Claude.ProjectsDir = expandHome("~/.claude/projects")
	cfg.Claude.CoworkSessionsDir = defaultCoworkSessionsDir()
	// Empty means "use the resolution chain" (executable dir / app config
	// dir override, else the embedded built-in table). A non-empty value is
	// an explicit override. See ingest.ResolvePriceTable.
	cfg.Pricing.TablePath = ""
	cfg.Tailer.PollIntervalMs = 1000
	cfg.Logging.Level = "info"
	cfg.Logging.File = ""
	cfg.Slack.BaselineMaxAgeSeconds = 480
	cfg.Slack.SessionSurplusThreshold = 0.50
	cfg.Slack.WeeklySurplusThreshold = 0.10
	cfg.Slack.SessionAbsoluteThreshold = 0.98
	cfg.Slack.WeeklyAbsoluteThreshold = 0.80
	cfg.Retention.ParseErrorsDays = 30
	cfg.Retention.SlackSamplesDays = 90
	// Empty means "do not forward" — see the Uplink field comment.
	cfg.Uplink.URL = ""
	// Opt-in; three minutes when enabled. See the OAuthUsage field comment.
	cfg.OAuthUsage.Enabled = false
	cfg.OAuthUsage.PollIntervalSeconds = 180
	cfg.OAuthUsage.CredentialsPath = ""
	cfg.EnableSlackSampling = false

	// If no path provided, return defaults
	if path == "" {
		return &cfg, nil
	}

	// Load from file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Resolve env-style placeholders in path/dir fields.
	cfg.Database.Path = expandPlaceholders(cfg.Database.Path)
	cfg.Claude.ProjectsDir = expandPlaceholders(cfg.Claude.ProjectsDir)
	cfg.Claude.CoworkSessionsDir = expandPlaceholders(cfg.Claude.CoworkSessionsDir)
	cfg.Pricing.TablePath = expandPlaceholders(cfg.Pricing.TablePath)
	cfg.OAuthUsage.CredentialsPath = expandPlaceholders(cfg.OAuthUsage.CredentialsPath)

	// Reject malformed slack profiles at startup with the offending key in
	// the message, rather than letting the gate misbehave silently later.
	if _, err := slack.ProfileFromPairs(cfg.Slack.SessionProfile); err != nil {
		return nil, fmt.Errorf("config slack.session_profile: %w", err)
	}
	if _, err := slack.ProfileFromPairs(cfg.Slack.WeeklyProfile); err != nil {
		return nil, fmt.Errorf("config slack.weekly_profile: %w", err)
	}

	// Reject a malformed uplink at startup for the same reason. A bad URL
	// here would otherwise fail silently on every forward attempt, and the
	// symptom (missing events on the receiving host) shows up far from the
	// cause.
	normalized, err := normalizeUplinkURL(cfg.Uplink.URL)
	if err != nil {
		return nil, fmt.Errorf("config uplink.url: %w", err)
	}
	cfg.Uplink.URL = normalized

	// Only meaningful when something will actually poll: a stale or
	// nonsense interval left behind in a disabled block should not block
	// startup, since nothing is at risk.
	if cfg.OAuthUsage.Enabled && cfg.OAuthUsage.PollIntervalSeconds < minOAuthPollIntervalSeconds {
		return nil, fmt.Errorf(
			"config oauth_usage.poll_interval_seconds: %d is below the %d second minimum",
			cfg.OAuthUsage.PollIntervalSeconds, minOAuthPollIntervalSeconds)
	}

	return &cfg, nil
}

// normalizeUplinkURL validates an uplink address and returns it with any
// trailing slash removed. An empty string is valid and means "disabled".
//
// The address must be a bare origin: the forwarder joins "/log" (and later
// "/healthz") onto it, so a path, query, or fragment here would produce a
// URL that silently misses the endpoint. Rejecting them is friendlier than
// accepting a value that cannot work.
func normalizeUplinkURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("must be a base URL with no path, got path %q", u.Path)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("must be a base URL with no query or fragment")
	}

	u.Path = ""
	return u.String(), nil
}

// expandPlaceholders replaces Windows-style environment placeholders
// (%APPDATA%, %LOCALAPPDATA%, %USERPROFILE%, %HOME%) with values from the
// environment. On Linux those vars are typically empty, so we fall back to
// the user's home directory to keep cross-platform config files testable.
func expandPlaceholders(s string) string {
	if s == "" {
		return s
	}
	tokens := []string{"APPDATA", "LOCALAPPDATA", "USERPROFILE", "HOME"}
	var home string
	homeResolved := false
	for _, name := range tokens {
		token := "%" + name + "%"
		if !strings.Contains(s, token) {
			continue
		}
		val := os.Getenv(name)
		if val == "" {
			if !homeResolved {
				if h, err := os.UserHomeDir(); err == nil {
					home = h
				}
				homeResolved = true
			}
			val = home
		}
		if val == "" {
			continue
		}
		s = strings.ReplaceAll(s, token, val)
	}
	return s
}

// defaultCoworkSessionsDir returns the root of the Claude desktop app's
// Cowork ("local agent mode") session tree on Windows. Each Cowork session
// runs against its own private, sandboxed .claude home nested several
// levels under this root rather than the user's real ~/.claude — the
// tailer walks it recursively to reach those nested projects/ dirs (see
// docs/data-sources.md, Tier 1a). Returns "" when APPDATA isn't set (e.g.
// non-Windows), which disables this second root without erroring.
func defaultCoworkSessionsDir() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return ""
	}
	return filepath.Join(appData, "Claude", "local-agent-mode-sessions")
}

// expandHome expands a leading ~/ to the user's home directory.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}
