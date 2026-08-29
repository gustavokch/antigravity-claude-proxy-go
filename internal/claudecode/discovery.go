package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DiscoverLocalCredentials attempts to scan standard Claude CLI config paths and environment
// variables for active credentials.
func DiscoverLocalCredentials(customHome string) ([]AccountConfig, error) {
	homeDir := customHome
	if homeDir == "" {
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("unable to determine user home directory: %w", err)
		}
	}

	foundTokens := make(map[string]AccountConfig)

	// 1. Check ~/.claude.json
	claudeJSONPath := filepath.Join(homeDir, ".claude.json")
	if data, err := os.ReadFile(claudeJSONPath); err == nil {
		extractTokensFromJSON(data, "claude-json", foundTokens)
	}

	// 2. Check ~/.claude/settings.json
	claudeSettingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	if data, err := os.ReadFile(claudeSettingsPath); err == nil {
		extractTokensFromJSON(data, "claude-settings", foundTokens)
	}

	// 3. Check environment variables
	envKeys := []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_TOKEN", "ANTHROPIC_AUTH_TOKEN"}
	for _, envKey := range envKeys {
		if val := strings.TrimSpace(os.Getenv(envKey)); val != "" {
			tokenType := "setup_token"
			if strings.HasPrefix(val, "sk-ant-api") {
				tokenType = "api_key"
			}
			id := generateTokenID(val)
			foundTokens[val] = AccountConfig{
				ID:       fmt.Sprintf("auto-env-%s", id),
				Name:     fmt.Sprintf("Claude Code (%s)", envKey),
				Token:    val,
				Type:     tokenType,
				Priority: 1,
				Enabled:  true,
				Source:   "auto_import",
			}
		}
	}

	accounts := make([]AccountConfig, 0, len(foundTokens))
	for _, acc := range foundTokens {
		accounts = append(accounts, acc)
	}

	return accounts, nil
}

func extractTokensFromJSON(data []byte, sourceLabel string, target map[string]AccountConfig) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}

	var email, accountUUID, orgUUID, refreshToken string
	var expiresAt *time.Time

	// Check oauthAccount object
	if oauthAcc, ok := raw["oauthAccount"].(map[string]any); ok {
		if em, ok := oauthAcc["emailAddress"].(string); ok {
			email = strings.TrimSpace(em)
		}
		if au, ok := oauthAcc["accountUuid"].(string); ok {
			accountUUID = strings.TrimSpace(au)
		}
		if ou, ok := oauthAcc["organizationUuid"].(string); ok {
			orgUUID = strings.TrimSpace(ou)
		}
	}

	if refTok, ok := raw["refreshToken"].(string); ok {
		refreshToken = strings.TrimSpace(refTok)
	} else if refTok, ok := raw["refresh_token"].(string); ok {
		refreshToken = strings.TrimSpace(refTok)
	}

	if expVal, ok := raw["expiresAt"]; ok {
		if expStr, ok := expVal.(string); ok {
			if t, err := time.Parse(time.RFC3339, expStr); err == nil {
				expiresAt = &t
			}
		} else if expNum, ok := expVal.(float64); ok {
			t := time.Unix(int64(expNum), 0)
			expiresAt = &t
		}
	}

	tokenKeys := []string{
		"sessionKey", "setup_token", "setupToken", "token",
		"oauth_token", "oauthToken", "apiKey", "api_key", "anthropicApiKey",
	}

	for _, key := range tokenKeys {
		if val, ok := raw[key]; ok {
			if strVal, isStr := val.(string); isStr {
				cleanToken := strings.TrimSpace(strVal)
				if cleanToken != "" && !strings.HasPrefix(cleanToken, "test") {
					if _, exists := target[cleanToken]; !exists {
						tokenType := "setup_token"
						if IsOAuthToken(cleanToken) || key == "oauthToken" || key == "oauth_token" || refreshToken != "" {
							tokenType = "oauth"
						} else if strings.HasPrefix(cleanToken, "sk-ant-api") {
							tokenType = "api_key"
						}
						id := generateTokenID(cleanToken)
						name := fmt.Sprintf("Claude CLI (%s)", sourceLabel)
						if email != "" {
							name = fmt.Sprintf("Claude Code (%s)", email)
						}
						target[cleanToken] = AccountConfig{
							ID:               fmt.Sprintf("auto-%s-%s", sourceLabel, id),
							Name:             name,
							Token:            cleanToken,
							RefreshToken:     refreshToken,
							ExpiresAt:        expiresAt,
							Email:            email,
							AccountUUID:      accountUUID,
							OrganizationUUID: orgUUID,
							Type:             tokenType,
							Priority:         1,
							Enabled:          true,
							Source:           "auto_import",
						}
					}
				}
			}
		}
	}

	// Also inspect nested env map if present (e.g. {"env": {"ANTHROPIC_API_KEY": "..."}})
	if envRaw, ok := raw["env"].(map[string]any); ok {
		for _, key := range tokenKeys {
			if val, ok := envRaw[key]; ok {
				if strVal, isStr := val.(string); isStr {
					cleanToken := strings.TrimSpace(strVal)
					if cleanToken != "" && !strings.HasPrefix(cleanToken, "test") {
						if _, exists := target[cleanToken]; !exists {
							tokenType := "setup_token"
							if IsOAuthToken(cleanToken) {
								tokenType = "oauth"
							} else if strings.HasPrefix(cleanToken, "sk-ant-api") {
								tokenType = "api_key"
							}
							id := generateTokenID(cleanToken)
							target[cleanToken] = AccountConfig{
								ID:       fmt.Sprintf("auto-env-%s-%s", sourceLabel, id),
								Name:     fmt.Sprintf("Claude CLI Env (%s)", sourceLabel),
								Token:    cleanToken,
								Type:     tokenType,
								Priority: 1,
								Enabled:  true,
								Source:   "auto_import",
							}
						}
					}
				}
			}
		}
	}
}

func generateTokenID(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:4]) // 8 chars
}
