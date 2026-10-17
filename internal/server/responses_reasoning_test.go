package server

import (
	"encoding/json"
	"testing"
)

// TestApplyResponsesReasoning 锁定 Responses → Chat 的思考档位翻译。
//
// 背景：适配层曾只解析 reasoning 却不转发，导致 Codex 的 model_reasoning_effort
// 被静默丢弃、模型回落到自身默认档。这里把映射关系钉死，防止再次退化。
func TestApplyResponsesReasoning(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantKey string
		wantVal string
		wantNil bool
	}{
		{"high 档透传", `{"effort":"high"}`, "reasoning_effort", "high", false},
		{"low 档透传", `{"effort":"low"}`, "reasoning_effort", "low", false},
		{"带 summary 也只取 effort", `{"effort":"high","summary":"auto"}`, "reasoning_effort", "high", false},
		{"显式 none 关思考", `{"effort":"none"}`, "thinking", "disabled", false},
		{"空对象不动", `{}`, "", "", true},
		{"仅 summary 不动", `{"summary":"auto"}`, "", "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chat := map[string]any{}
			applyResponsesReasoning(&respRequest{Reasoning: json.RawMessage(c.raw)}, chat)

			if c.wantNil {
				if _, ok := chat["reasoning_effort"]; ok {
					t.Fatalf("reasoning_effort 不该出现: %v", chat)
				}
				if _, ok := chat["thinking"]; ok {
					t.Fatalf("thinking 不该出现: %v", chat)
				}
				return
			}
			got := chat[c.wantKey]
			if c.wantKey == "thinking" {
				m, ok := got.(map[string]any)
				if !ok || m["type"] != c.wantVal {
					t.Fatalf("thinking = %v, want {type:%s}", got, c.wantVal)
				}
				return
			}
			if got != c.wantVal {
				t.Fatalf("%s = %v, want %s", c.wantKey, got, c.wantVal)
			}
		})
	}
}

// TestBuildChatBodyCarriesReasoning 端到端确认 effort 进了出站 chat body。
func TestBuildChatBodyCarriesReasoning(t *testing.T) {
	req := &respRequest{
		Model:             "cn:step-5-preview",
		Input:             json.RawMessage(`"hi"`),
		Reasoning:         json.RawMessage(`{"effort":"high"}`),
		MaxOutputTokens:   intPtr(64000),
		ParallelToolCalls: boolPtr(true),
	}
	body, err := buildChatBody(req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high (body=%s)", obj["reasoning_effort"], body)
	}
	if obj["max_tokens"] != float64(64000) {
		t.Fatalf("max_tokens = %v, want 64000", obj["max_tokens"])
	}
}

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }
