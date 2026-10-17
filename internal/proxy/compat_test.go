package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"accountant/internal/config"
)

// enableCompat adds a second client flagged claude_code_compat.
func (r *rig) enableCompat(t *testing.T, id, key string) {
	t.Helper()
	cfg, err := config.Load(r.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Clients = append(cfg.Clients, config.Client{ID: id, KeySHA256: HashKey(key), ClaudeCodeCompat: true})
	if err := config.Save(r.cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestCompatClientBodyShaped(t *testing.T) {
	r := newRig(t, []config.Account{acctCfg("a1", 10)})
	r.enableCompat(t, "pi", "pi-client-key")

	body := []byte(`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}],"system":"You are pi, a coding agent."}`)
	resp := r.doRequest(t, http.MethodPost, "/v1/messages", body, map[string]string{"x-api-key": "pi-client-key"})
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	up := r.up.requestsFor("tok-a1")
	if len(up) != 1 {
		t.Fatalf("upstream requests = %d", len(up))
	}
	var got struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(up[0].Body, &got); err != nil {
		t.Fatalf("upstream body does not parse: %v\n%s", err, up[0].Body)
	}
	if len(got.System) != 2 || got.System[0].Text != "You are Claude Code, Anthropic's official CLI for Claude." ||
		got.System[1].Text != "You are agent, a coding agent." {
		t.Fatalf("upstream system blocks = %+v", got.System)
	}
}

func TestNonCompatClientByteIdentical(t *testing.T) {
	r := newRig(t, []config.Account{acctCfg("a1", 10)})
	r.enableCompat(t, "pi", "pi-client-key")

	body := []byte(`{"model":"claude-haiku-5-5","messages":[],"system":"You are Claude Code, Anthropic's official CLI for Claude."}`)
	resp := r.doRequest(t, http.MethodPost, "/v1/messages", body, nil) // default client c1
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	up := r.up.requestsFor("tok-a1")
	if len(up) != 1 || !bytes.Equal(up[0].Body, body) {
		t.Fatalf("non-compat body not byte-identical: %s", up[0].Body)
	}
}

// A compat client sending an already-shaped body stays byte-identical.
func TestCompatClientAlreadyShapedUntouched(t *testing.T) {
	r := newRig(t, []config.Account{acctCfg("a1", 10)})
	r.enableCompat(t, "pi", "pi-client-key")

	body := []byte(`{"messages":[],"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}]}`)
	resp := r.doRequest(t, http.MethodPost, "/v1/messages", body, map[string]string{"x-api-key": "pi-client-key"})
	_ = resp.Body.Close()

	up := r.up.requestsFor("tok-a1")
	if len(up) != 1 || !bytes.Equal(up[0].Body, body) {
		t.Fatalf("already-shaped body not byte-identical: %s", up[0].Body)
	}
}
