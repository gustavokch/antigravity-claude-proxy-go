package ccusage

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClaudePaths_ConfigDirEnv(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "home/a/projects", "b/projects", "c", "home/.claude/projects")
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)

	tests := []struct {
		name string
		env  string
		want []string
	}{
		{"list with spaces, tilde and projects suffix", " ~/a , " + filepath.Join(root, "b", "projects") + ",," + filepath.Join(root, "c"),
			[]string{filepath.Join(home, "a"), filepath.Join(root, "b")}},
		{"duplicates collapse", filepath.Join(root, "b") + "," + filepath.Join(root, "b", "projects") + "," + filepath.Join(root, "b") + "/",
			[]string{filepath.Join(root, "b")}},
		{"nothing valid does not fall back", filepath.Join(root, "c"), nil},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", tt.env)
			if got := ClaudePaths(); !slices.Equal(got, tt.want) {
				t.Errorf("ClaudePaths() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClaudePaths_Defaults(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	// t.Setenv first so the variables are restored after the test.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	t.Setenv("XDG_CONFIG_HOME", "")
	os.Unsetenv("XDG_CONFIG_HOME")

	if got := ClaudePaths(); len(got) != 0 {
		t.Errorf("no config dirs: ClaudePaths() = %v, want empty", got)
	}

	mkdirs(t, home, ".claude/projects", ".config/claude/projects")
	want := []string{filepath.Join(home, ".config", "claude"), filepath.Join(home, ".claude")}
	if got := ClaudePaths(); !slices.Equal(got, want) {
		t.Errorf("default XDG: ClaudePaths() = %v, want %v", got, want)
	}

	mkdirs(t, root, "xdg/claude/projects")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	want = []string{filepath.Join(root, "xdg", "claude"), filepath.Join(home, ".claude")}
	if got := ClaudePaths(); !slices.Equal(got, want) {
		t.Errorf("XDG_CONFIG_HOME: ClaudePaths() = %v, want %v", got, want)
	}
}

func TestUsageFiles(t *testing.T) {
	root := t.TempDir()
	files := []string{
		"projects/p/s.jsonl",
		"projects/p/s/subagents/agent-a.jsonl",
		"projects/a/session/chat.jsonl",
		"projects/a/notes.txt",
		"projects/a/.jsonl",
		"other/x.jsonl",
	}
	for _, f := range files {
		mkdirs(t, root, filepath.Dir(f))
		if err := os.WriteFile(filepath.Join(root, f), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "other"), filepath.Join(root, "projects", "link")); err != nil {
		t.Fatal(err)
	}

	got := UsageFiles([]string{root, filepath.Join(root, "missing")})
	want := []string{
		filepath.Join(root, "projects/a/session/chat.jsonl"),
		filepath.Join(root, "projects/p/s.jsonl"),
		filepath.Join(root, "projects/p/s/subagents/agent-a.jsonl"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("UsageFiles = %v, want %v", got, want)
	}

	fixtures := UsageFiles([]string{filepath.Join("testdata", "claude")})
	if len(fixtures) != 2 {
		t.Errorf("fixture UsageFiles = %v, want 2 files", fixtures)
	}
}

func TestSessionParts(t *testing.T) {
	tests := []struct {
		path, session, project string
	}{
		{"/home/me/.claude/projects/project-a/session-a.jsonl", "session-a", "project-a"},
		{"/home/me/.claude/projects/project-a/session-a/chat.jsonl", "session-a", "project-a"},
		{"/home/me/.claude/projects/project-a/session-a/subagents/worker.jsonl", "session-a", "project-a"},
		{"/home/me/.claude/projects/org/project-a/session-a/subagents/worker.jsonl", "session-a", "org/project-a"},
		{"/home/me/.claude/projects/p/s/deep/chat.jsonl", "deep", "p/s"},
		{"/home/me/.claude/projects/session.jsonl", "session.jsonl", "Unknown Project"},
		{"projects/.jsonl/x", ".jsonl", "Unknown Project"},
	}
	for _, tt := range tests {
		session, project := SessionParts(filepath.FromSlash(tt.path))
		if session != tt.session || project != filepath.FromSlash(tt.project) {
			t.Errorf("SessionParts(%q) = %q, %q; want %q, %q", tt.path, session, project, tt.session, tt.project)
		}
	}
}

func TestExtractProject(t *testing.T) {
	for path, want := range map[string]string{
		"/home/me/.claude/projects/project-a/session-a/chat.jsonl": "project-a",
		"/home/me/.claude/projects/ /s.jsonl":                      "unknown",
		"/home/me/.claude/projects":                                "unknown",
		"/home/me/chat.jsonl":                                      "unknown",
	} {
		if got := ExtractProject(filepath.FromSlash(path)); got != want {
			t.Errorf("ExtractProject(%q) = %q, want %q", path, got, want)
		}
	}
}
