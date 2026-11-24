//go:build !linux

package config

import "errors"

// cacheFilesystem is unavailable off Linux: no clamp (the notice is optional
// safety, never a correctness requirement).
func cacheFilesystem(string) (cacheFSSize, error) {
	return cacheFSSize{}, errors.New("statfs unavailable")
}
