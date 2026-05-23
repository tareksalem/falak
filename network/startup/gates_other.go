//go:build !linux

package startup

import (
	"errors"
	"io/fs"
)

// readProcFile is the non-Linux stub. macOS and Windows have no procfs;
// returning an fs.ErrNotExist lets the rp_filter check return nil via
// the non-Linux short-circuit in rpFilterCheck and lets the firewalld
// pidfile fallback report "not present" without panicking on missing
// syscalls.
func readProcFile(_ string) ([]byte, error) {
	return nil, errors.New("procfs unavailable on non-Linux host: " + fs.ErrNotExist.Error())
}
