package server

// Responses API 适配层（/v1/responses）。
//
// 背景：Codex CLI 自 2026-02 起移除 chat/completions 协议支持，WireApi 只剩
// "responses"，即必须存在真正的 /v1/responses 端点。本网关的核心链路（选号、
// 轮换、熔断、限流冷却、会话粘性、用量记录）全部沉淀在 chatCompletions 里，
// 重写一遍代价高且必然与原链路漂移，因此这里做**协议翻译**而不是另起炉灶：
// Responses 请求 → Chat Completions 请求 → 自环打回本网关 → 响应翻译回 Responses。
//
// 自环只走本机回环地址（由 config.listen 推导），不经过外网；代价是一次本地
// HTTP 往返，换来的是与 /v1/chat/completions 完全一致的调度语义。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ---------- 请求侧 ----------

type respRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       json.RawMessage `json:"instructions"`
	Tools              []respTool      `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	Stream             bool            `json:"stream"`
	Temperature        *float64        `json:"temperature"`
	TopP               *float64        `json:"top_p"`
	MaxOutputTokens    *int            `json:"max_output_tokens"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls"`
	Reasoning          json.RawMessage `json:"reasoning"`
	Store              *bool           `json:"store"`
	PreviousResponseID string          `json:"previous_response_id"`
}

type respTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      *bool          `json:"strict"`
}

type respInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Status    string          `json:"status"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    string          `json:"output"`
}

type respContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ---------- 响应侧 ----------

type chatStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string         `json:"role"`
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type chatCompletionResp struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int `json:"index"`
		FinishReason *string
		Message      struct {
			Role             string         `json:"role"`
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type chatToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// responses 是 POST /v1/responses 入口。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeRespError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req respRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRespError(w, http.StatusBadRequest, "invalid_request", "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeRespError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}

	chatBody, err := buildChatBody(&req)
	if err != nil {
		writeRespError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	up, err := h.loopbackChat(r.Context(), chatBody)
	if err != nil {
		writeRespError(w, http.StatusBadGateway, "upstream_error", "loopback: "+err.Error())
		return
	}
	defer up.Body.Close()

	if up.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(up.Body, 64<<10))
		writeRespError(w, up.StatusCode, "upstream_error", string(raw))
		return
	}

	if req.Stream {
		h.streamResponses(w, up.Body, req.Model)
		return
	}
	h.nonStreamResponses(w, up.Body, req.Model)
}

// ---------- 请求翻译 ----------

// applyResponsesReasoning 把 Responses 的 reasoning 对象翻译成网关 chat 侧认得的思考控制字段。
//
// 上游判定开思考的条件是 isThinkingEnabled = !!(reasoning_summary || reasoning_effort || reasoning?.effort)
// （逆向结论见 internal/upstream/thinking.go）。此前适配层只解析 reasoning 却从不转发，
// 于是 Codex 侧的 model_reasoning_effort=high 被静默丢弃，模型回落到自己的默认档（step-5 是 medium）。
//
// 映射：effort 非空且非 none → reasoning_effort；显式 none/off/disabled → thinking.type=disabled。
// summary 一律不透传：它只是思考摘要的呈现开关， reasoning_effort 已足以让上游开思考，
// 多传一个上游未必认的枚举值反而增加出错面。
// 未携带 reasoning 时保持零改动，交给上游默认行为（含 only_reasoning 模型的兜底注入）。
func applyResponsesReasoning(req *respRequest, chat map[string]any) {
	if len(req.Reasoning) == 0 {
		return
	}
	var r struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(req.Reasoning, &r); err != nil {
		return // 非对象形态（历史版本可能是裸值）忽略，不打断主流程
	}
	switch strings.TrimSpace(strings.ToLower(r.Effort)) {
	case "":
		return
	case "none", "off", "disabled":
		chat["thinking"] = map[string]any{"type": "disabled"}
	default:
		chat["reasoning_effort"] = r.Effort
	}
}

func buildChatBody(req *respRequest) ([]byte, error) {
	msgs, err := responsesInputToMessages(req)
	if err != nil {
		return nil, err
	}
	chat := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	applyResponsesReasoning(req, chat)
	if req.MaxOutputTokens != nil {
		chat["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "" && t.Type != "function" {
				continue // 目前只映射 function 工具（web_search 等内置工具上游不认）
			}
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Parameters != nil {
				fn["parameters"] = t.Parameters
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			if t.Strict != nil {
				fn["strict"] = *t.Strict
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if len(tools) > 0 {
			chat["tools"] = tools
			if tc := translateToolChoice(req.ToolChoice); tc != nil {
				chat["tool_choice"] = tc
			}
		}
	}
	if req.ParallelToolCalls != nil {
		chat["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	return json.Marshal(chat)
}

func translateToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return s
		default:
			return nil // 未知字符串交给上游默认行为，别冒进
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Type == "function" {
		name := obj.Name
		if name == "" {
			name = obj.Function.Name
		}
		if name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return nil
}

func responsesInputToMessages(req *respRequest) ([]map[string]any, error) {
	out := []map[string]any{}
	if ins := instructionsText(req.Instructions); ins != "" {
		out = append(out, map[string]any{"role": "system", "content": ins})
	}

	if len(req.Input) == 0 {
		return out, nil
	}

	// input 可以是纯字符串
	var asStr string
	if err := json.Unmarshal(req.Input, &asStr); err == nil {
		out = append(out, map[string]any{"role": "user", "content": asStr})
		return out, nil
	}

	var items []respInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, err
	}

	// 挂起中的助手消息：连续的 function_call 合并进同一条 assistant 消息，
	// 否则上游会看到多条 assistant 消息各带一个 tool_call，容易判成非法历史。
	var pendingCalls []map[string]any
	flushCalls := func() {
		if len(pendingCalls) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "assistant", "tool_calls": pendingCalls})
		pendingCalls = nil
	}

	for _, it := range items {
		switch it.Type {
		case "", "message":
			flushCalls()
			role := it.Role
			switch role {
			case "developer":
				role = "system" // 上游不认 developer，降级成 system
			case "":
				role = "user"
			}
			text := partText(it.Content)
			if text == "" {
				continue
			}
			out = append(out, map[string]any{"role": role, "content": text})
		case "function_call":
			callID := it.CallID
			if callID == "" {
				callID = it.ID
			}
			pendingCalls = append(pendingCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      it.Name,
					"arguments": it.Arguments,
				},
			})
		case "function_call_output":
			flushCalls()
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": it.CallID,
				"content":      it.Output,
			})
		default:
			// reasoning / 其他 item 类型：上游无对应表达，丢弃但不要破坏历史结构
			flushCalls()
		}
	}
	flushCalls()
	if len(out) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	return out, nil
}

func instructionsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []respContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

func partText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []respContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "input_text", "output_text", "text", "":
				b.WriteString(p.Text)
			case "input_image", "input_file":
				b.WriteString("[") // 图片/文件上游不可达时给个占位，避免静默丢内容
				b.WriteString(p.Type)
				b.WriteString("]")
			}
		}
		return b.String()
	}
	return ""
}

// ---------- 自环 ----------

// loopbackChat 把翻译后的 chat 请求打回本进程（复用选号/轮换/熔断链路）。
func (h *Handler) loopbackChat(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.loopbackURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := h.loadLive().APIKey; key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("X-Responses-Loopback", "1")
	return http.DefaultClient.Do(req)
}

func (h *Handler) loopbackURL() string {
	return "http://" + loopbackAddr(h.cfg.ListenAddr) + "/v1/chat/completions"
}

// loopbackAddr 由监听地址推导可拨号的本机地址：":7863"/"0.0.0.0:7863" → "127.0.0.1:7863"。
func loopbackAddr(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "127.0.0.1:7863"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		// 没有冒号（如 "7863"）时视为纯端口
		return "127.0.0.1:" + listen
	}
	if port == "" {
		return "127.0.0.1:7863"
	}
	switch host {
	case "", "0.0.0.0", "[::]", "::":
		return "127.0.0.1:" + port
	}
	return listen
}

// ---------- 非流式响应翻译 ----------

func (h *Handler) nonStreamResponses(w http.ResponseWriter, rc io.Reader, model string) {
	raw, err := io.ReadAll(io.LimitReader(rc, 32<<20))
	if err != nil {
		writeRespError(w, http.StatusBadGateway, "upstream_error", "read upstream: "+err.Error())
		return
	}
	var cr chatCompletionResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		writeRespError(w, http.StatusBadGateway, "upstream_error", "bad upstream json: "+err.Error())
		return
	}
	if len(cr.Choices) == 0 {
		writeRespError(w, http.StatusBadGateway, "upstream_error", "empty choices")
		return
	}

	msg := cr.Choices[0].Message
	out := []any{}
	if msg.Content != "" {
		out = append(out, messageItem(newID("msg"), msg.Content))
	}
	for _, tc := range msg.ToolCalls {
		out = append(out, functionCallItem(newID("fc"), tc))
	}

	usage := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	if cr.Usage != nil {
		usage = map[string]any{
			"input_tokens":  cr.Usage.PromptTokens,
			"output_tokens": cr.Usage.CompletionTokens,
			"total_tokens":  cr.Usage.TotalTokens,
		}
	}
	created := cr.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	respObj := map[string]any{
		"id":                  newID("resp"),
		"object":              "response",
		"created_at":          created,
		"status":              "completed",
		"model":               cr.Model,
		"output":              out,
		"output_text":         msg.Content,
		"usage":               usage,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
	}
	writeJSON(w, http.StatusOK, respObj)
}

// ---------- 流式响应翻译 ----------

func (h *Handler) streamResponses(w http.ResponseWriter, rc io.Reader, model string) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	respID := newID("resp")
	created := time.Now().Unix()
	emit := func(event string, payload any) {
		writeSSEEvent(w, flusher, event, payload)
	}

	// response.created：Codex 靠首个事件建立上下文
	emit("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":                  respID,
			"object":              "response",
			"created_at":          created,
			"status":              "in_progress",
			"model":               model,
			"output":              []any{},
			"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
			"parallel_tool_calls": true,
			"tool_choice":         "auto",
			"tools":               []any{},
		},
	})

	var (
		msgItemID string
		msgOpen   bool
		text      strings.Builder
		outIdx    int
		calls     = map[int]*respCallAcc{}
		callOrder []int
		usage     = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
		gotModel  = model
	)

	openMessage := func() {
		if msgOpen {
			return
		}
		msgItemID = newID("msg")
		item := map[string]any{
			"id":      msgItemID,
			"type":    "message",
			"role":    "assistant",
			"status":  "in_progress",
			"content": []any{},
		}
		emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": outIdx, "item": item,
		})
		emit("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": msgItemID,
			"output_index": outIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
		msgOpen = true
	}
	closeMessage := func() {
		if !msgOpen {
			return
		}
		full := text.String()
		emit("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": msgItemID,
			"output_index": outIdx, "content_index": 0, "text": full,
		})
		emit("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": msgItemID,
			"output_index": outIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
		})
		emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": outIdx,
			"item": messageItem(msgItemID, full),
		})
		outIdx++
		msgOpen = false
	}

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // 跳过 ": heartbeat" 注释与空行
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk chatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Model != "" {
			gotModel = chunk.Model
		}
		if chunk.Usage != nil {
			usage = map[string]any{
				"input_tokens":  chunk.Usage.PromptTokens,
				"output_tokens": chunk.Usage.CompletionTokens,
				"total_tokens":  chunk.Usage.TotalTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.Content != "" {
			openMessage()
			text.WriteString(d.Content)
			emit("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": msgItemID,
				"output_index": outIdx, "content_index": 0, "delta": d.Content,
			})
		}
		for _, tc := range d.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			acc, ok := calls[idx]
			if !ok {
				closeMessage() // 正文与工具调用不能混在同一 output item 里
				acc = &respCallAcc{itemID: newID("fc"), outputIndex: outIdx}
				if tc.ID != "" {
					acc.callID = tc.ID
				} else {
					acc.callID = newID("call")
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				calls[idx] = acc
				callOrder = append(callOrder, idx)
				emit("response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": acc.outputIndex,
					"item": map[string]any{
						"id":        acc.itemID,
						"type":      "function_call",
						"call_id":   acc.callID,
						"name":      acc.name,
						"arguments": "",
						"status":    "in_progress",
					},
				})
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				acc.args.WriteString(tc.Function.Arguments)
				emit("response.function_call_arguments.delta", map[string]any{
					"type": "response.function_call_arguments.delta", "item_id": acc.itemID,
					"output_index": acc.outputIndex, "delta": tc.Function.Arguments,
				})
			}
		}
	}
	closeMessage()
	for _, idx := range callOrder {
		acc := calls[idx]
		emit("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": acc.itemID,
			"output_index": acc.outputIndex, "arguments": acc.args.String(), "name": acc.name,
		})
		emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": acc.outputIndex,
			"item": map[string]any{
				"id":        acc.itemID,
				"type":      "function_call",
				"call_id":   acc.callID,
				"name":      acc.name,
				"arguments": acc.args.String(),
				"status":    "completed",
			},
		})
	}

	// 收尾：Codex 只认 response.completed 才算这一轮结束
	output := []any{}
	if text.Len() > 0 {
		output = append(output, messageItem(msgItemID, text.String()))
	}
	for _, idx := range callOrder {
		acc := calls[idx]
		output = append(output, map[string]any{
			"id":        acc.itemID,
			"type":      "function_call",
			"call_id":   acc.callID,
			"name":      acc.name,
			"arguments": acc.args.String(),
			"status":    "completed",
		})
	}
	emit("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":                  respID,
			"object":              "response",
			"created_at":          created,
			"status":              "completed",
			"model":               gotModel,
			"output":              output,
			"output_text":         text.String(),
			"usage":               usage,
			"parallel_tool_calls": true,
			"tool_choice":         "auto",
			"tools":               []any{},
		},
	})
}

type respCallAcc struct {
	itemID      string
	callID      string
	name        string
	args        strings.Builder
	outputIndex int
}

// ---------- 工具函数 ----------

func messageItem(id, text string) map[string]any {
	return map[string]any{
		"id":     id,
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []any{
			map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		},
	}
}

func functionCallItem(id string, tc chatToolCall) map[string]any {
	callID := tc.ID
	if callID == "" {
		callID = newID("call")
	}
	return map[string]any{
		"id":        id,
		"type":      "function_call",
		"call_id":   callID,
		"name":      tc.Function.Name,
		"arguments": tc.Function.Arguments,
		"status":    "completed",
	}
}

func writeSSEEvent(w http.ResponseWriter, f http.Flusher, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	w.Write([]byte("event: " + event + "\n"))
	w.Write([]byte("data: " + string(b) + "\n\n"))
	if f != nil {
		f.Flush()
	}
}

func writeRespError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": msg,
			"type":    "invalid_request_error",
		},
	})
}

var idSeq atomic.Int64

// newID 生成带前缀的伪随机 id（Responses 协议要求 resp_/msg_/fc_ 形态；
// 只需进程内唯一，无需全局唯一，故用原子自增 + 时间戳拼接）。
func newID(prefix string) string {
	return prefix + "_" + itoa(time.Now().UnixNano()) + itoa(idSeq.Add(1))
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
