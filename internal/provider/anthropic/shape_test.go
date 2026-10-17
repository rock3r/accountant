package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// shapedSystemTexts returns the text of each system block after shaping.
func shapedSystemTexts(t *testing.T, out []byte) []string {
	t.Helper()
	var obj struct {
		System []struct {
			Text         string `json:"text"`
			CacheControl struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"system"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("shaped body does not parse: %v\n%s", err, out)
	}
	texts := make([]string, len(obj.System))
	for i, b := range obj.System {
		texts[i] = b.Text
		if i == 0 && b.CacheControl.Type != "ephemeral" {
			t.Fatalf("injected block missing cache_control: %+v", b)
		}
	}
	return texts
}

func TestShapeClaudeCodeCompat(t *testing.T) {
	cc := claudeCodeSystemPrompt
	cases := []struct {
		name string
		body string
		want []string // nil: expect byte-identical passthrough
	}{
		{"no system", `{"model":"m","messages":[]}`, []string{cc}},
		{"null system", `{"system":null}`, []string{cc}},
		{"string system already cc", `{"system":"` + cc + `"}`, nil},
		{"string system other", `{"system":"You are pi."}`, []string{cc, "You are pi."}},
		{"empty array", `{"system":[]}`, []string{cc}},
		{"array already cc first", `{"system":[{"type":"text","text":"` + cc + `"},{"type":"text","text":"x"}]}`, nil},
		{"billing header then cc", `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.0.76.ab12; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"` + cc + `"}]}`, nil},
		{"cc second without billing header", `{"system":[{"type":"text","text":"You are pi."},{"type":"text","text":"` + cc + `"}]}`, []string{cc, "You are pi.", cc}},
		{"array other", `{"system":[{"type":"text","text":"You are pi."}]}`, []string{cc, "You are pi."}},
		{"not json", `garbage{`, nil},
		{"json array top level", `["not","an","object"]`, nil},
		{"system wrong type", `{"system":42}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := ShapeClaudeCodeCompat([]byte(tc.body))
			if tc.want == nil {
				if !bytes.Equal(out, []byte(tc.body)) {
					t.Fatalf("expected byte-identical passthrough, got %s", out)
				}
				return
			}
			if bytes.Equal(out, []byte(tc.body)) {
				t.Fatalf("expected shaping, body unchanged")
			}
			got := shapedSystemTexts(t, out)
			if len(got) != len(tc.want) {
				t.Fatalf("system blocks = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("system[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestShapeClaudeCodeCompatPreservesOtherFields(t *testing.T) {
	body := `{"model":"claude-haiku-5-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}],"system":"You are pi."}`
	out := ShapeClaudeCodeCompat([]byte(body))
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("shaped body does not parse: %v", err)
	}
	for _, k := range []string{"model", "max_tokens", "stream", "messages", "system"} {
		if _, ok := obj[k]; !ok {
			t.Fatalf("shaped body lost field %q: %s", k, out)
		}
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Fatalf("stream flag mangled: %s", out)
	}
}

// A second pass over shaped output must be a no-op.
func TestShapeClaudeCodeCompatIdempotent(t *testing.T) {
	body := `{"messages":[],"system":"You are pi."}`
	once := ShapeClaudeCodeCompat([]byte(body))
	twice := ShapeClaudeCodeCompat(once)
	if !bytes.Equal(once, twice) {
		t.Fatalf("not idempotent:\n%s\n%s", once, twice)
	}
}
