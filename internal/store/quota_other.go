//go:build !linux && !freebsd && !netbsd && !openbsd && !dragonfly && !solaris && !aix

package store

// isQuotaErr is false where EDQUOT is not a defined errno.
func isQuotaErr(error) bool { return false }
