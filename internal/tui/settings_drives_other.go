//go:build !windows

package tui

// fixedDriveRoots is Windows-only: other systems mount every disk under /.
func fixedDriveRoots() []string { return nil }
