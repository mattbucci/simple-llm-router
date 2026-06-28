package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes body to a temp file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestLoadAuthFailClosed covers ADR-0009/0010: an auth.tokens entry that resolves
// to empty — the hallmark of an unset ${ENV} placeholder, which interpolates to a
// null/blank sequence entry — must make config.Load FAIL closed rather than
// silently degrade into an open (auth-disabled) router. An explicit empty list
// (tokens: []), an absent auth block, and a token that resolves to a real value
// all still load and mean "trust the LAN" / "require this key".
func TestLoadAuthFailClosed(t *testing.T) {
	// base supplies the otherwise-valid remainder of the config so the only
	// variable under test is the auth block.
	const base = `
backends:
  - name: b1
    base_url: http://localhost:8000/v1
`
	cases := []struct {
		name       string
		env        string // value bound to ROUTER_API_KEY ("" => unset/empty)
		auth       string // the auth block prepended to base
		wantErr    bool
		errSubstr  string
		wantTokens int // expected len(cfg.Auth.Tokens) on success
	}{
		{
			name:      "unset env token fails closed",
			env:       "",
			auth:      "auth:\n  tokens:\n    - ${ROUTER_API_KEY}\n",
			wantErr:   true,
			errSubstr: "auth.tokens",
		},
		{
			name:       "explicit empty list loads (trust the LAN)",
			auth:       "auth:\n  tokens: []\n",
			wantErr:    false,
			wantTokens: 0,
		},
		{
			name:       "absent auth block loads",
			auth:       "",
			wantErr:    false,
			wantTokens: 0,
		},
		{
			name:       "resolved env token loads",
			env:        "s3cret",
			auth:       "auth:\n  tokens:\n    - ${ROUTER_API_KEY}\n",
			wantErr:    false,
			wantTokens: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROUTER_API_KEY", tc.env)
			path := writeConfig(t, tc.auth+base)

			cfg, err := Load(path)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load = nil error, want a fail-closed error")
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("error %q does not mention %q", err.Error(), tc.errSubstr)
				}
				if cfg != nil {
					t.Fatalf("cfg = %+v, want nil on error", cfg)
				}
				return
			}

			if err != nil {
				t.Fatalf("Load: unexpected error %v", err)
			}
			if cfg == nil {
				t.Fatalf("cfg = nil, want a loaded config")
			}
			if len(cfg.Auth.Tokens) != tc.wantTokens {
				t.Fatalf("auth tokens = %d (%v), want %d", len(cfg.Auth.Tokens), cfg.Auth.Tokens, tc.wantTokens)
			}
			if tc.wantTokens == 1 && cfg.Auth.Tokens[0] != tc.env {
				t.Fatalf("auth token = %q, want %q", cfg.Auth.Tokens[0], tc.env)
			}
		})
	}
}

// TestLoadSelectorValidation covers ADR-0010: the selector enum is validated
// independently of the alias type, so an illegal value must be rejected on BOTH a
// proxy alias and a fusion alias (where the selector is otherwise unused) before
// the type-specific checks run. Valid proxy and fusion aliases must still load.
func TestLoadSelectorValidation(t *testing.T) {
	// backends supplies a single valid upstream every alias under test references.
	const backends = `
backends:
  - name: b1
    base_url: http://localhost:8000/v1
`
	cases := []struct {
		name      string
		alias     string // the aliases block prepended to backends
		wantErr   bool
		errSubstr string
	}{
		{
			name: "proxy alias with bogus selector is rejected",
			alias: `
aliases:
  fast:
    type: proxy
    selector: bogus
    model: upstream-model
    backends: [b1]
`,
			wantErr:   true,
			errSubstr: "selector",
		},
		{
			name: "fusion alias with bogus selector is rejected",
			alias: `
aliases:
  panel:
    type: fusion
    selector: bogus
    panel:
      - model: m1
        backends: [b1]
    judge:
      model: judge-model
      backends: [b1]
    synthesis:
      model: synth-model
      backends: [b1]
`,
			wantErr:   true,
			errSubstr: "selector",
		},
		{
			name: "valid proxy round_robin alias loads",
			alias: `
aliases:
  fast:
    type: proxy
    selector: round_robin
    model: upstream-model
    backends: [b1]
`,
			wantErr: false,
		},
		{
			name: "valid proxy pareto alias loads",
			alias: `
aliases:
  smart:
    type: proxy
    selector: pareto
    pool:
      - model: m1
        backends: [b1]
        quality: 0.9
`,
			wantErr: false,
		},
		{
			name: "valid fusion alias (default selector) loads",
			alias: `
aliases:
  panel:
    type: fusion
    panel:
      - model: m1
        backends: [b1]
    judge:
      model: judge-model
      backends: [b1]
    synthesis:
      model: synth-model
      backends: [b1]
`,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, backends+tc.alias)

			cfg, err := Load(path)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load = nil error, want a validation error")
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("error %q does not mention %q", err.Error(), tc.errSubstr)
				}
				if cfg != nil {
					t.Fatalf("cfg = %+v, want nil on error", cfg)
				}
				return
			}

			if err != nil {
				t.Fatalf("Load: unexpected error %v", err)
			}
			if cfg == nil {
				t.Fatalf("cfg = nil, want a loaded config")
			}
		})
	}
}
