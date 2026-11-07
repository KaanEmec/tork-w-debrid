//go:build windows

package tui

import (
	"syscall"
	"unsafe"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetLogicalDrives = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW    = kernel32.NewProc("GetDriveTypeW")
)

const driveFixed = 3 // DRIVE_FIXED

// fixedDriveRoots lists the roots of the local fixed drives ("C:\", "D:\"),
// for the folder editor's quick picks.
func fixedDriveRoots() []string {
	mask, _, _ := procGetLogicalDrives.Call()
	var roots []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, err := syscall.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		if t, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(p))); t == driveFixed {
			roots = append(roots, root)
		}
	}
	return roots
}
