package corpus

import "testing"

func TestRedactHome(t *testing.T) {
	const home = "/home/a"
	cases := []struct {
		name string
		text string
		want string
	}{
		{"path under home", `{"Bash":"cat /home/a/.ssh/config"}`, `{"Bash":"cat ~/.ssh/config"}`},
		{"home alone", `{"Bash":"cd /home/a"}`, `{"Bash":"cd ~"}`},
		{"whole text", "/home/a", "~"},
		{"longer sibling", "cat /home/abc/x", "cat /home/abc/x"},
		{"dotted sibling", "cat /home/a.bak/x", "cat /home/a.bak/x"},
		{"non-ASCII sibling", "cat /home/aé/x", "cat /home/aé/x"},
		{"nested under another root", "cat /mnt/home/a/x", "cat /mnt/home/a/x"},
		{"adjacent occurrences", "/home/a/x /home/a/y", "~/x ~/y"},
		{"path list", "PATH=/home/a/bin:/home/a/go/bin", "PATH=~/bin:~/go/bin"},
		{"after a JSON escape", `echo hi\n/home/a/bin`, `echo hi\n~/bin`},
		{"no occurrence", "ls /tmp", "ls /tmp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactHome(tc.text, home); got != tc.want {
				t.Errorf("redactHome(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
	if got := redactHome("cat /home/a/x", ""); got != "cat /home/a/x" {
		t.Errorf("an empty home rewrote the text: %q", got)
	}
}

func TestUsableHome(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"/":          "",
		"/home/a":    "/home/a",
		"/home/a/":   "/home/a",
		"/Users/a//": "/Users/a",
	}
	for home, want := range cases {
		if got := usableHome(home); got != want {
			t.Errorf("usableHome(%q) = %q, want %q", home, got, want)
		}
	}
}
