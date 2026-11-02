package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionLine(t *testing.T) {
	vcs := &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "true"}}}
	for _, tt := range []struct {
		name         string
		commit, date string
		info         *debug.BuildInfo
		want         string
	}{
		{"release", "fa2bd9b", "2026-10-24T18:00:00Z", vcs, "osmex dev (commit fa2bd9b, built 2026-10-24T18:00:00Z)"},
		{"checkout", "", "", vcs, "osmex dev (commit abc123 (modified), built unknown)"},
		{"clean checkout", "", "", &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "false"}}},
			"osmex dev (commit abc123, built unknown)"},
		{"no build info", "", "", nil, "osmex dev (commit unknown, built unknown)"},
		{"module download", "", "", &debug.BuildInfo{}, "osmex dev (commit unknown, built unknown)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			originalCommit, originalDate := commit, date
			t.Cleanup(func() { commit, date = originalCommit, originalDate })
			commit, date = tt.commit, tt.date
			if got := versionLine(tt.info); got != tt.want {
				t.Errorf("versionLine = %q, want %q", got, tt.want)
			}
		})
	}
}
