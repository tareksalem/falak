//go:build linux

package startup

import "os"

// readProcFile reads a procfs/sysfs entry on Linux hosts. It exists as a
// separate build-tagged file so non-Linux dev hosts can stub the read
// without ever touching `/proc/sys`.
func readProcFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
