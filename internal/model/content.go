package model

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Content is a message body that is either a plain string or an ordered list of
// typed parts (multimodal). Unknown part types are preserved verbatim so the
// router never corrupts a multimodal request (ADR-0008). Code must never assume
// content is a string.
type Content struct {
	// Str holds the value when content was a JSON string.
	Str *string
	// Parts holds the raw parts when content was a JSON array. Each element is
	// the verbatim JSON of one part (text, image_url, input_audio, video, …).
	Parts []json.RawMessage
}

// StringContent builds plain-text Content.
func StringContent(s string) Content { return Content{Str: &s} }

// UnmarshalJSON accepts a string, an array of parts, an object, or null without
// discarding anything.
func (c *Content) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		c.Str = &s
	case '[':
		return json.Unmarshal(b, &c.Parts)
	default:
		// An object or scalar: keep it as a single opaque part rather than drop it.
		c.Parts = []json.RawMessage{append(json.RawMessage(nil), b...)}
	}
	return nil
}

// MarshalJSON re-emits the original shape (string, array, or null).
func (c Content) MarshalJSON() ([]byte, error) {
	if c.Str != nil {
		return json.Marshal(*c.Str)
	}
	if c.Parts != nil {
		return json.Marshal(c.Parts)
	}
	return []byte("null"), nil
}

// Text returns a best-effort plain-text view: the string content, or the joined
// "text" fields of any text parts. Used only for cross-protocol translation and
// fusion prompt assembly (ADR-0014, ADR-0016); the same-protocol path never
// flattens content.
func (c Content) Text() string {
	if c.Str != nil {
		return *c.Str
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		var tp struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(p, &tp); err == nil && tp.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(tp.Text)
		}
	}
	return sb.String()
}
