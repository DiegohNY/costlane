package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// baseEnv is a minimal valid environment. Tests mutate a copy of it to
// isolate the one field under test.
func baseEnv() map[string]string {
	return map[string]string{
		"COSTLANE_DATABASE_URL":     "postgres://u:p@localhost:5432/costlane",
		"COSTLANE_MASTER_KEY":       "test-master-key-at-least-32-chars-long",
		"COSTLANE_PROVIDER_TIMEOUT": "300s",
		"COSTLANE_DRAIN_TIMEOUT":    "60s",
		"COSTLANE_RESERVATION_TTL":  "400s",
	}
}

func withEnv(t *testing.T, env map[string]string) *Config {
	t.Helper()
	cfg, err := LoadFrom(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	return cfg
}

func TestValidBaseConfig(t *testing.T) {
	cfg := withEnv(t, baseEnv())
	if cfg.ProviderTimeout != 300*time.Second {
		t.Errorf("ProviderTimeout = %v, want 300s", cfg.ProviderTimeout)
	}
	if cfg.DisconnectPolicy != "cancel" {
		t.Errorf("DisconnectPolicy = %q, want %q (spec 4.1: cancel is the default)",
			cfg.DisconnectPolicy, "cancel")
	}
}

func TestRequiredFieldsAreRequired(t *testing.T) {
	for _, key := range []string{"COSTLANE_DATABASE_URL", "COSTLANE_MASTER_KEY"} {
		t.Run(key, func(t *testing.T) {
			env := baseEnv()
			delete(env, key)
			_, err := LoadFrom(lookupFrom(env))
			if err == nil {
				t.Fatalf("missing %s must be rejected", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error must name the offending variable %s, got: %v", key, err)
			}
		})
	}
}

// Spec 5.7: drain_timeout <= provider_timeout, enforced at boot.
func TestDrainTimeoutMustNotExceedProviderTimeout(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_PROVIDER_TIMEOUT"] = "60s"
	env["COSTLANE_DRAIN_TIMEOUT"] = "61s"
	env["COSTLANE_RESERVATION_TTL"] = "300s"

	_, err := LoadFrom(lookupFrom(env))
	if err == nil {
		t.Fatal("drain_timeout > provider_timeout must be rejected")
	}
	if !strings.Contains(err.Error(), "COSTLANE_DRAIN_TIMEOUT") {
		t.Errorf("error must name COSTLANE_DRAIN_TIMEOUT, got: %v", err)
	}
}

func TestDrainTimeoutEqualToProviderTimeoutIsAllowed(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_PROVIDER_TIMEOUT"] = "60s"
	env["COSTLANE_DRAIN_TIMEOUT"] = "60s"
	env["COSTLANE_RESERVATION_TTL"] = "300s"
	withEnv(t, env) // boundary is inclusive
}

// Spec 5.7: the enforced floor is TTL >= provider_timeout + drain_timeout.
// The extra margin applies only to the default, not to the validation.
func TestReservationTTLMustCoverProviderPlusDrain(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_PROVIDER_TIMEOUT"] = "300s"
	env["COSTLANE_DRAIN_TIMEOUT"] = "60s"
	env["COSTLANE_RESERVATION_TTL"] = "359s" // one second short of the sum

	_, err := LoadFrom(lookupFrom(env))
	if err == nil {
		t.Fatal("TTL below provider_timeout + drain_timeout must be rejected")
	}
	if !strings.Contains(err.Error(), "COSTLANE_RESERVATION_TTL") {
		t.Errorf("error must name COSTLANE_RESERVATION_TTL, got: %v", err)
	}
}

func TestReservationTTLAtExactSumIsAllowed(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_PROVIDER_TIMEOUT"] = "300s"
	env["COSTLANE_DRAIN_TIMEOUT"] = "60s"
	env["COSTLANE_RESERVATION_TTL"] = "360s"
	withEnv(t, env)
}

func TestDisconnectPolicyRejectsUnknownValue(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_DISCONNECT_POLICY"] = "buffer"

	_, err := LoadFrom(lookupFrom(env))
	if err == nil {
		t.Fatal("unknown disconnect policy must be rejected")
	}
	if !strings.Contains(err.Error(), "cancel") || !strings.Contains(err.Error(), "drain") {
		t.Errorf("error should list the valid policies, got: %v", err)
	}
}

func TestDisconnectPolicyAcceptsDrain(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_DISCONNECT_POLICY"] = "drain"
	cfg := withEnv(t, env)
	if cfg.DisconnectPolicy != "drain" {
		t.Errorf("DisconnectPolicy = %q, want drain", cfg.DisconnectPolicy)
	}
}

func TestMalformedDurationIsRejected(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_PROVIDER_TIMEOUT"] = "five minutes"

	_, err := LoadFrom(lookupFrom(env))
	if err == nil {
		t.Fatal("malformed duration must be rejected")
	}
	if !strings.Contains(err.Error(), "COSTLANE_PROVIDER_TIMEOUT") {
		t.Errorf("error must name the offending variable, got: %v", err)
	}
}

func TestNonPositiveDurationIsRejected(t *testing.T) {
	for _, v := range []string{"0s", "-5s"} {
		t.Run(v, func(t *testing.T) {
			env := baseEnv()
			env["COSTLANE_PROVIDER_TIMEOUT"] = v
			if _, err := LoadFrom(lookupFrom(env)); err == nil {
				t.Fatalf("provider timeout %q must be rejected", v)
			}
		})
	}
}

func TestDrainSemaphoreDefaultsTo64(t *testing.T) {
	cfg := withEnv(t, baseEnv())
	if cfg.MaxConcurrentDrains != 64 {
		t.Errorf("MaxConcurrentDrains = %d, want 64", cfg.MaxConcurrentDrains)
	}
}

func TestLogPromptsDefaultsToFalse(t *testing.T) {
	cfg := withEnv(t, baseEnv())
	if cfg.LogPrompts {
		t.Error("LogPrompts must default to false: prompts are logged only on explicit opt-in")
	}
}

func TestMigrateOnBootDefaultsToTrue(t *testing.T) {
	cfg := withEnv(t, baseEnv())
	if !cfg.MigrateOnBoot {
		t.Error("MigrateOnBoot must default to true (spec 8.2)")
	}
}

// The master key ends up in constant-time comparisons; a short one is a
// configuration error, not a runtime surprise.
func TestShortMasterKeyIsRejected(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_MASTER_KEY"] = "short"
	if _, err := LoadFrom(lookupFrom(env)); err == nil {
		t.Fatal("a master key below the minimum length must be rejected")
	}
}

// Config carries secrets; its String must never render them.
func TestStringRedactsSecrets(t *testing.T) {
	env := baseEnv()
	cfg := withEnv(t, env)
	s := cfg.String()
	for _, secret := range []string{env["COSTLANE_MASTER_KEY"], "p@localhost", "://u:p@"} {
		if strings.Contains(s, secret) {
			t.Errorf("Config.String() leaked %q:\n%s", secret, s)
		}
	}
}

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

// A required value that is present but padded must arrive trimmed: it feeds a
// connection string and a constant-time credential comparison, where a stray
// newline from a shell heredoc would fail in a way that is hard to see.
func TestRequiredValuesAreTrimmed(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_DATABASE_URL"] = "  postgres://u:p@localhost:5432/costlane\n"
	env["COSTLANE_MASTER_KEY"] = "\ttest-master-key-at-least-32-chars-long  "

	cfg := withEnv(t, env)
	if got := cfg.DatabaseURL.Expose(); strings.TrimSpace(got) != got {
		t.Errorf("DatabaseURL retained whitespace: %q", got)
	}
	if got := cfg.MasterKey.Expose(); strings.TrimSpace(got) != got {
		t.Errorf("MasterKey retained whitespace: %q", got)
	}
}

// Every tunable that is loaded must appear in the logged rendering, or a
// deployment running on a non-default value shows no sign of it.
func TestStringRendersEveryTunable(t *testing.T) {
	cfg := withEnv(t, baseEnv())
	s := cfg.String()
	for _, field := range []string{
		"provider_timeout", "drain_timeout", "reservation_ttl",
		"disconnect_policy", "max_concurrent_drains", "default_max_tokens",
		"reaper_interval", "read_timeout", "max_query_window", "shutdown_timeout",
		"log_prompts", "migrate_on_boot",
	} {
		if !strings.Contains(s, field) {
			t.Errorf("Config.String() omits %q:\n%s", field, s)
		}
	}
}

func TestTierGuardDefaultsToThreeQuarters(t *testing.T) {
	cfg := withEnv(t, baseEnv())
	if cfg.TierGuard != 0.75 {
		t.Errorf("TierGuard = %v, want 0.75", cfg.TierGuard)
	}
}

// Zero would disable the guard without saying so, and above one would push
// every request to the top tier. Both are configuration errors.
func TestTierGuardRejectsValuesOutsideItsRange(t *testing.T) {
	for _, v := range []string{"0", "-0.5", "1.5", "2"} {
		t.Run(v, func(t *testing.T) {
			env := baseEnv()
			env["COSTLANE_TIER_GUARD"] = v
			if _, err := LoadFrom(lookupFrom(env)); err == nil {
				t.Errorf("tier guard %q must be rejected", v)
			}
		})
	}
}

func TestTierGuardAcceptsOne(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_TIER_GUARD"] = "1"
	cfg := withEnv(t, env)
	if cfg.TierGuard != 1 {
		t.Errorf("TierGuard = %v, want 1 (guard only at the threshold itself)", cfg.TierGuard)
	}
}

// The whole config is logged at startup and embedded in errors, so no
// rendering of it may carry a credential. With Secret this holds by
// construction rather than by remembering to redact each new field.
func TestNoRenderingOfConfigLeaksCredentials(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_MASTER_KEY"] = "sk-master-sentinel-value-32-chars-x"
	env["COSTLANE_DATABASE_URL"] = "postgres://dbuser:dbpassword-sentinel@localhost:5432/costlane"
	cfg := withEnv(t, env)

	renderings := map[string]string{
		"String()": cfg.String(),
		"%v":       fmt.Sprintf("%v", cfg),
		"%+v":      fmt.Sprintf("%+v", *cfg),
		"%#v":      fmt.Sprintf("%#v", *cfg),
		"error":    fmt.Errorf("startup failed: %+v", *cfg).Error(),
	}
	for _, secret := range []string{"sk-master-sentinel-value-32-chars-x", "dbpassword-sentinel"} {
		for name, got := range renderings {
			if strings.Contains(got, secret) {
				t.Errorf("%s leaked %q:\n%s", name, secret, got)
			}
		}
	}

	// The host is still visible, because a redacted DSN that hides which
	// database you failed to reach helps nobody.
	if !strings.Contains(cfg.String(), "localhost:5432") {
		t.Errorf("the connection target should stay visible: %s", cfg.String())
	}
}

// Prompt capture without an expiry is a copy of a customer's data that
// nothing ever deletes. The process refuses to boot rather than start
// accumulating it, because by the time anyone notices the table the data is
// already there.
func TestPromptLoggingRequiresARetentionPeriod(t *testing.T) {
	env := baseEnv()
	env["COSTLANE_LOG_PROMPTS"] = "true"

	_, err := LoadFrom(lookupFrom(env))
	if err == nil {
		t.Fatal("COSTLANE_LOG_PROMPTS=true with no retention must be rejected")
	}
	if !strings.Contains(err.Error(), "COSTLANE_PROMPT_RETENTION") {
		t.Errorf("error must name the missing variable, got: %v", err)
	}

	env["COSTLANE_PROMPT_RETENTION"] = "168h"
	cfg := withEnv(t, env)
	if cfg.PromptRetention != 168*time.Hour {
		t.Errorf("PromptRetention = %v, want 168h", cfg.PromptRetention)
	}
}

// Retention off is the default, and it must not be dragged in by anything
// else: a deployment that never enables prompt logging keeps its accounting
// rows forever, which is what an audit trail is for.
func TestRetentionIsOffByDefault(t *testing.T) {
	cfg := withEnv(t, baseEnv())
	if cfg.PromptRetention != 0 || cfg.UsageRetention != 0 {
		t.Errorf("retention defaults = (%v, %v), want both zero",
			cfg.PromptRetention, cfg.UsageRetention)
	}
	if cfg.RetentionInterval != time.Hour {
		t.Errorf("RetentionInterval = %v, want 1h", cfg.RetentionInterval)
	}
}
