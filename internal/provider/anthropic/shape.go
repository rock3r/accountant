package anthropic

import (
	"bytes"
	"encoding/json"
)

// claudeCodeSystemPrompt is the exact first system block Claude Code
// sends. Subscription credentials are only accepted for requests whose
// system prompt begins with this block (probes in docs/s1-results.md), so
// clients that are not Claude Code need it prepended.
const claudeCodeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."

// billingHeaderPrefix marks the attribution block Claude Code prepends
// ahead of its system prompt; the gate skips it.
const billingHeaderPrefix = "x-anthropic-billing-header:"

// claudeCodeBlock is the injected system block.
var claudeCodeBlock = json.RawMessage(
	`{"type":"text","text":"` + claudeCodeSystemPrompt + `","cache_control":{"type":"ephemeral"}}`)

// ShapeRequest implements provider.RequestShaper. It makes the request
// body look like it came from Claude Code by ensuring the system prompt
// starts with Claude Code's own first block. Bodies that already pass the
// gate, or that are not JSON objects, are returned byte-for-byte
// unchanged.
func (p *Provider) ShapeRequest(body []byte) ([]byte, error) {
	return ShapeClaudeCodeCompat(body), nil
}

// ShapeClaudeCodeCompat prepends the Claude Code system block unless the
// system prompt already starts with it (or with Claude Code's billing
// header block followed by it). Anything unexpected is returned unchanged.
func ShapeClaudeCodeCompat(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body // not a JSON object: pass through, upstream will say so
	}

	raw, ok := obj["system"]
	if ok {
		trimmed := bytes.TrimSpace(raw)
		switch {
		case bytes.Equal(trimmed, []byte("null")):
			// treat as absent
		case len(trimmed) > 0 && trimmed[0] == '"':
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return body
			}
			if s == claudeCodeSystemPrompt {
				return body // already shaped
			}
			clientBlock, err := json.Marshal(map[string]string{"type": "text", "text": s})
			if err != nil {
				return body
			}
			obj["system"] = concatBlocks(claudeCodeBlock, []json.RawMessage{clientBlock})
			return marshalOrOriginal(obj, body)
		case len(trimmed) > 0 && trimmed[0] == '[':
			var blocks []json.RawMessage
			if err := json.Unmarshal(raw, &blocks); err != nil {
				return body
			}
			if alreadyShaped(blocks) {
				return body
			}
			obj["system"] = concatBlocks(claudeCodeBlock, blocks)
			return marshalOrOriginal(obj, body)
		default:
			return body
		}
	}

	obj["system"] = concatBlocks(claudeCodeBlock, nil)
	return marshalOrOriginal(obj, body)
}

// alreadyShaped reports whether the system blocks already pass the gate:
// the first block is the Claude Code prompt, or the first block is the
// billing header and the second is the prompt.
func alreadyShaped(blocks []json.RawMessage) bool {
	textAt := func(i int) string {
		if i >= len(blocks) {
			return ""
		}
		var b struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(blocks[i], &b); err != nil {
			return ""
		}
		return b.Text
	}
	if textAt(0) == claudeCodeSystemPrompt {
		return true
	}
	return bytes.HasPrefix([]byte(textAt(0)), []byte(billingHeaderPrefix)) &&
		textAt(1) == claudeCodeSystemPrompt
}

// concatBlocks builds a JSON array with head first, then rest.
func concatBlocks(head json.RawMessage, rest []json.RawMessage) json.RawMessage {
	out := make([]byte, 0, len(head)+64)
	out = append(out, '[')
	out = append(out, head...)
	for _, b := range rest {
		out = append(out, ',')
		out = append(out, b...)
	}
	out = append(out, ']')
	return out
}

func marshalOrOriginal(obj map[string]json.RawMessage, original []byte) []byte {
	out, err := json.Marshal(obj)
	if err != nil {
		return original
	}
	return out
}
