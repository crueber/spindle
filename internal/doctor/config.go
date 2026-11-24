// config.go — the config half of `spindle doctor`. It reuses the shipped
// validators (config.Validate and the strict loader's unknown/removed-key
// errors); it never re-implements a rule.
package doctor

import (
	"regexp"
	"strings"

	"github.com/crueber/spindle/internal/config"
)

// configKeyRe finds the key path a validator message names. Validator
// messages are written to start with (or name) their key, so the first match
// is the offending path; nested slice elements (bundles.strategy[0].name)
// match in full.
var configKeyRe = regexp.MustCompile(`[a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+(?:\[\d+\])?(?:\.[a-z0-9_]+)*`)

// quotedKeyRe finds the key in the strict loader's own messages:
// `unsupported key "server.auth.mode" (line 3)` and `unknown key "x" (line 2)`.
var quotedKeyRe = regexp.MustCompile(`(?:unsupported|unknown) key "([^"]+)"`)

// ConfigKey extracts the key path a validator or loader message names, or
// "config" when it names none (the finding still carries the message).
func ConfigKey(message string) string {
	if m := quotedKeyRe.FindStringSubmatch(message); m != nil {
		return m[1]
	}
	if m := configKeyRe.FindString(message); m != "" {
		return m
	}
	return "config"
}

// ConfigFindings turns config.Validate's output into findings: one per error
// (CheckConfig) and one per warning (CheckConfigWarn). Both count toward the
// exit code because the doctor is a readiness gate, not a linter the operator
// can ignore.
func ConfigFindings(warnings []string, errs []error) []Finding {
	out := make([]Finding, 0, len(warnings)+len(errs))
	for _, e := range errs {
		if e == nil {
			continue
		}
		out = append(out, Finding{Check: CheckConfig, Key: ConfigKey(e.Error()), Reason: e.Error()})
	}
	for _, w := range warnings {
		out = append(out, Finding{Check: CheckConfigWarn, Key: ConfigKey(w), Reason: w})
	}
	return dedupe(out)
}

// LoadFindings turns the strict loader's errors into findings. Errors the
// loader produces for unknown and removed keys are CheckConfigKey (the key is
// quoted); anything else is a CheckConfigLoad finding. The caller passes the
// errors from config.LoadSetupOnly (the strict loader's own best-effort
// sibling) when the strict load failed before producing a config.
func LoadFindings(errs []error) []Finding {
	out := make([]Finding, 0, len(errs))
	for _, e := range errs {
		if e == nil {
			continue
		}
		msg := e.Error()
		check := CheckConfigLoad
		if quotedKeyRe.MatchString(msg) {
			check = CheckConfigKey
		}
		out = append(out, Finding{Check: check, Key: ConfigKey(msg), Reason: msg})
	}
	return dedupe(out)
}

// dedupe drops identical findings (the strict loader reports validation
// failures and the config dump's own validation can name the same message).
func dedupe(in []Finding) []Finding {
	seen := map[Finding]bool{}
	out := in[:0]
	for _, f := range in {
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// CheckConfigWarn is a config.Validate warning.
const CheckConfigWarn = "config.warn"

// ValidateConfig is the exported convenience the CLI uses: run the shipped
// validator and convert its output. It exists so cmd/spindle never maps
// messages itself (the mapping stays one function, tested once).
func ValidateConfig(c *config.Config) []Finding {
	return ValidateConfigWithLoad(c, nil)
}

// ValidateConfigWithLoad is ValidateConfig plus the loader's own error when it
// is NOT just the joined validation messages. config.Load validates as part of
// loading, so a non-nil error is usually the same findings Validate returns —
// but an env-override error or a PORT error also carries the config (with
// len(errs) == 0), and swallowing it would let `doctor` report clean on a host
// whose env overlay the server refuses.
func ValidateConfigWithLoad(c *config.Config, loadErr error) []Finding {
	if c == nil {
		if loadErr == nil {
			return nil
		}
		return LoadFindings([]error{loadErr})
	}
	warnings, errs := config.Validate(c)
	out := ConfigFindings(warnings, errs)
	if loadErr == nil {
		return out
	}
	joined := make([]string, len(errs))
	for i, e := range errs {
		joined[i] = e.Error()
	}
	if loadErr.Error() == strings.Join(joined, "; ") {
		return out // the loader error is exactly these findings
	}
	return dedupe(append(out, LoadFindings([]error{loadErr})...))
}

// OverrideFindings reports the loader's ignored env overrides that name an
// unknown key (the `--strict` detection, reused). A legacy-alias notice
// ("overridden by SPINDLE__…") is informational and is not a finding: the
// effective value is the one the operator asked for.
func OverrideFindings(overrides []config.Override) []Finding {
	out := make([]Finding, 0, len(overrides))
	for _, o := range overrides {
		if !strings.HasPrefix(o.Reason, "unknown key") {
			continue
		}
		key := o.Key
		if key == "" {
			key = ConfigKey(o.Name)
		}
		out = append(out, Finding{
			Check:  CheckConfigKey,
			Key:    key,
			Reason: "ignored override " + o.Name + " (" + o.Reason + "): the instance does not receive this setting",
		})
	}
	return out
}
