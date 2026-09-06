//go:build linux

package tui_e2e

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openPTY creates a master/slave terminal pair without adding a third-party
// dependency to the production binary. The child receives the slave as its
// controlling terminal; the test reads and writes the master exactly as a
// terminal emulator would.
func openPTY(width, height int) (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("open ptmx: %w", err)
	}
	closeMaster := func() {
		_ = master.Close()
		master = nil
	}

	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		closeMaster()
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}
	ptyNumber, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		closeMaster()
		return nil, nil, fmt.Errorf("get pty number: %w", err)
	}

	slave, err = os.OpenFile(filepath.Join("/dev/pts", fmt.Sprint(ptyNumber)), os.O_RDWR|unix.O_NOCTTY, 0600)
	if err != nil {
		closeMaster()
		return nil, nil, fmt.Errorf("open pty slave: %w", err)
	}
	if err := resizePTY(master, width, height); err != nil {
		_ = slave.Close()
		closeMaster()
		return nil, nil, err
	}
	return master, slave, nil
}

func resizePTY(master *os.File, width, height int) error {
	if master == nil {
		return fmt.Errorf("resize pty: master is nil")
	}
	if width < 1 || height < 1 {
		return fmt.Errorf("resize pty: invalid dimensions %dx%d", width, height)
	}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(height),
		Col: uint16(width),
	}); err != nil {
		return fmt.Errorf("resize pty to %dx%d: %w", width, height, err)
	}
	return nil
}
