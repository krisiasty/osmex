//go:build unix

package secret

import (
	"os"
	"slices"
	"syscall"
)

// fileGroupIsOurs reports whether the file's group is one this process belongs to.
func fileGroupIsOurs(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	gid := int(st.Gid)
	if gid == os.Getgid() || gid == os.Getegid() {
		return true
	}
	// Whether Getgroups includes the primary group depends on the platform, so both are checked.
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	return slices.Contains(groups, gid)
}
