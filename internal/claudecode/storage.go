package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var (
	storageMu sync.Mutex
)

// getConfigDir returns the path to ~/.config/antigravity-proxy directory or custom ANTIGRAVITY_CONFIG_DIR.
func getConfigDir() string {
	if custom := os.Getenv("ANTIGRAVITY_CONFIG_DIR"); custom != "" {
		return custom
	}
	if custom := os.Getenv("CONFIG_DIR"); custom != "" {
		return custom
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".config", "antigravity-proxy")
	}
	return filepath.Join(home, ".config", "antigravity-proxy")
}

// DefaultStoragePath returns the path to the dynamic Claude Code accounts file.
func DefaultStoragePath() string {
	return filepath.Join(getConfigDir(), "claudecode_accounts.json")
}

// LoadStoredAccounts loads persistent Claude Code accounts from disk.
func LoadStoredAccounts(path string) ([]AccountConfig, error) {
	storageMu.Lock()
	defer storageMu.Unlock()

	if path == "" {
		path = DefaultStoragePath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read claudecode accounts file: %w", err)
	}

	if len(data) == 0 {
		return nil, nil
	}

	var accounts []AccountConfig
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, fmt.Errorf("unmarshal claudecode accounts: %w", err)
	}

	return accounts, nil
}

// SaveStoredAccounts atomically persists dynamic Claude Code accounts to disk.
func SaveStoredAccounts(path string, accounts []AccountConfig) error {
	storageMu.Lock()
	defer storageMu.Unlock()

	if path == "" {
		path = DefaultStoragePath()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create directory for claudecode accounts: %w", err)
	}

	data, err := json.MarshalIndent(accounts, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claudecode accounts: %w", err)
	}

	tmpPath := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("write temp claudecode accounts: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename claudecode accounts file: %w", err)
	}

	return nil
}
