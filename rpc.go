package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// ---------------------------------------------------------------- 信封 ----

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func okEnvelope(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return errorEnvelope("marshal_failed", err.Error(), http.StatusInternalServerError)
	}
	out, _ := json.Marshal(envelope{OK: true, Result: raw})
	return out
}

func errorEnvelope(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: httpStatus}})
	return raw
}

type rpcError struct {
	code   string
	msg    string
	status int
}

func (e *rpcError) Error() string { return e.msg }

func rpcErrf(status int, code, format string, a ...any) *rpcError {
	return &rpcError{code: code, msg: fmt.Sprintf(format, a...), status: status}
}

// ---------------------------------------------------------------- 配置 ----

type pluginConfig struct {
	Listen            string            `json:"listen"`
	ScriptPath        string            `json:"script_path"`
	DataDir           string            `json:"data_dir"`
	APIKeys           []string          `json:"api_keys"`
	ModelPrefix      string            `json:"model_prefix"`
	Models            []string          `json:"models"`
	ModelMap          map[string]string `json:"model_map"`
	Timezone          string            `json:"timezone"`
	MaxOutputTokens   int               `json:"max_output_tokens"`
	RequestTimeoutSec int               `json:"request_timeout_sec"`
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		Listen:            "127.0.0.1:8788",
		DataDir:           "pggo-data",
		ModelPrefix:       "pg-",
		Models:            []string{"gemini-3.8-flash", "gemini-3.6-flash", "gemini-2.5-pro", "gemini-2.5-flash"},
		Timezone:          "Asia/Shanghai",
		MaxOutputTokens:   65536,
		RequestTimeoutSec: 600,
	}
}

var (
	cfgMu sync.RWMutex
	cfg   = defaultConfig()
)

func currentConfig() pluginConfig {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

// applyConfig 解析宿主下发的 config_yaml。
// 注意：宿主只下发 plugins.configs.pggo 这一节（见 CPA pluginhost runtimeConfigYAML），
// 不含顶层 api-keys，因此执行器 Key 必须写在本节的 api-keys 或 JSON 的 api_keys 里。
func applyConfig(request []byte) {
	next := defaultConfig()
	var lifecycle struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(request) > 0 && json.Unmarshal(request, &lifecycle) == nil && len(lifecycle.ConfigYAML) > 0 {
		yamlText := string(lifecycle.ConfigYAML)
		if path := yamlScalar(yamlText, "config_file"); path != "" {
			if loaded, err := loadConfigFile(path); err == nil {
				next = loaded
			} else {
				logf("读取配置文件 %s 失败: %v（使用默认配置）", path, err)
			}
		}
		if keys := yamlListBlock(yamlText, "api-keys"); len(keys) > 0 {
			next.APIKeys = append(append([]string{}, next.APIKeys...), keys...)
		}
		// 面板字段（plugins.configs.pggo.*）非空时覆盖 JSON 配置
		for key, dst := range map[string]*string{
			"listen":      &next.Listen,
			"script_path": &next.ScriptPath,
			"data_dir":    &next.DataDir,
		} {
			if v := yamlScalar(yamlText, key); v != "" {
				*dst = v
			}
		}
		if models := yamlListBlock(yamlText, "models"); len(models) > 0 {
			next.Models = models
		}
	}
	if len(next.APIKeys) == 0 {
		logf("未配置任何执行器 Key：页面 register 将全部被 4008 拒绝（请在插件配置的 api-keys 中填写）")
	}
	cfgMu.Lock()
	cfg = next
	cfgMu.Unlock()
	ensureHub(currentConfig())
}

func loadConfigFile(path string) (pluginConfig, error) {
	raw, err := readFile(path)
	if err != nil {
		return defaultConfig(), err
	}
	out := defaultConfig()
	if err := json.Unmarshal(raw, &out); err != nil {
		return defaultConfig(), err
	}
	// JSON 里未出现的字段回落到默认值（Unmarshal 到预填结构天然如此）
	return out, nil
}

// yamlScalar 从整份 YAML 文本里捞一个标量键（够用即止，不引入 YAML 依赖）。
func yamlScalar(yamlText, key string) string {
	// 只在同一行内匹配，避免空值键（`listen:`）吞掉下一行的内容
	re := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*:[ \t]*("([^"]*)"|'([^']*)'|([^\s#]+))`)
	m := re.FindStringSubmatch(yamlText)
	if m == nil {
		return ""
	}
	switch {
	case m[2] != "":
		return m[2]
	case m[3] != "":
		return m[3]
	default:
		return m[4]
	}
}

// yamlListBlock 解析 `api-keys:` 下的块列表或行内列表。
func yamlListBlock(yamlText, key string) []string {
	lines := strings.Split(yamlText, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, key+":") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, key+":"))
		if rest != "" {
			rest = strings.Trim(rest, "[]")
			var out []string
			for _, part := range strings.Split(rest, ",") {
				part = strings.Trim(strings.TrimSpace(part), `"'`)
				if part != "" {
					out = append(out, part)
				}
			}
			return out
		}
		indent := countIndent(line)
		var out []string
		for _, sub := range lines[i+1:] {
			if strings.TrimSpace(sub) == "" {
				continue
			}
			item := strings.TrimSpace(sub)
			// 兼容紧凑写法：列表项可与父键同缩进（`key:\n- a`）
			if countIndent(sub) < indent || !strings.HasPrefix(item, "-") {
				break
			}
			item = strings.Trim(strings.TrimSpace(strings.TrimPrefix(item, "-")), `"'`)
			if item != "" {
				out = append(out, item)
			}
		}
		return out
	}
	return nil
}

func countIndent(s string) int {
	n := 0
	for _, r := range s {
		if r == ' ' {
			n++
			continue
		}
		if r == '\t' {
			n += 4
			continue
		}
		break
	}
	return n
}

// ------------------------------------------------------------ 注册元数据 ----

const pluginVersion = "1.0.1"

type configField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	Default     any      `json:"Default,omitempty"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

type metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type capabilities struct {
	ModelRegistrar        bool     `json:"model_registrar"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

func registrationPayload() registration {
	return registration{
		SchemaVersion: 1,
		Metadata: metadata{
			Name:             "pggo",
			Version:          pluginVersion,
			Author:           "in30mn1a",
			GitHubRepository: "https://github.com/in30mn1a/cpa-plugin-pggo",
			ConfigFields: []configField{
				{Name: "api-keys", Type: "array", Description: "页面执行器 register 用的 Key（脚本面板里填的那个）；建议与 CPA 顶层 api-keys 相同。必填"},
				{Name: "listen", Type: "string", Default: "127.0.0.1:8788", Description: "内嵌中枢监听地址；CPA 跑在 Docker 里需改为 0.0.0.0:8788 并映射端口。修改后自动重绑"},
				{Name: "script_path", Type: "string", Description: "用户脚本文件路径（供 /api/pg/script.user.js 分发）；不填则不提供脚本分发"},
				{Name: "data_dir", Type: "string", Default: "pggo-data", Description: "Cookie 推送 / Robin 绑定的落盘目录"},
				{Name: "models", Type: "array", Description: "上游模型 ID 列表，注册为 pg-<ID>；不填使用内置默认列表"},
				{Name: "config_file", Type: "string", Description: "可选：JSON 配置文件路径（高级项 model_map/timezone 等）；面板字段非空时优先"},
			},
		},
		Capabilities: capabilities{
			ModelRegistrar:        true,
			Executor:              true,
			ExecutorModelScope:    "both",
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
}

type modelInfo struct {
	ID                         string   `json:"ID"`
	Object                     string   `json:"Object"`
	OwnedBy                    string   `json:"OwnedBy"`
	DisplayName                string   `json:"DisplayName"`
	SupportedGenerationMethods []string `json:"SupportedGenerationMethods"`
	ContextLength              int      `json:"ContextLength"`
	MaxCompletionTokens        int      `json:"MaxCompletionTokens"`
	UserDefined                bool     `json:"UserDefined"`
}

type modelRegistration struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

func modelRegistrationPayload() modelRegistration {
	c := currentConfig()
	out := modelRegistration{Provider: "pggo"}
	for _, m := range c.Models {
		out.Models = append(out.Models, modelInfo{
			ID:                         c.ModelPrefix + m,
			Object:                     "model",
			OwnedBy:                    "pggo",
			DisplayName:                c.ModelPrefix + m,
			SupportedGenerationMethods: []string{"chat"},
			MaxCompletionTokens:        c.MaxOutputTokens,
			UserDefined:                true,
		})
	}
	return out
}

// ---------------------------------------------------------------- 执行器 ----

type executorRequest struct {
	AuthID          string `json:"AuthID"`
	AuthProvider    string `json:"AuthProvider"`
	Model           string `json:"Model"`
	Format          string `json:"Format"`
	Stream          bool   `json:"Stream"`
	Alt             string `json:"Alt"`
	OriginalRequest []byte `json:"OriginalRequest"`
	SourceFormat    string `json:"SourceFormat"`
	Payload         []byte `json:"Payload"`
}

type executorResponse struct {
	Payload []byte      `json:"Payload"`
	Headers http.Header `json:"Headers,omitempty"`
}

type streamChunk struct {
	Payload []byte `json:"Payload"`
}

// 字段名与 CPA pluginhost.rpcExecutorStreamResponse 的 json tag 保持一致（小写）。
type executorStreamResponse struct {
	Headers http.Header   `json:"headers,omitempty"`
	Chunks  []streamChunk `json:"chunks,omitempty"`
}

func handleRPC(method string, request []byte) (out []byte, rc int) {
	logf("rpc %s (%d bytes)", method, len(request))
	switch method {
	case "plugin.register", "plugin.reconfigure":
		applyConfig(request)
		return okEnvelope(registrationPayload()), 0
	case "executor.identifier":
		return okEnvelope(map[string]string{"identifier": "pggo"}), 0
	case "model.register":
		return okEnvelope(modelRegistrationPayload()), 0
	case "executor.execute":
		resp, err := execute(request)
		if err != nil {
			return errorEnvelopeOf(err), 1
		}
		return okEnvelope(resp), 0
	case "executor.execute_stream":
		resp, err := executeStreamRPC(request)
		if err != nil {
			return errorEnvelopeOf(err), 1
		}
		return okEnvelope(resp), 0
	case "executor.count_tokens":
		resp, err := countTokens(request)
		if err != nil {
			return errorEnvelopeOf(err), 1
		}
		return okEnvelope(resp), 0
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0), 1
	}
}

func errorEnvelopeOf(err error) []byte {
	if re, ok := err.(*rpcError); ok {
		return errorEnvelope(re.code, re.msg, re.status)
	}
	return errorEnvelope("plugin_error", err.Error(), http.StatusBadGateway)
}

// run 把一次执行器调用翻译成「上游 wire → 浏览器执行 → 上游原文 → 文本」。
func run(request []byte) (*chatRequest, *upstreamResult, string, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, nil, "", rpcErrf(http.StatusBadRequest, "invalid_request", "解析执行器请求失败: %v", err)
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	var chat chatRequest
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, nil, "", rpcErrf(http.StatusBadRequest, "invalid_request", "请求体不是 OpenAI chat-completions JSON: %v", err)
	}
	c := currentConfig()
	upstreamModel := resolveUpstreamModel(c, req.Model)
	wire, binding := buildWire(upstreamModel, chat, c)
	h := activeHub()
	if h == nil {
		return nil, nil, "", rpcErrf(http.StatusServiceUnavailable, "hub_offline", "执行器中枢未启动")
	}
	raw, err := h.dispatch(upstreamModel, wire, binding)
	if err != nil {
		return nil, nil, "", err
	}
	result, err := decodeUpstream(raw)
	if err != nil {
		return nil, nil, "", err
	}
	return &chat, result, upstreamModel, nil
}

func execute(request []byte) (executorResponse, error) {
	chat, result, model, err := run(request)
	if err != nil {
		return executorResponse{}, err
	}
	body := buildChatCompletion(*chat, result, model)
	return executorResponse{Payload: body, Headers: http.Header{"Content-Type": []string{"application/json"}}}, nil
}

func executeStreamRPC(request []byte) (executorStreamResponse, error) {
	chat, result, model, err := run(request)
	if err != nil {
		return executorStreamResponse{}, err
	}
	chunks := buildStreamChunks(*chat, result, model)
	out := executorStreamResponse{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  make([]streamChunk, 0, len(chunks)),
	}
	for _, ch := range chunks {
		out.Chunks = append(out.Chunks, streamChunk{Payload: ch})
	}
	return out, nil
}

func countTokens(request []byte) (executorResponse, error) {
	var req executorRequest
	_ = json.Unmarshal(request, &req)
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	var chat chatRequest
	_ = json.Unmarshal(body, &chat)
	n := 0
	for _, m := range chat.Messages {
		n += len([]rune(textOf(m.Content)))
	}
	payload, _ := json.Marshal(map[string]int{"total_tokens": n/4 + 1})
	return executorResponse{Payload: payload}, nil
}
