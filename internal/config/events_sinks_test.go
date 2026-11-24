package config

import (
	"strings"
	"testing"
)

func validateSinkCfg(sinks ...EventSink) []error {
	c := Defaults()
	// OIDC is mandatory now, so a fixture that only cares about sinks still
	// needs a signable, admission-controlled auth block — otherwise every
	// assertion below would be buried under unrelated validation errors.
	c.Server.Auth.Issuer = "https://id.example.com"
	c.Server.Auth.AllowedDomains = []string{"example.com"}
	c.Server.Auth.SessionSecret = strings.Repeat("s", 32)
	c.Server.Auth.OAuthClientID = "spindle"
	c.Server.Auth.OAuthClientSecret = "secret"
	c.Server.Auth.AdminGroups = []string{"spindle-admin"}
	c.Events.Sinks = sinks
	_, errs := Validate(c)
	return errs
}

func TestEventSinksValidation(t *testing.T) {
	ok := validateSinkCfg(
		EventSink{Name: "ci", Kind: EventSinkWebhook, URL: "https://ci.example/hook", SecretEnv: "HOOK_SECRET"},
		EventSink{Name: "audit", Kind: EventSinkJSONL, Path: "/var/lib/spindle/audit.jsonl", MaxBytes: 1 << 20, Keep: 2},
	)
	if len(ok) != 0 {
		t.Fatalf("valid sinks rejected: %v", ok)
	}

	cases := []struct{ name, want string }{
		{"unknown kind", `events.sinks[0].kind must be "webhook" or "jsonl"`},
		{"webhook needs url", `events.sinks[0].url is required`},
		{"jsonl needs path", `events.sinks[0].path is required`},
		{"jsonl rejects url", `events.sinks[0].url is a webhook key`},
		{"webhook rejects path", `events.sinks[0].path is a jsonl key`},
		{"negative max_bytes", `events.sinks[0].max_bytes must be positive`},
		{"negative keep", `events.sinks[0].keep must be >= 1`},
	}
	for _, tc := range cases {
		var sink EventSink
		switch tc.name {
		case "unknown kind":
			sink = EventSink{Kind: "pigeon"}
		case "webhook needs url":
			sink = EventSink{Kind: EventSinkWebhook}
		case "jsonl needs path":
			sink = EventSink{Kind: EventSinkJSONL}
		case "jsonl rejects url":
			sink = EventSink{Kind: EventSinkJSONL, Path: "/tmp/a.jsonl", URL: "https://x"}
		case "webhook rejects path":
			sink = EventSink{Kind: EventSinkWebhook, URL: "https://x", Path: "/tmp/a.jsonl"}
		case "negative max_bytes":
			sink = EventSink{Kind: EventSinkJSONL, Path: "/tmp/a.jsonl", MaxBytes: -1}
		case "negative keep":
			sink = EventSink{Kind: EventSinkJSONL, Path: "/tmp/a.jsonl", Keep: -1}
		}
		errs := validateSinkCfg(sink)
		if len(errs) == 0 || !strings.Contains(errs[0].Error(), tc.want) {
			t.Errorf("%s: errs = %v, want %q", tc.name, errs, tc.want)
		}
	}
}

func TestEventSinkNamesAreUnique(t *testing.T) {
	errs := validateSinkCfg(
		EventSink{Name: "dup", Kind: EventSinkWebhook, URL: "https://a"},
		EventSink{Name: "dup", Kind: EventSinkWebhook, URL: "https://b"},
	)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), `"dup" is used twice`) {
		t.Fatalf("duplicate names: %v", errs)
	}
}
