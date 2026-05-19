package fuse

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func unmount(dir string) error {
	if os.Geteuid() == 0 {
		return syscall.Unmount(dir, 0)
	}

	cmd := exec.Command("fusermount3", "-u", dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if len(output) > 0 {
			output = bytes.TrimRight(output, "\n")
			msg := err.Error() + ": " + string(output)
			err = errors.New(msg)
		}
		return err
	}
	return nil
}
