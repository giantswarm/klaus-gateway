package slack

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Secrets holds the three Slack credentials used by the adapter.
type Secrets struct {
	// AppToken is the Slack app-level … token required for Socket Mode.
	AppToken string `yaml:"app_token"`
	// BotToken is the Slack bot … token for Slack API calls (chat.postMessage, chat.update).
	BotToken string `yaml:"bot_token"`
	// SigningSecret validates the x-slack-signature header on Events API requests.
	SigningSecret string `yaml:"signing_secret"`
}

// LoadSecrets reads credentials from a YAML file and then overlays the
// SLACK_APP_TOKEN / SLACK_BOT_TOKEN / SLACK_SIGNING_SECRET environment
// variables. BotToken and SigningSecret are always required; AppToken is only
// needed in socketmode.
//
// If file is empty or does not exist the function still succeeds and relies
// solely on environment variables.
func LoadSecrets(file string) (Secrets, error) {
	var s Secrets

	if file != "" {
		data, err := os.ReadFile(file) //nolint:gosec // path comes from trusted operator config
		if err != nil && !os.IsNotExist(err) {
			return Secrets{}, fmt.Errorf("slack: read secrets file %q: %w", file, err)
		}
		if err == nil {
			if yerr := yaml.Unmarshal(data, &s); yerr != nil {
				return Secrets{}, fmt.Errorf("slack: parse secrets file %q: %w", file, yerr)
			}
		}
	}

	if v := os.Getenv("SLACK_APP_TOKEN"); v != "" {
		s.AppToken = v
	}
	if v := os.Getenv("SLACK_BOT_TOKEN"); v != "" {
		s.BotToken = v
	}
	if v := os.Getenv("SLACK_SIGNING_SECRET"); v != "" {
		s.SigningSecret = v
	}

	if s.BotToken == "" {
		return Secrets{}, fmt.Errorf("slack: bot token is required (SLACK_BOT_TOKEN env or secrets file)")
	}
	if s.SigningSecret == "" {
		return Secrets{}, fmt.Errorf("slack: signing secret is required (SLACK_SIGNING_SECRET env or secrets file)")
	}
	return s, nil
}
