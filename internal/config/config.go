package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Lookup reports the value of an environment variable and whether it was set.
// It mirrors os.LookupEnv so tests can supply an environment without touching
// the process.
type Lookup func(key string) (string, bool)

const (
	// PolicyCancel stops the upstream call when the client disconnects.
	// Providers halt generation on connection close and bill only for what
	// they produced, so cancelling is the correct default for a
	// cost-control product. See spec 4.1.
	PolicyCancel = "cancel"
	// PolicyDrain keeps reading upstream to obtain an exact usage figure,
	// at the cost of paying for tokens nobody will read.
	PolicyDrain = "drain"

	minMasterKeyLen = 32
)

// Config holds every tunable, resolved from the environment and validated.
// There are no cloud-specific dependencies: everything arrives as an
// environment variable.
type Config struct {
	DatabaseURL string
	MasterKey   string

	ListenAddr  string
	MetricsAddr string

	ProviderTimeout time.Duration
	DrainTimeout    time.Duration
	ReservationTTL  time.Duration

	DisconnectPolicy    string
	MaxConcurrentDrains int

	DefaultMaxTokens int

	ReaperInterval  time.Duration
	ReadTimeout     time.Duration
	MaxQueryWindow  time.Duration
	ShutdownTimeout time.Duration

	LogPrompts    bool
	MigrateOnBoot bool
}

// Load reads configuration from the process environment.
func Load() (*Config, error) { return LoadFrom(os.LookupEnv) }

// LoadFrom reads configuration using the supplied lookup function and
// validates it. Every problem found is reported together, so a misconfigured
// deployment surfaces all its errors in one pass rather than one per restart.
func LoadFrom(look Lookup) (*Config, error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	cfg := &Config{}

	cfg.DatabaseURL = requireString(look, "COSTLANE_DATABASE_URL", fail)
	cfg.MasterKey = requireString(look, "COSTLANE_MASTER_KEY", fail)
	if cfg.MasterKey != "" && len(cfg.MasterKey) < minMasterKeyLen {
		fail("COSTLANE_MASTER_KEY must be at least %d characters, got %d",
			minMasterKeyLen, len(cfg.MasterKey))
	}

	cfg.ListenAddr = optString(look, "COSTLANE_LISTEN_ADDR", ":8080")
	cfg.MetricsAddr = optString(look, "COSTLANE_METRICS_ADDR", ":9090")

	cfg.ProviderTimeout = duration(look, "COSTLANE_PROVIDER_TIMEOUT", 300*time.Second, fail)
	cfg.DrainTimeout = duration(look, "COSTLANE_DRAIN_TIMEOUT", 60*time.Second, fail)
	cfg.ReservationTTL = duration(look, "COSTLANE_RESERVATION_TTL", 0, fail)
	cfg.ReaperInterval = duration(look, "COSTLANE_REAPER_INTERVAL", 30*time.Second, fail)
	cfg.ReadTimeout = duration(look, "COSTLANE_READ_TIMEOUT", 5*time.Second, fail)
	cfg.MaxQueryWindow = duration(look, "COSTLANE_MAX_QUERY_WINDOW", 90*24*time.Hour, fail)
	cfg.ShutdownTimeout = duration(look, "COSTLANE_SHUTDOWN_TIMEOUT", 30*time.Second, fail)

	// A TTL that does not cover the whole request would let the reaper
	// expropriate a live stream, so default it to the sum plus a margin
	// rather than to a bare constant.
	if cfg.ReservationTTL == 0 {
		cfg.ReservationTTL = cfg.ProviderTimeout + cfg.DrainTimeout + 60*time.Second
	}

	cfg.DisconnectPolicy = optString(look, "COSTLANE_DISCONNECT_POLICY", PolicyCancel)
	if cfg.DisconnectPolicy != PolicyCancel && cfg.DisconnectPolicy != PolicyDrain {
		fail("COSTLANE_DISCONNECT_POLICY must be %q or %q, got %q",
			PolicyCancel, PolicyDrain, cfg.DisconnectPolicy)
	}

	cfg.MaxConcurrentDrains = positiveInt(look, "COSTLANE_MAX_CONCURRENT_DRAINS", 64, fail)
	cfg.DefaultMaxTokens = positiveInt(look, "COSTLANE_DEFAULT_MAX_TOKENS", 4096, fail)

	cfg.LogPrompts = boolean(look, "COSTLANE_LOG_PROMPTS", false, fail)
	cfg.MigrateOnBoot = boolean(look, "COSTLANE_MIGRATE_ON_BOOT", true, fail)

	// Spec 5.7. Draining runs after the provider timeout has elapsed, so a
	// drain window wider than the provider window cannot be honoured.
	if cfg.DrainTimeout > cfg.ProviderTimeout {
		fail("COSTLANE_DRAIN_TIMEOUT (%s) must not exceed COSTLANE_PROVIDER_TIMEOUT (%s)",
			cfg.DrainTimeout, cfg.ProviderTimeout)
	}

	// Spec 5.7. A reservation must outlive the request that owns it,
	// otherwise the reaper releases budget still in use and the settle
	// lands after expiry.
	if minTTL := cfg.ProviderTimeout + cfg.DrainTimeout; cfg.ReservationTTL < minTTL {
		fail("COSTLANE_RESERVATION_TTL (%s) must be at least "+
			"COSTLANE_PROVIDER_TIMEOUT + COSTLANE_DRAIN_TIMEOUT (%s)",
			cfg.ReservationTTL, minTTL)
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// String renders the configuration for logging with every secret redacted.
func (c *Config) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "database_url=%s ", redactURL(c.DatabaseURL))
	fmt.Fprintf(&b, "master_key=%s ", redact(c.MasterKey))
	fmt.Fprintf(&b, "listen=%s metrics=%s ", c.ListenAddr, c.MetricsAddr)
	fmt.Fprintf(&b, "provider_timeout=%s drain_timeout=%s reservation_ttl=%s ",
		c.ProviderTimeout, c.DrainTimeout, c.ReservationTTL)
	fmt.Fprintf(&b, "disconnect_policy=%s max_concurrent_drains=%d ",
		c.DisconnectPolicy, c.MaxConcurrentDrains)
	fmt.Fprintf(&b, "default_max_tokens=%d log_prompts=%t migrate_on_boot=%t",
		c.DefaultMaxTokens, c.LogPrompts, c.MigrateOnBoot)
	return b.String()
}

func redact(s string) string {
	if s == "" {
		return "(unset)"
	}
	return "(redacted)"
}

// redactURL keeps the shape of a connection string without its credentials,
// which is enough to debug a wrong host and never enough to leak a password.
func redactURL(raw string) string {
	if raw == "" {
		return "(unset)"
	}
	scheme, rest, found := strings.Cut(raw, "://")
	if !found {
		return "(redacted)"
	}
	if _, after, hasCreds := strings.Cut(rest, "@"); hasCreds {
		return scheme + "://(redacted)@" + after
	}
	return scheme + "://" + rest
}

func requireString(look Lookup, key string, fail func(string, ...any)) string {
	v, ok := look(key)
	if !ok || strings.TrimSpace(v) == "" {
		fail("%s is required", key)
		return ""
	}
	return v
}

func optString(look Lookup, key, def string) string {
	if v, ok := look(key); ok && v != "" {
		return v
	}
	return def
}

func duration(look Lookup, key string, def time.Duration, fail func(string, ...any)) time.Duration {
	raw, ok := look(key)
	if !ok || raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s is not a valid duration (%q): %v", key, raw, err)
		return def
	}
	if d <= 0 {
		fail("%s must be positive, got %s", key, d)
		return def
	}
	return d
}

func positiveInt(look Lookup, key string, def int, fail func(string, ...any)) int {
	raw, ok := look(key)
	if !ok || raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s is not a valid integer (%q): %v", key, raw, err)
		return def
	}
	if n <= 0 {
		fail("%s must be positive, got %d", key, n)
		return def
	}
	return n
}

func boolean(look Lookup, key string, def bool, fail func(string, ...any)) bool {
	raw, ok := look(key)
	if !ok || raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		fail("%s is not a valid boolean (%q): %v", key, raw, err)
		return def
	}
	return b
}
