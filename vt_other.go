//go:build !windows

package main

import "os"

// enableVirtualTerminal reports whether f can render VT escape sequences.
// Unix terminals always can.
func enableVirtualTerminal(*os.File) bool {
	return true
}
