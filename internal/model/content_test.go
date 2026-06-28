package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestContentNeverAssumedString covers ADR-0008: message content may be a plain
// string OR an ordered array of typed parts, and unknown part types must survive
// verbatim. The router must never assume content is a string.
func TestContentNeverAssumedString(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantStr   *string
		wantParts int
		wantText  string
		// wantMarshal, when non-empty, asserts the re-emitted JSON is semantically
		// equal to this value.
		wantMarshal string
	}{
		{
			name:        "plain string",
			in:          `"hello world"`,
			wantStr:     strptr("hello world"),
			wantText:    "hello world",
			wantMarshal: `"hello world"`,
		},
		{
			name:        "multimodal array with unknown part",
			in:          `[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"video","src":"v.mp4","frames":24}]`,
			wantParts:   3,
			wantText:    "describe this",
			wantMarshal: `[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"video","src":"v.mp4","frames":24}]`,
		},
		{
			name:        "null content",
			in:          `null`,
			wantMarshal: `null`,
		},
		{
			name:      "object content kept as single opaque part",
			in:        `{"type":"weird","payload":1}`,
			wantParts: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c Content
			if err := json.Unmarshal([]byte(tc.in), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			switch {
			case tc.wantStr != nil:
				if c.Str == nil || *c.Str != *tc.wantStr {
					t.Fatalf("Str = %v, want %q", c.Str, *tc.wantStr)
				}
				if c.Parts != nil {
					t.Fatalf("Parts = %v, want nil for a string content", c.Parts)
				}
			default:
				if c.Str != nil {
					t.Fatalf("Str = %q, want nil for non-string content", *c.Str)
				}
			}

			if tc.wantParts > 0 && len(c.Parts) != tc.wantParts {
				t.Fatalf("len(Parts) = %d, want %d", len(c.Parts), tc.wantParts)
			}
			if got := c.Text(); got != tc.wantText {
				t.Fatalf("Text() = %q, want %q", got, tc.wantText)
			}

			if tc.wantMarshal != "" {
				got, err := json.Marshal(c)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if !jsonEqual(t, got, []byte(tc.wantMarshal)) {
					t.Fatalf("re-marshal = %s, want %s", got, tc.wantMarshal)
				}
			}
		})
	}
}

// TestContentArrayPreservesUnknownPartBytes asserts the verbatim bytes of an
// unknown part survive a decode/encode cycle (ADR-0001, ADR-0008).
func TestContentArrayPreservesUnknownPartBytes(t *testing.T) {
	in := `[{"type":"input_audio","input_audio":{"data":"Zm9v","format":"wav"}}]`
	var c Content
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(c.Parts) != 1 {
		t.Fatalf("len(Parts) = %d, want 1", len(c.Parts))
	}
	// The single part's raw bytes must be byte-identical to the input element.
	want := `{"type":"input_audio","input_audio":{"data":"Zm9v","format":"wav"}}`
	if string(c.Parts[0]) != want {
		t.Fatalf("part bytes = %s, want %s", c.Parts[0], want)
	}
}

// TestParsePlugins covers the reserved plugins routing-control parsing (ADR-0001,
// ADR-0013), including the per-request min_quality override lookup.
func TestParsePlugins(t *testing.T) {
	raw := json.RawMessage(`[{"id":"pareto","min_quality":0.7},{"id":"other","x":1}]`)
	plugins := ParsePlugins(raw)
	if len(plugins) != 2 {
		t.Fatalf("len = %d, want 2", len(plugins))
	}
	if plugins[0].ID != "pareto" {
		t.Fatalf("plugins[0].ID = %q, want pareto", plugins[0].ID)
	}
	q, ok := plugins[0].Float("min_quality")
	if !ok || q != 0.7 {
		t.Fatalf("Float(min_quality) = %v,%v want 0.7,true", q, ok)
	}

	req := &ChatRequest{Plugins: plugins}
	if v, ok := req.PluginParam("pareto", "min_quality"); !ok || v != 0.7 {
		t.Fatalf("PluginParam = %v,%v want 0.7,true", v, ok)
	}
	if _, ok := req.PluginParam("pareto", "missing"); ok {
		t.Fatalf("PluginParam(missing) ok = true, want false")
	}

	// A malformed plugins array is advisory and must yield nil, not an error.
	if got := ParsePlugins(json.RawMessage(`{"not":"an array"}`)); got != nil {
		t.Fatalf("ParsePlugins(malformed) = %v, want nil", got)
	}
}

func strptr(s string) *string { return &s }

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	return reflect.DeepEqual(av, bv)
}
