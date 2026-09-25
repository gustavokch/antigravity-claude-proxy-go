//go:build !windows

package kimi

import (
	"golang.org/x/sys/unix"
)

// osInfo returns the uname release and version strings.
func osInfo() (release, version string) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "", ""
	}
	return unix.ByteSliceToString(u.Release[:]), unix.ByteSliceToString(u.Version[:])
}
