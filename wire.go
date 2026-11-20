package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ------------------------------------------------------------ OpenAI 请求 ----

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Stream              bool          `json:"stream"`
	MaxTokens           *int          `json:"max_tokens"`
	MaxCompletionTokens *int          `json:"max_completion_tokens"`
	Temperature         *float64      `json:"temperature"`
	TopP                *float64      `json:"top_p"`
	TopK                *int          `json:"top_k"`
	Stop                []string      `json:"stop"`
}

// textOf 取 content 的纯文本（字符串或 [{type:"text",text:"..."}] 数组）。
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// routeModel 按最长匹配前缀决定执行通道并剥掉该前缀：
// <model_prefix> → page，<headless_model_prefix> → headless；
// 都不匹配时走 page（兼容 model_map 里的裸名）。model_map 按完整请求名查。
// 前缀为空表示该通道停用：不参与匹配；page 停用时不匹配的请求返回空通道（不可路由）。
func routeModel(c pluginConfig, requested string) (channel, upstream string) {
	if c.ModelPrefix != "" {
		channel = channelPage
	}
	name := requested
	matched := -1
	for _, cand := range []struct{ prefix, channel string }{
		{c.ModelPrefix, channelPage},
		{c.HeadlessModelPrefix, channelHeadless},
	} {
		if cand.prefix == "" || !strings.HasPrefix(requested, cand.prefix) || len(cand.prefix) <= matched {
			continue
		}
		matched = len(cand.prefix)
		channel = cand.channel
		name = strings.TrimPrefix(requested, cand.prefix)
	}
	if mapped, ok := c.ModelMap[requested]; ok && mapped != "" {
		return channel, mapped
	}
	return channel, strings.TrimPrefix(name, "models/")
}

// ------------------------------------------------------- 上游 wire 编码 ----
//
// GenerateContent 是 JSON+protobuf：数组下标 = protobuf 字段号 - 1。
// 字段表见 AIStudio 私有协议：
//   [0]=models/<id> [1]=contents [2]=safety [3]=generationConfig
//   [4]=WAA proof（由浏览器脚本注入，这里留 null） [5]=systemInstruction
//   [10]=固定 1 [13]=toolConfig（时区）

func protoText(text string) []any { return []any{nil, text} }

func buildWire(modelID string, chat chatRequest, c pluginConfig) ([]any, string) {
	var contents []any
	var systemParts []any
	binding := ""
	for _, m := range chat.Messages {
		text := textOf(m.Content)
		if text == "" {
			continue
		}
		switch strings.ToLower(m.Role) {
		case "system", "developer":
			systemParts = append(systemParts, protoText(text))
		case "assistant":
			contents = append(contents, []any{[]any{protoText(text)}, "model"})
		default: // user / tool → 一律按 user 轮发送
			contents = append(contents, []any{[]any{protoText(text)}, "user"})
			binding = text
		}
	}
	if binding == "" && len(chat.Messages) > 0 {
		binding = textOf(chat.Messages[len(chat.Messages)-1].Content)
	}

	safety := []any{
		[]any{nil, nil, 7, 5},
		[]any{nil, nil, 8, 5},
		[]any{nil, nil, 9, 5},
		[]any{nil, nil, 10, 5},
	}

	gen := make([]any, 14)
	maxOut := c.MaxOutputTokens
	if chat.MaxCompletionTokens != nil && *chat.MaxCompletionTokens > 0 {
		maxOut = *chat.MaxCompletionTokens
	}
	if chat.MaxTokens != nil && *chat.MaxTokens > 0 {
		maxOut = *chat.MaxTokens
	}
	if len(chat.Stop) > 0 {
		stops := make([]any, 0, len(chat.Stop))
		for _, s := range chat.Stop {
			stops = append(stops, s)
		}
		gen[1] = stops
	}
	gen[3] = maxOut
	if chat.Temperature != nil {
		gen[4] = *chat.Temperature
	}
	if chat.TopP != nil {
		gen[5] = *chat.TopP
	}
	if chat.TopK != nil {
		gen[6] = *chat.TopK
	}
	gen[13] = 1

	tz := c.Timezone
	if tz == "" {
		tz = "UTC"
	}

	root := make([]any, 14)
	root[0] = "models/" + modelID
	root[1] = contents
	root[2] = safety
	root[3] = gen
	root[4] = nil
	if len(systemParts) > 0 {
		root[5] = []any{systemParts, "user"}
	}
	root[10] = 1
	root[13] = []any{[]any{nil, nil, tz}}
	return root, binding
}

// ------------------------------------------------------- 上游响应解码 ----

type delta struct {
	Text    string
	Thought bool
}

type usageInfo struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

type upstreamResult struct {
	Text         string
	Thoughts     string
	FinishReason string
	Usage        usageInfo
	Deltas       []delta
}

var finishReasons = map[int]string{
	0:  "stop",
	1:  "stop",
	2:  "length",
	3:  "content_filter",
	4:  "content_filter",
	5:  "stop",
	6:  "content_filter",
	7:  "content_filter",
	8:  "content_filter",
	9:  "stop",
	10: "content_filter",
	11: "stop",
	12: "stop",
}

// decodeUpstream 把 MakerSuite 的 JSON+protobuf 响应体还原成文本帧。
// 根数组 [0] 是 repeated frames，[1] 非空时是流尾 google.rpc 状态。
func decodeUpstream(raw string) (*upstreamResult, error) {
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), ")]}'"))
	if body == "" {
		return nil, rpcErrf(http.StatusBadGateway, "empty_upstream", "上游返回空内容")
	}
	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		snippet := body
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, rpcErrf(http.StatusBadGateway, "upstream_not_json", "上游响应不是合法 JSON（页面可能没拿到 200）: %v | %s", err, snippet)
	}
	arr, ok := root.([]any)
	if !ok {
		return nil, rpcErrf(http.StatusBadGateway, "upstream_shape", "上游响应根节点不是数组")
	}
	if len(arr) > 1 && arr[1] != nil {
		if st, ok := arr[1].([]any); ok {
			code, msg := 0, ""
			if len(st) > 0 {
				if f, ok := st[0].(float64); ok {
					code = int(f)
				}
			}
			if len(st) > 1 {
				if s, ok := st[1].(string); ok {
					msg = s
				}
			}
			if code != 0 {
				return nil, rpcErrf(http.StatusBadGateway, "upstream_status", "上游错误 code=%d: %s", code, msg)
			}
		}
	}
	if len(arr) == 0 || arr[0] == nil {
		return nil, rpcErrf(http.StatusBadGateway, "upstream_empty", "上游响应没有任何帧")
	}
	frames, ok := arr[0].([]any)
	if !ok {
		return nil, rpcErrf(http.StatusBadGateway, "upstream_shape", "上游帧容器不是数组")
	}

	out := &upstreamResult{FinishReason: "stop"}
	for _, fr := range frames {
		frame, ok := fr.([]any)
		if !ok || len(frame) == 0 {
			continue
		}
		if cands, ok := frame[0].([]any); ok {
			for _, cd := range cands {
				cand, ok := cd.([]any)
				if !ok {
					continue
				}
				if len(cand) > 0 && cand[0] != nil {
					if content, ok := cand[0].([]any); ok && len(content) > 0 {
						if parts, ok := content[0].([]any); ok {
							for _, p := range parts {
								part, ok := p.([]any)
								if !ok || len(part) < 2 {
									continue
								}
								text, ok := part[1].(string)
								if !ok || text == "" {
									continue
								}
								thought := len(part) > 12 && truthy(part[12])
								if thought {
									out.Thoughts += text
								} else {
									out.Text += text
								}
								out.Deltas = append(out.Deltas, delta{Text: text, Thought: thought})
							}
						}
					}
				}
				if len(cand) > 1 {
					if f, ok := cand[1].(float64); ok {
						if mapped, ok := finishReasons[int(f)]; ok {
							out.FinishReason = mapped
						}
					}
				}
			}
		}
		if len(frame) > 2 && frame[2] != nil {
			if u, ok := frame[2].([]any); ok {
				out.Usage.PromptTokens = intAt(u, 0)
				out.Usage.CompletionTokens = intAt(u, 2)
				out.Usage.TotalTokens = out.Usage.PromptTokens + out.Usage.CompletionTokens
			}
		}
	}
	if out.Text == "" && out.Thoughts == "" {
		return nil, rpcErrf(http.StatusBadGateway, "upstream_no_text", "上游 200 但没有文本帧")
	}
	return out, nil
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != "" && t != "0"
	}
	return false
}

func intAt(arr []any, i int) int {
	if i < len(arr) {
		if f, ok := arr[i].(float64); ok {
			return int(f)
		}
	}
	return 0
}

// ------------------------------------------------------- 下游响应编码 ----

func completionID() string {
	return fmt.Sprintf("chatcmpl-pggo-%d", time.Now().UnixNano())
}

func buildChatCompletion(chat chatRequest, r *upstreamResult, model string) []byte {
	body := map[string]any{
		"id":      completionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": r.Text},
			"finish_reason": r.FinishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     r.Usage.PromptTokens,
			"completion_tokens": r.Usage.CompletionTokens,
			"total_tokens":      r.Usage.TotalTokens,
		},
	}
	raw, _ := json.Marshal(body)
	return raw
}

func buildStreamChunks(chat chatRequest, r *upstreamResult, model string) [][]byte {
	id := completionID()
	created := time.Now().Unix()
	out := make([][]byte, 0, len(r.Deltas)+2)
	for _, d := range r.Deltas {
		if d.Thought {
			continue // 思考摘要不进 SSE 正文，避免污染下游拼接
		}
		frame := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"content": d.Text},
				"finish_reason": nil,
			}},
		}
		raw, _ := json.Marshal(frame)
		out = append(out, []byte("data: "+string(raw)+"\n\n"))
	}
	last := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": r.FinishReason,
		}},
	}
	raw, _ := json.Marshal(last)
	out = append(out, []byte("data: "+string(raw)+"\n\n"), []byte("data: [DONE]\n\n"))
	return out
}

// ------------------------------------------------------------- 小工具 ----

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func logf(format string, a ...any) {
	msg := fmt.Sprintf("[pggo] "+format+"\n", a...)
	_, _ = os.Stderr.WriteString(msg)
}
