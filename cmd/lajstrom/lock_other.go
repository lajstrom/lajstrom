//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
)

// lockFile is errors.ErrUnsupported where the boot is not known (processScope), which lockGone
// needs.
func lockFile(string) (*os.File, string, error) { return nil, "", errors.ErrUnsupported }
