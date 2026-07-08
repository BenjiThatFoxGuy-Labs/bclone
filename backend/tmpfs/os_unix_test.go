//go:build !windows

package tmpfs

import "os"

var (
	osMkdirAll  = os.MkdirAll
	osReadDir   = os.ReadDir
	osSymlink   = os.Symlink
	osWriteFile = os.WriteFile
)
