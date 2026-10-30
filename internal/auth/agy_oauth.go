package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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
		// pos is the end of the last match (or one past the last rejected
		// suffix), so the run never reaches back into bytes FindAll has already
		// consumed. Without the bound it could re-enter the previous match's
		// literal "com" tail; that is harmless today because "com" cannot start
		// a [0-9]+-led match, but the bound keeps the argument local.
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

// agyOAuthCredentialCandidates reads the installed CLI's OAuth client
// credentials, most likely pair first. They are intentionally not copied into
// this proxy or its environment files; reading them locally keeps sign-in and
// refresh aligned with the installed CLI version.
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

	// agy embeds several installed-app clients, and not every one accepts a
	// loopback redirect (agy 1.4.10 added one that answers
	// redirect_uri_mismatch). Their order in the string table is a linker
	// artifact, so the client that issued agy's own login goes first. Without
	// that login the older observation stands: last ID, first secret. Every other
	// unique pair follows, tried only if Google rejects the one before it as
	// invalid_client. A refresh token remains scoped to the issuing client, so
	// this resolves the exact pair for the active login.
	orderedIDs := append([][]byte{clientIDs[len(clientIDs)-1]}, clientIDs[:len(clientIDs)-1]...)
	if loginClientID := agyLoginClientID(); loginClientID != "" {
		if i := slices.IndexFunc(orderedIDs, func(id []byte) bool { return string(id) == loginClientID }); i > 0 {
			id := orderedIDs[i]
			copy(orderedIDs[1:i+1], orderedIDs[:i])
			orderedIDs[0] = id
		}
	}
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

// agyLoginClientID returns the OAuth client that issued agy's own login: the
// azp claim of the id_token agy stores beside the refresh token. It returns ""
// when the token file, the id_token, or the claim is missing.
func agyLoginClientID() string {
	path, err := DefaultTokenPath()
	if err != nil {
		return ""
	}
	raw, err := readTokenFile(path)
	if err != nil {
		return ""
	}
	var document struct {
		IDToken string `json:"id_token"`
		Token   struct {
			IDToken string `json:"id_token"`
		} `json:"token"`
	}
	if json.Unmarshal(raw, &document) != nil {
		return ""
	}
	idToken := document.IDToken
	if idToken == "" {
		idToken = document.Token.IDToken
	}
	segments := strings.Split(idToken, ".")
	if len(segments) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segments[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.AuthorizedParty
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
