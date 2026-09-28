package ccusage

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ClaudePaths returns the Claude config directories that hold a projects/
// directory. CLAUDE_CONFIG_DIR, when set, is a comma-separated list and is the
// only source; each entry may name the config directory or its projects/
// directory. Otherwise $XDG_CONFIG_HOME/claude (default ~/.config/claude) and
// ~/.claude are checked, in that order. No directory is not an error.
func ClaudePaths() []string {
	home, _ := os.UserHomeDir()
	var paths []string
	seen := make(map[string]bool)
	add := func(path string) {
		path = filepath.Clean(path)
		if isDir(filepath.Join(path, "projects")) && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}

	if env, ok := os.LookupEnv("CLAUDE_CONFIG_DIR"); ok {
		for _, raw := range strings.Split(env, ",") {
			if raw = strings.TrimSpace(raw); raw == "" {
				continue
			}
			path := expandHome(raw, home)
			if filepath.Base(path) == "projects" && isDir(path) {
				path = filepath.Dir(path)
			}
			add(path)
		}
		return paths
	}

	if home == "" {
		return nil
	}
	xdg, ok := os.LookupEnv("XDG_CONFIG_HOME")
	if !ok {
		xdg = filepath.Join(home, ".config")
	}
	add(filepath.Join(xdg, "claude"))
	add(filepath.Join(home, ".claude"))
	return paths
}

func expandHome(raw, home string) string {
	if home == "" {
		return raw
	}
	if raw == "~" {
		return home
	}
	if rest, ok := strings.CutPrefix(raw, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return raw
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// UsageFiles lists every *.jsonl file below each config directory's
// projects/, sorted by path. Symlinks below projects/ are not followed.
func UsageFiles(paths []string) []string {
	var files []string
	for _, path := range paths {
		collectJSONL(filepath.Join(path, "projects"), &files)
	}
	slices.Sort(files)
	return files
}

func collectJSONL(dir string, files *[]string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		switch {
		case e.Type().IsRegular() && e.Name() != ".jsonl" && strings.HasSuffix(e.Name(), ".jsonl"):
			*files = append(*files, path)
		case e.IsDir():
			collectJSONL(path, files)
		}
	}
}

// pathParts splits a path into components the way Rust's Path::components
// does: empty and "." segments are dropped, and an absolute path keeps its
// root as the first component.
func pathParts(path string) []string {
	var parts []string
	if filepath.IsAbs(path) {
		parts = append(parts, string(filepath.Separator))
	}
	for _, p := range strings.Split(filepath.ToSlash(path), "/") {
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	return parts
}

// ExtractProject returns the first directory under projects/, or "unknown".
func ExtractProject(path string) string {
	parts := pathParts(path)
	for i, p := range parts {
		if p == "projects" && i+1 < len(parts) {
			if strings.TrimSpace(parts[i+1]) == "" {
				return "unknown"
			}
			return parts[i+1]
		}
	}
	return "unknown"
}

// SessionParts returns the session ID and project path of a transcript. It
// understands projects/{proj}/{sid}.jsonl, the subagent layout
// projects/{proj}/{sid}/subagents/{agent}.jsonl, and otherwise takes the
// file's parent directory as the session, as in projects/{proj}/{sid}/chat.jsonl.
func SessionParts(path string) (sessionID, projectPath string) {
	rel := pathParts(path)
	if i := slices.Index(rel, "projects"); i >= 0 {
		rel = rel[i+1:]
	}
	sep := string(filepath.Separator)
	if len(rel) == 2 {
		if sid := strings.TrimSuffix(rel[1], ".jsonl"); sid != rel[1] && sid != "" {
			return sid, rel[0]
		}
	}
	if len(rel) >= 4 && rel[len(rel)-2] == "subagents" {
		projectPath = strings.Join(rel[:len(rel)-3], sep)
		if projectPath == "" {
			projectPath = "Unknown Project"
		}
		return rel[len(rel)-3], projectPath
	}
	sessionID = "unknown"
	if len(rel) > 0 {
		sessionID = rel[max(len(rel)-2, 0)]
	}
	projectPath = "Unknown Project"
	if len(rel) > 2 {
		projectPath = strings.Join(rel[:len(rel)-2], sep)
	}
	return sessionID, projectPath
}
