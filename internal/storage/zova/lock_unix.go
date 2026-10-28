//go:build !windows

package zova

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes an flock on f without blocking. It reports false when another
// open file holds a conflicting lock.
func tryLock(f *os.File, exclusive bool) (bool, error) {
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	for {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}

func unlockFile(f *os.File) { unix.Flock(int(f.Fd()), unix.LOCK_UN) }
