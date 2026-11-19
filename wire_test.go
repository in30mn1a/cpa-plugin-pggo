package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func testConfig() pluginConfig {
	c := defaultConfig()
	c.Timezone = "Asia/Shanghai"
	c.MaxOutputTokens = 65536
	return c
}

func TestBuildWireShape(t *testing.T) {
	chat := chatRequest{
		Model: "pg-gemini-3.8-flash",
		Messages: []chatMessage{
			{Role: "system", Content: json.RawMessage(`"你是诊断助手"`)},
			{Role: "user", Content: json.RawMessage(`"第一轮"`)},
			{Role: "assistant", Content: json.RawMessage(`"第一答"`)},
			{Role: "user", Content: json.RawMessage(`"调用 ping 检查服务"`)},
		},
	}
	wire, binding := buildWire("gemini-3.8-flash", chat, testConfig())
	if binding != "调用 ping 检查服务" {
		t.Fatalf("binding 应取最后一条 user 文本，得到 %q", binding)
	}
	if wire[0] != "models/gemini-3.8-flash" {
		t.Fatalf("wire[0] 错误: %v", wire[0])
	}
	if wire[4] != nil {
		t.Fatalf("wire[4] 必须留给脚本注入 proof")
	}
	if wire[10] != 1 {
		t.Fatalf("wire[10] 应为固定 1")
	}
	contents, _ := wire[1].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents 应有 3 轮，得到 %d", len(contents))
	}
	first, _ := contents[0].([]any)
	if first[1] != "user" {
		t.Fatalf("第一轮角色错误: %v", first[1])
	}
	second, _ := contents[1].([]any)
	if second[1] != "model" {
		t.Fatalf("assistant 应编码为 model 轮，得到 %v", second[1])
	}
	system, _ := wire[5].([]any)
	if system[1] != "user" {
		t.Fatalf("system instruction 角色应为 user")
	}
	gen, _ := wire[3].([]any)
	if len(gen) != 14 || gen[13] != 1 {
		t.Fatalf("generationConfig 形状错误: %v", gen)
	}
	if gen[3] != 65536 {
		t.Fatalf("maxOutputTokens 位置错误: %v", gen[3])
	}
	tc, _ := wire[13].([]any)
	inner, _ := tc[0].([]any)
	if inner[2] != "Asia/Shanghai" {
		t.Fatalf("时区应位于 $[13][0][2]，得到 %v", inner[2])
	}
}

// 帧构造器（结构对齐官方协议文档：$[0][frame] 是帧，
// 帧[0] 是 candidates，candidate[0] 是 content=[parts,role]）。
func textFrame(text string) []any {
	content := []any{[]any{[]any{nil, text}}, "model"}
	candidate := []any{content, nil}
	return []any{[]any{candidate}} // frame[0] = candidates
}

func usageFrame(prompt, completion float64) []any {
	candidate := []any{nil, float64(1)} // 完成帧：content 为空 + finish=1
	frame := make([]any, 3)
	frame[0] = []any{candidate}
	frame[2] = []any{prompt, 1.0, completion, nil, nil, nil, nil, 0.0, nil, 0.0}
	return frame
}

func marshalRoot(frames []any) string {
	root := []any{frames}
	raw, _ := json.Marshal(root)
	return ")]}'\n" + string(raw)
}

func TestDecodeUpstreamTextAndFinish(t *testing.T) {
	body := marshalRoot([]any{textFrame("42"), textFrame("43"), usageFrame(27, 28)})
	res, err := decodeUpstream(body)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if res.Text != "4243" {
		t.Fatalf("文本拼接错误: %q", res.Text)
	}
	if res.FinishReason != "stop" {
		t.Fatalf("finish reason 映射错误: %q", res.FinishReason)
	}
	if res.Usage.PromptTokens != 27 || res.Usage.CompletionTokens != 28 || res.Usage.TotalTokens != 55 {
		t.Fatalf("usage 解析错误: %+v", res.Usage)
	}
	if len(res.Deltas) != 2 {
		t.Fatalf("应有两段增量，得到 %d", len(res.Deltas))
	}
}

func TestDecodeUpstreamErrorStatus(t *testing.T) {
	body := `[[], [7, "permission denied", null]]`
	if _, err := decodeUpstream(body); err == nil {
		t.Fatal("流尾 google.rpc 非零状态应当报错")
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("错误信息未透传: %v", err)
	}
}

func TestDecodeUpstreamThoughtPart(t *testing.T) {
	// part[12]=1 → 思考摘要，不进正文
	thoughtPart := make([]any, 13)
	thoughtPart[1] = "想法"
	thoughtPart[12] = float64(1)
	content := []any{[]any{thoughtPart}, "model"}
	frame := []any{[]any{[]any{content, nil}}}
	body := marshalRoot([]any{frame})
	res, err := decodeUpstream(body)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if res.Text != "" || res.Thoughts != "想法" {
		t.Fatalf("thought 帧应进 Thoughts，得到 text=%q thought=%q", res.Text, res.Thoughts)
	}
}

func TestDecodeUpstreamEmpty(t *testing.T) {
	if _, err := decodeUpstream("[]"); err == nil {
		t.Fatal("空响应应当报错，避免下游拿到空结果")
	}
	if _, err := decodeUpstream("   "); err == nil {
		t.Fatal("空 body 应当报错")
	}
}

func TestBuildChatCompletion(t *testing.T) {
	chat := chatRequest{Model: "pg-gemini-3.8-flash"}
	res := &upstreamResult{Text: "你好", FinishReason: "stop", Usage: usageInfo{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}}
	raw := buildChatCompletion(chat, res, "gemini-3.8-flash")
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("输出不是 JSON: %v", err)
	}
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Fatalf("content 错误: %v", msg["content"])
	}
}

func TestBuildStreamChunksEndsWithDone(t *testing.T) {
	res := &upstreamResult{
		Deltas:       []delta{{Text: "a"}, {Text: "思考", Thought: true}, {Text: "b"}},
		FinishReason: "stop",
	}
	chunks := buildStreamChunks(chatRequest{}, res, "m")
	if len(chunks) != 4 { // a、b、收尾帧、[DONE]
		t.Fatalf("增量应跳过 thought 段，得到 %d 块", len(chunks))
	}
	if !strings.HasPrefix(string(chunks[0]), "data: ") {
		t.Fatalf("块必须是 SSE 帧: %q", chunks[0])
	}
	if string(chunks[len(chunks)-1]) != "data: [DONE]\n\n" {
		t.Fatalf("最后一帧必须是 [DONE]: %q", chunks[len(chunks)-1])
	}
}

func TestYAMLHelpers(t *testing.T) {
	yaml := `
plugins:
  enabled: true
  dir: plugins
  configs:
    pggo:
      enabled: true
      config_file: /opt/cliproxyapi/pggo.json
api-keys:
  - "abc-123"
  - def-456
`
	if got := yamlScalar(yaml, "config_file"); got != "/opt/cliproxyapi/pggo.json" {
		t.Fatalf("config_file 解析错误: %q", got)
	}
	keys := yamlListBlock(yaml, "api-keys")
	if len(keys) != 2 || keys[0] != "abc-123" || keys[1] != "def-456" {
		t.Fatalf("api-keys 解析错误: %v", keys)
	}
	inline := "api-keys: [\"k1\", \"k2\"]\n"
	if keys := yamlListBlock(inline, "api-keys"); len(keys) != 2 {
		t.Fatalf("行内 api-keys 解析错误: %v", keys)
	}
}
