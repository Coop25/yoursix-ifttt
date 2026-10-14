// Package config loads settings from Y6IFTTT_* environment variables.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

type Config struct {
	Addr           string
	DataDir        string
	PublicURL      string
	EventRetention time.Duration
	MaxBodyBytes   int64
	ClientIPHeader string
	DiscordWebhook string
	Webhook        WebhookConfig
	Auth           AuthConfig
	DevAuth        bool
	SeedRulesDir   string
	// SecretsKey enables the encrypted secrets manager (32 bytes, hex or base64).
	SecretsKey string
}

type WebhookConfig struct {
	// Token is the secret path segment: POST /hook/{token}. YourSix does not
	// sign webhooks, so an unguessable URL is the primary authenticator.
	Token string
	// RequireUserAgent rejects requests whose User-Agent is not y6-platform.
	RequireUserAgent bool
	// VerifySourceIP rejects requests whose client IP is not in the A record
	// of webhook.cloud.yoursix.com. Needs ClientIPHeader behind a tunnel.
	VerifySourceIP bool
}

type AuthConfig struct {
	ClientID        string
	ClientSecret    string
	RedirectURL     string
	SessionSecret   string
	SessionDuration time.Duration
	CookieSecure    bool
	AdminUserIDs    map[string]struct{}
	ViewerUserIDs   map[string]struct{}
}

func (a AuthConfig) Enabled() bool {
	return a.ClientID != "" && a.ClientSecret != "" && a.RedirectURL != "" && a.SessionSecret != ""
}

type raw struct {
	Addr                string   `envconfig:"ADDR" default:":8080"`
	DataDir             string   `envconfig:"DATA_DIR" default:"data"`
	PublicURL           string   `envconfig:"PUBLIC_URL"`
	EventRetentionDays  int      `envconfig:"EVENT_RETENTION_DAYS" default:"30"`
	MaxBodyBytes        int64    `envconfig:"MAX_BODY_BYTES" default:"262144"`
	ClientIPHeader      string   `envconfig:"CLIENT_IP_HEADER"`
	DiscordWebhookURL   string   `envconfig:"DISCORD_WEBHOOK_URL"`
	WebhookToken        string   `envconfig:"WEBHOOK_TOKEN"`
	WebhookRequireUA    bool     `envconfig:"WEBHOOK_REQUIRE_USER_AGENT" default:"true"`
	WebhookVerifyIP     bool     `envconfig:"WEBHOOK_VERIFY_SOURCE_IP" default:"false"`
	DiscordClientID     string   `envconfig:"DISCORD_CLIENT_ID"`
	DiscordClientSecret string   `envconfig:"DISCORD_CLIENT_SECRET"`
	DiscordRedirectURL  string   `envconfig:"DISCORD_REDIRECT_URL"`
	SessionSecret       string   `envconfig:"SESSION_SECRET"`
	SessionDurationDays int      `envconfig:"SESSION_DURATION_DAYS" default:"14"`
	CookieSecure        bool     `envconfig:"COOKIE_SECURE" default:"true"`
	AdminUserIDs        []string `envconfig:"ADMIN_USER_IDS"`
	ViewerUserIDs       []string `envconfig:"VIEWER_USER_IDS"`
	DevAuth             bool     `envconfig:"DEV_AUTH" default:"false"`
	SeedRulesDir        string   `envconfig:"SEED_RULES_DIR"`
	SecretsKey          string   `envconfig:"SECRETS_KEY"`
}

func Load() (Config, error) {
	var r raw
	if err := envconfig.Process("y6ifttt", &r); err != nil {
		return Config{}, err
	}
	c := Config{
		Addr:           strings.TrimSpace(r.Addr),
		DataDir:        strings.TrimSpace(r.DataDir),
		PublicURL:      strings.TrimRight(strings.TrimSpace(r.PublicURL), "/"),
		EventRetention: time.Duration(r.EventRetentionDays) * 24 * time.Hour,
		MaxBodyBytes:   r.MaxBodyBytes,
		ClientIPHeader: strings.TrimSpace(r.ClientIPHeader),
		DiscordWebhook: strings.TrimSpace(r.DiscordWebhookURL),
		Webhook: WebhookConfig{
			Token:            strings.TrimSpace(r.WebhookToken),
			RequireUserAgent: r.WebhookRequireUA,
			VerifySourceIP:   r.WebhookVerifyIP,
		},
		Auth: AuthConfig{
			ClientID:        strings.TrimSpace(r.DiscordClientID),
			ClientSecret:    strings.TrimSpace(r.DiscordClientSecret),
			RedirectURL:     strings.TrimSpace(r.DiscordRedirectURL),
			SessionSecret:   r.SessionSecret,
			SessionDuration: time.Duration(r.SessionDurationDays) * 24 * time.Hour,
			CookieSecure:    r.CookieSecure,
			AdminUserIDs:    idSet(r.AdminUserIDs),
			ViewerUserIDs:   idSet(r.ViewerUserIDs),
		},
		DevAuth:      r.DevAuth,
		SeedRulesDir: strings.TrimSpace(r.SeedRulesDir),
		SecretsKey:   strings.TrimSpace(r.SecretsKey),
	}
	if c.Auth.RedirectURL == "" && c.PublicURL != "" {
		c.Auth.RedirectURL = c.PublicURL + "/auth/callback"
	}
	return c, c.validate()
}

func (c Config) validate() error {
	var errs []error
	if len(c.Webhook.Token) < 24 {
		errs = append(errs, errors.New("Y6IFTTT_WEBHOOK_TOKEN must be set to a random string of at least 24 characters"))
	}
	if !c.DevAuth {
		if !c.Auth.Enabled() {
			errs = append(errs, errors.New("Discord login is required: set Y6IFTTT_DISCORD_CLIENT_ID, _CLIENT_SECRET, _SESSION_SECRET and _PUBLIC_URL (or _DISCORD_REDIRECT_URL); or Y6IFTTT_DEV_AUTH=true for local testing"))
		}
		if len(c.Auth.AdminUserIDs) == 0 && len(c.Auth.ViewerUserIDs) == 0 {
			errs = append(errs, errors.New("Y6IFTTT_ADMIN_USER_IDS must list at least one Discord user ID"))
		}
	}
	if c.Auth.SessionSecret != "" && len(c.Auth.SessionSecret) < 32 {
		errs = append(errs, errors.New("Y6IFTTT_SESSION_SECRET must be at least 32 characters"))
	}
	if c.MaxBodyBytes <= 0 {
		errs = append(errs, fmt.Errorf("Y6IFTTT_MAX_BODY_BYTES must be positive"))
	}
	return errors.Join(errs...)
}

func idSet(ids []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}
