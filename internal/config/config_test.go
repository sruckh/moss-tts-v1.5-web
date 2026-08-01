package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("TIMBRE_ADDR", "")
	t.Setenv("RUNPOD_ENDPOINT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	// No compiled-in endpoint: it names a specific paid deployment.
	if cfg.RunPodEndpoint != "" {
		t.Errorf("RunPodEndpoint = %q, want empty when %s is unset",
			cfg.RunPodEndpoint, RunPodEndpointEnv)
	}
	if cfg.MaxInFlight != 2 {
		t.Errorf("MaxInFlight = %d, want 2", cfg.MaxInFlight)
	}
}

func TestLoadTrimsTrailingSlashes(t *testing.T) {
	t.Setenv("RUNPOD_ENDPOINT", "https://api.runpod.ai/v2/abc/")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RunPodEndpoint != "https://api.runpod.ai/v2/abc" {
		t.Errorf("RunPodEndpoint = %q", cfg.RunPodEndpoint)
	}
}

func TestSecureCookiesOptIn(t *testing.T) {
	// Defaults to off — local HTTP testing must keep the cookie usable.
	t.Setenv(SecureCookiesEnv, "")
	if (Config{}).SecureCookies() {
		t.Error("SecureCookies = true with no env, want false")
	}

	for _, val := range []string{"true", "1", "yes", "on", "TRUE"} {
		t.Setenv(SecureCookiesEnv, val)
		if !(Config{}).SecureCookies() {
			t.Errorf("SecureCookies(%q) = false, want true", val)
		}
	}
	for _, val := range []string{"", "false", "0", "no", "off", "maybe"} {
		t.Setenv(SecureCookiesEnv, val)
		if (Config{}).SecureCookies() {
			t.Errorf("SecureCookies(%q) = true, want false", val)
		}
	}
}

func TestLoadRejectsBadMaxInFlight(t *testing.T) {
	t.Setenv("TIMBRE_MAX_IN_FLIGHT", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for TIMBRE_MAX_IN_FLIGHT=0")
	}
}

func TestHasRunPodKey(t *testing.T) {
	if (Config{}).HasRunPodKey() {
		t.Error("empty config should not report a key")
	}
	if !(Config{RunPodAPIKey: "x"}).HasRunPodKey() {
		t.Error("config with a key should report one")
	}
}
