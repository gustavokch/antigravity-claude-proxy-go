//go:build windows

package kimi

// osInfo returns no OS details on Windows; header values become "unknown".
func osInfo() (release, version string) {
	return "", ""
}
