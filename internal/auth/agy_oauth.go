package auth

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

const agyBinaryPathEnv = "AGY_BINARY_PATH"

var (
	agyClientIDPattern     = regexp.MustCompile(`[0-9]+-[A-Za-z0-9_-]+\.apps\.googleusercontent\.com`)
	agyClientSecretPattern = regexp.MustCompile(`GOCSPX-[A-Za-z0-9_-]{28}`)
)

var agyClientIDSuffix = []byte(".apps.googleusercontent.com")

// findAgyClientIDs returns the same matches as agyClientIDPattern.FindAll but
// without scanning every byte of a ~170 MB executable with the regexp. The
// pattern has no literal prefix, so RE2 tries every offset (about 8 s). Every
// match ends with a fixed suffix and ID characters exclude '.', so each suffix
// occurrence can match only the run of ID characters directly before it. The
// regexp runs on that run alone.
func findAgyClientIDs(data []byte) [][]byte {
	var matches [][]byte
	pos := 0
	for {
		offset := bytes.Index(data[pos:], agyClientIDSuffix)
		if offset < 0 {
			return matches
		}
		suffixStart := pos + offset
		end := suffixStart + len(agyClientIDSuffix)
		runStart := suffixStart
		for runStart > pos && isAgyClientIDByte(data[runStart-1]) {
			runStart--
		}
		location := agyClientIDPattern.FindIndex(data[runStart:end])
		if location == nil {
			pos = suffixStart + 1
			continue
		}
		matches = append(matches, data[runStart+location[0]:runStart+location[1]])
		pos = runStart + location[1]
	}
}

func isAgyClientIDByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b == '_' || b == '-'
}

type oauthClientCredentials struct {
	clientID     string
	clientSecret string
}

// AgyOAuthCredentials reads the installed CLI's current OAuth client
// credentials. They are intentionally not copied into this proxy or its
// environment files. In current agy builds the consumer client ID is the last
// matching ID and its secret is the first matching secret in the string table.
// Keeping that extraction local aligns refreshes with the installed CLI
// version.
func AgyOAuthCredentials() (string, string, error) {
	candidates, err := agyOAuthCredentialCandidates()
	if err != nil {
		return "", "", err
	}
	return candidates[0].clientID, candidates[0].clientSecret, nil
}

func agyOAuthCredentialCandidates() ([]oauthClientCredentials, error) {
	path, err := AgyBinaryPath()
	if err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agy executable %q: %w", path, err)
	}
	clientIDs := findAgyClientIDs(contents)
	clientSecrets := agyClientSecretPattern.FindAll(contents, -1)
	if len(clientIDs) == 0 || len(clientSecrets) == 0 {
		return nil, fmt.Errorf("could not find OAuth refresh credentials in agy executable %q", path)
	}

	// agy includes consumer and business installed-app credentials. Keep the
	// observed consumer pair first, then try every other unique local pair only
	// if Google rejects it as invalid_client. A refresh token remains scoped to
	// the issuing client, so this resolves the exact pair for the active login.
	orderedIDs := append([][]byte{clientIDs[len(clientIDs)-1]}, clientIDs[:len(clientIDs)-1]...)
	orderedSecrets := append([][]byte{clientSecrets[0]}, clientSecrets[1:]...)
	candidates := make([]oauthClientCredentials, 0, len(orderedIDs)*len(orderedSecrets))
	seen := make(map[oauthClientCredentials]struct{})
	for _, clientID := range orderedIDs {
		for _, clientSecret := range orderedSecrets {
			candidate := oauthClientCredentials{clientID: string(clientID), clientSecret: string(clientSecret)}
			if _, exists := seen[candidate]; exists {
				continue
			}
			seen[candidate] = struct{}{}
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

// AgyBinaryPath resolves the executable associated with the local agy login.
// AGY_BINARY_PATH exists for non-standard installs and test fixtures.
func AgyBinaryPath() (string, error) {
	if explicit := os.Getenv(agyBinaryPathEnv); explicit != "" {
		return explicit, nil
	}
	if path, err := exec.LookPath("agy"); err == nil {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find agy executable: %w", err)
	}
	path := filepath.Join(home, ".local", "bin", "agy")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("agy executable not found; set %s for a non-standard install", agyBinaryPathEnv)
		}
		return "", fmt.Errorf("inspect agy executable %q: %w", path, err)
	}
	return path, nil
}
