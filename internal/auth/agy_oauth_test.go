package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestFindAgyClientIDsRandomizedAgainstRegexp sweeps seeded random inputs
// against FindAll. The interesting failures here are positional: where pos
// lands after a failure, whether a run reaches back across a consumed match,
// whether two suffix occurrences sit adjacent. A hand-picked list covers what
// the author thought of; this sweep covers the neighbourhood.
func TestFindAgyClientIDsRandomizedAgainstRegexp(t *testing.T) {
	alphabets := [][]byte{
		[]byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_-."),
		[]byte("0-_.a"),
		[]byte(".apps.googleusercontent.com-0a_"),
		[]byte("0-.com"),
		[]byte("abc.apps.googleusercontent.com123-_"),
	}
	suffixes := [][]byte{
		agyClientIDSuffix,
		[]byte(".apps.googleusercontent.co"),
		[]byte(".apps."),
		[]byte(".com"),
		[]byte("apps.googleusercontent.com"),
	}
	const digits, idChars = "0123456789", "aZ9_-"
	var withMatch, withSeveral int
	for seed := 0; seed < 20000; seed++ {
		// Alphabet and suffix are indexed independently so all 25 pairs occur.
		alphabet := alphabets[seed%len(alphabets)]
		suffix := suffixes[(seed/len(alphabets))%len(suffixes)]
		random := rand.New(rand.NewSource(int64(seed)))
		var input []byte
		for n := 1 + random.Intn(40); n > 0; n-- {
			switch random.Intn(6) {
			case 0:
				input = append(input, suffix...)
			case 1:
				// An ID head, so matches are common rather than rare.
				for k := 1 + random.Intn(3); k > 0; k-- {
					input = append(input, digits[random.Intn(len(digits))])
				}
				input = append(input, '-')
				for k := 1 + random.Intn(3); k > 0; k-- {
					input = append(input, idChars[random.Intn(len(idChars))])
				}
			default:
				input = append(input, alphabet[random.Intn(len(alphabet))])
			}
		}
		want := agyClientIDPattern.FindAll(input, -1)
		got := findAgyClientIDs(input)
		if len(want) > 0 {
			withMatch++
		}
		if len(want) > 1 {
			withSeveral++
		}
		if len(got) != len(want) {
			t.Fatalf("seed %d input %.80q: got %d matches, want %d", seed, input, len(got), len(want))
		}
		for i := range want {
			if string(got[i]) != string(want[i]) {
				t.Fatalf("seed %d input %.80q: match %d = %.80q, want %.80q", seed, input, i, got[i], want[i])
			}
		}
	}
	// Guard the generator: a sweep where matches are rare proves little.
	if withMatch < 1000 || withSeveral < 500 {
		t.Fatalf("generator too weak: %d inputs with a match, %d with several", withMatch, withSeveral)
	}
}

// TestAgyClientIDByteClassMatchesPattern derives the pattern's character class
// from the live regexp and compares it with isAgyClientIDByte over every byte.
func TestAgyClientIDByteClassMatchesPattern(t *testing.T) {
	prefix := []byte("9-")
	tail := []byte(".apps." + "googleusercontent.com")
	for b := 0; b < 256; b++ {
		candidate := append(append([]byte{}, prefix...), byte(b))
		candidate = append(candidate, tail...)
		inPattern := agyClientIDPattern.Match(candidate)
		if inPattern != isAgyClientIDByte(byte(b)) {
			t.Fatalf("byte %#02x: accepted by pattern %v, accepted by isAgyClientIDByte %v",
				b, inPattern, isAgyClientIDByte(byte(b)))
		}
	}
}

// TestAgyClientIDSuffixIsPatternTail keeps the scan's anchor equal to the
// regexp's literal tail.
func TestAgyClientIDSuffixIsPatternTail(t *testing.T) {
	tail := regexp.QuoteMeta(string(agyClientIDSuffix))
	if !strings.HasSuffix(agyClientIDPattern.String(), tail) {
		t.Fatalf("pattern %q does not end with the quoted suffix %q", agyClientIDPattern.String(), tail)
	}
}

// fixtureLoginToken writes an agy token file whose id_token names clientID as
// its authorized party, the way agy's own login does.
func fixtureLoginToken(t *testing.T, clientID string) string {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"azp":"` + clientID + `","aud":"` + clientID + `"}`))
	contents := `{"token":{"refresh_token":"r"},"auth_method":"consumer","id_token":"e30.` + payload + `.c2ln"}`
	path := filepath.Join(t.TempDir(), "antigravity-oauth-token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAgyOAuthCredentialCandidatesPutLoginClientFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agy")
	firstID, loginID, lastID := fixtureClientID("123", "client"), fixtureClientID("456", "login"), fixtureClientID("789", "other")
	firstSecret, secondSecret := fixtureClientSecret("a"), fixtureClientSecret("b")
	contents := []byte(firstID + " " + loginID + " " + firstSecret + " " + lastID + " " + secondSecret)
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agyBinaryPathEnv, path)

	t.Run("login client first", func(t *testing.T) {
		t.Setenv("AGY_TOKEN_PATH", fixtureLoginToken(t, loginID))
		candidates, err := agyOAuthCredentialCandidates(agyLoginClientID())
		if err != nil {
			t.Fatal(err)
		}
		if candidates[0].clientID != loginID || candidates[0].clientSecret != firstSecret {
			t.Fatalf("first candidate = %+v", candidates[0])
		}
		if len(candidates) != 6 {
			t.Fatalf("got %d candidates, want every ID and secret pair", len(candidates))
		}
	})

	t.Run("without a login, last ID and first secret", func(t *testing.T) {
		t.Setenv("AGY_TOKEN_PATH", filepath.Join(t.TempDir(), "missing"))
		candidates, err := agyOAuthCredentialCandidates(agyLoginClientID())
		if err != nil {
			t.Fatal(err)
		}
		if candidates[0].clientID != lastID || candidates[0].clientSecret != firstSecret {
			t.Fatalf("first candidate = %+v", candidates[0])
		}
	})
}

// Manager.Get holds an exclusive lock on the agy token file while it refreshes,
// and the refresh reads the login client from that same file. That read must
// not wait for a lock this process already holds.
func TestManagerGetRefreshesTheDefaultAgyTokenWithoutDeadlock(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "agy")
	otherID, loginID := fixtureClientID("123", "other"), fixtureClientID("456", "login")
	if err := os.WriteFile(binary, []byte(loginID+" "+otherID+" "+fixtureClientSecret("a")), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agyBinaryPathEnv, binary)
	t.Setenv("AGY_TOKEN_PATH", fixtureLoginToken(t, loginID))

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			if got := request.Form.Get("client_id"); got != loginID {
				t.Errorf("client_id = %q, want the login client %q", got, loginID)
			}
			_, _ = writer.Write([]byte(`{"access_token":"fresh","expires_in":3600}`))
		case "/userinfo":
			_, _ = writer.Write([]byte(`{"email":"user@example.com"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	done := make(chan error, 1)
	go func() {
		manager := Manager{HTTPClient: server.Client(), TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo"}
		_, err := manager.Get(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Manager.Get did not return: the login-client read is waiting for the lock Get holds")
	}
}

// A refresh token is scoped to the client that issued it, and the id_token
// stored beside it names that client. An agy account can point at a token file
// other than the default one, so Get must take the login client from the file
// it refreshes, not from the default agy login.
func TestManagerGetOrdersByTheLoginOfTheTokenItRefreshes(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "agy")
	loginID, otherID := fixtureClientID("123", "login"), fixtureClientID("456", "other")
	if err := os.WriteFile(binary, []byte(loginID+" "+otherID+" "+fixtureClientSecret("a")), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agyBinaryPathEnv, binary)
	t.Setenv(oauthClientIDEnv, "")
	t.Setenv(oauthClientSecretEnv, "")
	t.Setenv("AGY_TOKEN_PATH", fixtureLoginToken(t, otherID))

	var mu sync.Mutex
	var attempts []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			clientID := request.Form.Get("client_id")
			mu.Lock()
			attempts = append(attempts, clientID)
			mu.Unlock()
			if clientID != loginID {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = writer.Write([]byte(`{"error":"invalid_client"}`))
				return
			}
			_, _ = writer.Write([]byte(`{"access_token":"fresh","expires_in":3600}`))
		case "/userinfo":
			_, _ = writer.Write([]byte(`{"email":"user@example.com"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	manager := Manager{
		Path:       fixtureLoginToken(t, loginID),
		HTTPClient: server.Client(), TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo",
	}
	if _, err := manager.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 1 || attempts[0] != loginID {
		t.Fatalf("client_id attempts = %v, want only the login client of the refreshed token %q", attempts, loginID)
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
	t.Setenv("AGY_TOKEN_PATH", filepath.Join(t.TempDir(), "missing"))
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
	response, err := (Manager{HTTPClient: server.Client(), TokenURL: server.URL}).refresh(context.Background(), "refresh-token", agyLoginClientID)
	if err != nil {
		t.Fatal(err)
	}
	if response.AccessToken != "fresh" || attempts != 2 {
		t.Fatalf("response=%#v attempts=%d", response, attempts)
	}
}

// agyScanFixture is a ~22 MB stand-in for the agy executable: mostly bytes that
// are not a client ID, with 200 client IDs spread through it. It gives the
// benchmark the same signal as the real binary without depending on an installed
// agy.
func agyScanFixture() []byte {
	var data []byte
	for block := 0; block < 200; block++ {
		data = append(data, bytes.Repeat([]byte("someOtherSymbolName_123 "), 5000)...)
		data = append(data, fixtureClientID("123", "abcDEF_-9")...)
	}
	return data
}

func BenchmarkFindAgyClientIDs(b *testing.B) {
	data := agyScanFixture()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if n := len(findAgyClientIDs(data)); n != 200 {
			b.Fatalf("got %d matches, want 200", n)
		}
	}
}

func BenchmarkAgyClientIDRegexpFindAll(b *testing.B) {
	data := agyScanFixture()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if n := len(agyClientIDPattern.FindAll(data, -1)); n != 200 {
			b.Fatalf("got %d matches, want 200", n)
		}
	}
}
