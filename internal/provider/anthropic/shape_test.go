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
		{"string system other", `{"system":"You are pi, a coding agent."}`, []string{cc, "You are agent, a coding agent."}},
		{"empty array", `{"system":[]}`, []string{cc}},
		{"array already cc first", `{"system":[{"type":"text","text":"` + cc + `"},{"type":"text","text":"x"}]}`, nil},
		{"billing header then cc", `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.0.76.ab12; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"` + cc + `"}]}`, nil},
		{"cc second without billing header", `{"system":[{"type":"text","text":"You are pi, a coding agent."},{"type":"text","text":"` + cc + `"}]}`, []string{cc, "You are agent, a coding agent.", cc}},
		{"array other", `{"system":[{"type":"text","text":"You are pi, a coding agent."}]}`, []string{cc, "You are agent, a coding agent."}},
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

func TestSanitizeSystemText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"You are pi, a coding agent.", "You are agent, a coding agent."},
		{"Pi is a harness.", "Agent is a harness."},
		{"operating inside pi, a coding agent harness", "operating inside agent, a coding agent harness"},
		{"pipeline", "pipeline"},   // embedded: untouched
		{"api", "api"},             // embedded: untouched
		{"Pi\npi", "Agent\nagent"}, // both sides of a newline
		{"run pi config", "run agent config"},
		{"pi.h", "pi.h"},           // dotted: untouched
		{"pi.md", "pi.md"},         // dotted: untouched
		{"README.md", "README.md"}, // file name: untouched
		{"read .md files", "read markdown files"},
		{"docs/AGENTS.md", "docs/AGENTS.md"}, // path: untouched
		{"follow .md cross-references", "follow markdown cross-references"},
		{"a .mdx file", "a .mdx file"}, // .mdx: word char after d
		{"x.md, y", "x.md, y"},         // preceded by word char
		{"Claude Code, Anthropic's official CLI for Claude.", "Claude Code, Anthropic's official CLI for Claude."},
	}
	for _, tc := range cases {
		if got := SanitizeSystemText(tc.in); got != tc.want {
			t.Errorf("SanitizeSystemText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
