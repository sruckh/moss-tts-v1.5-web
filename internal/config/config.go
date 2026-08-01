// Package config loads runtime configuration from the environment.
//
// Secrets (RUNPOD_API_KEY) arrive as environment variables injected by the
// Infisical CLI wrapper in the container entrypoint — this package never talks
// to Infisical itself.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SecureCookiesEnv names the variable that opts session cookies into the
// Secure flag. Set it to a truthy value ("true", "1", "yes", "on") in
// deployment behind Cloudflare/NPM where TLS is terminated upstream; leave it
// unset for local HTTP testing so the browser actually sends the cookie.
//
// This is an explicit opt-in: reference audio is delivered to RunPod base64
// inline (never a public URL), so there is no public hostname to infer "https"
// from anymore.
const SecureCookiesEnv = "TIMBRE_SECURE_COOKIES"

// RunPodEndpointEnv names the variable holding the serverless endpoint Timbre
// renders through, e.g. https://api.runpod.ai/v2/<your-endpoint-id>.
//
// There is no default on purpose: the endpoint id identifies a specific paid
// deployment, so it is supplied per environment rather than compiled in. It is
// a server-side value either way — the browser never calls RunPod directly.
const RunPodEndpointEnv = "RUNPOD_ENDPOINT"

// Config is the fully resolved runtime configuration.
type Config struct {
	// Addr is the listen address. No host port is published; NGINX Proxy
	// Manager reaches this port over the shared_net docker network.
	Addr string

	// DBPath is the SQLite file, on a volume the litestream sidecar shares.
	DBPath string

	// AudioDir holds rendered WAVs and uploaded reference samples.
	AudioDir string

	// RunPodEndpoint is the base URL for /run, /status/{id}, /health.
	RunPodEndpoint string

	// RunPodAPIKey is the bearer token. Injected by Infisical at runtime.
	RunPodAPIKey string

	// AdminUsername and AdminPassword seed the first user on startup when the
	// users table is empty (see internal/auth's Bootstrap). Injected by
	// Infisical alongside RUNPOD_API_KEY.
	AdminUsername string
	AdminPassword string

	// SessionSecret keys the HMAC that signs session cookies. Injected by
	// Infisical; when empty an ephemeral key is generated at startup and all
	// sessions die with the process.
	SessionSecret string

	// MaxInFlight caps how many jobs the worker keeps submitted at once.
	MaxInFlight int
}

// Load reads configuration from the environment, applying defaults.
func Load() (Config, error) {
	cfg := Config{
		Addr:           env("TIMBRE_ADDR", ":8080"),
		DBPath:         env("TIMBRE_DB_PATH", "/data/timbre.db"),
		AudioDir:       env("TIMBRE_AUDIO_DIR", "/data/audio"),
		RunPodEndpoint: strings.TrimRight(os.Getenv(RunPodEndpointEnv), "/"),
		RunPodAPIKey:   os.Getenv("RUNPOD_API_KEY"),
		AdminUsername:  os.Getenv("ADMIN_USERNAME"),
		AdminPassword:  os.Getenv("ADMIN_PASSWORD"),
		SessionSecret:  os.Getenv("TIMBRE_SESSION_SECRET"),
	}

	maxInFlight, err := strconv.Atoi(env("TIMBRE_MAX_IN_FLIGHT", "2"))
	if err != nil {
		return Config{}, fmt.Errorf("TIMBRE_MAX_IN_FLIGHT: %w", err)
	}
	if maxInFlight < 1 {
		return Config{}, fmt.Errorf("TIMBRE_MAX_IN_FLIGHT must be >= 1, got %d", maxInFlight)
	}
	cfg.MaxInFlight = maxInFlight

	return cfg, nil
}

// HasRunPodKey reports whether the API key was injected, without exposing it.
func (c Config) HasRunPodKey() bool { return c.RunPodAPIKey != "" }

// SecureCookies reports whether session cookies should carry the Secure flag.
// It is an explicit opt-in via TIMBRE_SECURE_COOKIES rather than inferred from
// a public URL: reference audio is sent to RunPod base64-inline, so nothing
// about the app needs a public hostname to function.
func (c Config) SecureCookies() bool {
	return envBool(SecureCookiesEnv)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envBool reads a truthy environment variable ("1", "true", "yes", "on",
// case-insensitive). Empty or anything else is false.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
