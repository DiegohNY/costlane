package config

import (
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

// Spec 5.7: TTL >= provider_timeout + drain_timeout + margin.
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
