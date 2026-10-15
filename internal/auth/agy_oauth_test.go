package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureClientID(prefix, label string) string {
	return prefix + "-" + label + ".apps." + "googleusercontent.com"
}

func fixtureClientSecret(character string) string {
	return "GOC" + "SPX-" + strings.Repeat(character, 28)
}

func TestFindAgyClientIDsMatchesRegexpSemantics(t *testing.T) {
	suffix := ".apps." + "googleusercontent.com"
	inputs := []string{
		"",
		"no match here",
		suffix,
		"-" + suffix,
		"1" + suffix,
		"1-" + suffix,
		"1-a" + suffix,
		"x 123-abc_DEF-9" + suffix + " y",
		"abc123-def" + suffix,
		"abc-123-def" + suffix,
		"a1-b2-c3" + suffix,
		"123-abc" + suffix + "456-def" + suffix,
		"123-abc" + suffix + suffix,
		"123-abc" + suffix + "-" + suffix,
		"123-abc.apps.googleusercontent.co 789-zzz" + suffix,
		"123.456-abc" + suffix,
		strings.Repeat("a", 5000) + "1-b" + suffix,
		strings.Repeat("9", 5000) + "-b" + suffix,
		strings.Repeat("9", 3) + strings.Repeat("-", 3) + "z" + suffix,
	}
	for _, input := range inputs {
		want := agyClientIDPattern.FindAll([]byte(input), -1)
		got := findAgyClientIDs([]byte(input))
		if len(got) != len(want) {
			t.Fatalf("input %.60q: got %d matches, want %d", input, len(got), len(want))
		}
		for i := range want {
			if string(got[i]) != string(want[i]) {
				t.Fatalf("input %.60q: match %d = %.80q, want %.80q", input, i, got[i], want[i])
			}
		}
	}
}

func TestAgyOAuthCredentialsReadsInstalledConsumerPair(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "agy")
	firstID, secondID := fixtureClientID("123", "client"), fixtureClientID("456", "other")
	firstSecret, secondSecret := fixtureClientSecret("a"), fixtureClientSecret("b")
	contents := []byte("prefix " + firstID + " middle " + firstSecret + " suffix " + secondID + " " + secondSecret)
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agyBinaryPathEnv, path)
	clientID, clientSecret, err := AgyOAuthCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if clientID != secondID || clientSecret != firstSecret {
		t.Fatalf("credentials = (%q, %q)", clientID, clientSecret)
	}
}

func TestManagerReadsRefreshCredentialsFromAgy(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "agy")
	clientID, clientSecret := fixtureClientID("123", "client"), fixtureClientSecret("a")
	if err := os.WriteFile(path, []byte(clientID+" "+clientSecret), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agyBinaryPathEnv, path)
	clientID, clientSecret, err := (Manager{}).oauthCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if clientID != fixtureClientID("123", "client") || clientSecret != fixtureClientSecret("a") {
		t.Fatalf("credentials = (%q, %q)", clientID, clientSecret)
	}
}

func TestManagerRefreshFallsBackToMatchingInstalledCredentials(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "agy")
	firstID, secondID := fixtureClientID("123", "client"), fixtureClientID("456", "other")
	firstSecret, secondSecret := fixtureClientSecret("a"), fixtureClientSecret("b")
	contents := []byte(firstID + " " + firstSecret + " " + secondID + " " + secondSecret)
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agyBinaryPathEnv, path)
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Error(err)
		}
		attempts++
		if request.Form.Get("client_secret") != secondSecret {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"access_token":"fresh","expires_in":3600}`))
	}))
	defer server.Close()
	response, err := (Manager{HTTPClient: server.Client(), TokenURL: server.URL}).refresh(context.Background(), "refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	if response.AccessToken != "fresh" || attempts != 2 {
		t.Fatalf("response=%#v attempts=%d", response, attempts)
	}
}
