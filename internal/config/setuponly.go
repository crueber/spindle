package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// LoadSetupOnly resolves the BEST-EFFORT effective config for SETUP-ONLY MODE
// (§3.4 in docs/go/06_server_http.md): compiled-in defaults ⊕ every field of the
// first existing candidate file that decodes ⊕ the valid env overlay (+ PORT
// lockstep). A file that fails validation still contributes all of its
// parseable values, so the listener stays where the operator configured it, the
// setup page pre-fills from the file instead of from defaults, and fixing one
// key cannot silently move the port (F5).
//
// Unlike Load, nothing here fails: an unparseable/unknown key is reported and
// skipped, a bad env override is reported and the rest applies, and an
// unparseable server.listen falls back to the compiled-in default (a listener
// that cannot bind would make the recovery page unreachable) while its error is
// returned for the banner.
func LoadSetupOnly(dataDir string, candidates []string, getenv func(string) string) (*Config, []error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var errs []error
	c := Defaults()
	loaded := ""
	for _, path := range candidates {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			errs = append(errs, rerr)
			continue
		}
		loaded = path
		// Decode into the defaults even when it errors: TOML fills the fields
		// it could read before the bad one, which is exactly the best effort.
		md, derr := toml.Decode(string(src), c)
		if derr != nil {
			errs = append(errs, fmt.Errorf("%s: %v", path, derr))
		}
		lines := strings.Split(string(src), "\n")
		for _, key := range md.Undecoded() {
			if len(key) > 1 && key[0] == "server" && key[1] == "tls" {
				errs = append(errs, fmt.Errorf("%s: unsupported key %q (line %d): %s",
					path, key.String(), findKeyLine(lines, key), removedTLSHint))
				continue
			}
			errs = append(errs, fmt.Errorf("%s: unknown key %q (line %d)",
				path, key.String(), findKeyLine(lines, key)))
		}
		break
	}
	if loaded == "" {
		// No file: setup-only mode is unreachable, but keep the shape honest
		// (the first-run filesystem store, not the s3 default).
		c = FirstRunDefaults(dataDir)
	}
	c.DataDir = dataDir

	if _, eerr := applyEnv(c, getenv, os.Environ()); eerr != nil {
		errs = append(errs, fmt.Errorf("env overlay: %v", eerr))
	}
	if perr := applyPort(c, getenv); perr != nil {
		errs = append(errs, perr)
	}
	if _, _, lerr := net.SplitHostPort(c.Server.Listen); lerr != nil {
		errs = append(errs, fmt.Errorf("server.listen %q must be host:port (binding %s instead)",
			c.Server.Listen, Defaults().Server.Listen))
		c.Server.Listen = Defaults().Server.Listen
	}
	return c, errs
}
