//go:build linux || freebsd || netbsd || openbsd || dragonfly || solaris || aix

package store

import (
	"errors"
	"syscall"
)

// isQuotaErr reports EDQUOT (the errno is not portable to every GOOS).
func isQuotaErr(err error) bool { return errors.Is(err, syscall.EDQUOT) }
