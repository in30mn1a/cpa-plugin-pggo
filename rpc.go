package main

import (
	"bytes"
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
	Listen              string            `json:"listen"`
	ScriptPath          string            `json:"script_path"`
	DataDir             string            `json:"data_dir"`
	APIKeys             []string          `json:"api_keys"`
	ModelPrefix         string            `json:"model_prefix"`
	HeadlessModelPrefix string            `json:"headless_model_prefix"`
	Models              []string          `json:"models"`
	ModelMap            map[string]string `json:"model_map"`
	Timezone            string            `json:"timezone"`
	MaxOutputTokens     int               `json:"max_output_tokens"`
	RequestTimeoutSec   int               `json:"request_timeout_sec"`
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		Listen:              "127.0.0.1:8788",
		DataDir:             "pggo-data",
		ModelPrefix:         "pg-",
		HeadlessModelPrefix: "pggo-",
		Models:              []string{"gemini-3.8-flash", "gemini-3.6-flash", "gemini-2.5-pro", "gemini-2.5-flash"},
		Timezone:            "Asia/Shanghai",
		MaxOutputTokens:     65536,
		RequestTimeoutSec:   600,
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
			"listen":                &next.Listen,
			"script_path":           &next.ScriptPath,
			"data_dir":              &next.DataDir,
			"model_prefix":          &next.ModelPrefix,
			"headless_model_prefix": &next.HeadlessModelPrefix,
		} {
			if v := yamlScalar(yamlText, key); v != "" {
				*dst = v
			}
		}
		// 前缀写成显式空串（`model_prefix: ""`）= 停用该通道；不写或留空则保持默认
		for key, dst := range map[string]*string{
			"model_prefix":          &next.ModelPrefix,
			"headless_model_prefix": &next.HeadlessModelPrefix,
		} {
			if yamlExplicitEmpty(yamlText, key) {
				*dst = ""
			}
		}
		if models := yamlListBlock(yamlText, "models"); len(models) > 0 {
			next.Models = models
		}
	}
	switch {
	case next.ModelPrefix == "" && next.HeadlessModelPrefix == "":
		logf("model_prefix 与 headless_model_prefix 均为空：两个通道都已停用，不注册任何模型")
	case next.ModelPrefix == "":
		logf("model_prefix 为空：page 通道已停用")
	case next.HeadlessModelPrefix == "":
		logf("headless_model_prefix 为空：headless 通道已停用")
	case next.HeadlessModelPrefix == next.ModelPrefix:
		logf("headless_model_prefix 与 model_prefix 相同（%q）：无头通道模型不注册，请改成不同前缀", next.ModelPrefix)
	}
	if len(next.APIKeys) == 0 {
		logf("未配置任何执行器 Key：页面 register 将全部被 4008 拒绝（请在插件配置的 api-keys 中填写）")
	}
	cfgMu.Lock()
	cfg = next
	cfgMu.Unlock()
	activateHub(currentConfig())
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

// yamlExplicitEmpty 判断某键是否被显式写成空串（双引号或单引号的空值）。
func yamlExplicitEmpty(yamlText, key string) bool {
	re := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*:[ \t]*(""|'')[ \t]*(#.*)?\r?$`)
	return re.MatchString(yamlText)
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

const pluginVersion = "1.1.2"

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

// capabilities 对应 CPA pluginhost.rpcCapabilities 的 json tag。
//
// 为什么要 model_router：CPA 的常规路径（模型 → provider → 认证调度器）要求 provider
// 下至少有一个 Auth 记录；本插件没有凭据（执行器连的是浏览器），executor 的静态模型
// 只会进 /v1/models 列表，请求会在选认证时直接 auth_not_found。CPA 为「无需认证的
// 插件执行器」提供的入口是 ModelRouter：model.route 返回 Handled + TargetKind=self 后，
// 宿主跳过 provider/认证选择，直接调用本插件的 executor.execute(_stream)
// （CPA sdk/api/handlers/handlers_execution.go executeWithAuthManagerFormats →
// executeWithPluginExecutor；internal/pluginhost/model_router.go RouteModelExcept）。
type capabilities struct {
	ModelRegistrar        bool     `json:"model_registrar"`
	ModelRouter           bool     `json:"model_router"`
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
				{Name: "models", Type: "array", Description: "上游模型 ID 列表，每个 ID 注册两次：<model_prefix><ID>（页面通道）与 <headless_model_prefix><ID>（无头通道）；不填使用内置默认列表"},
				{Name: "model_prefix", Type: "string", Default: "pg-", Description: "页面通道（真实浏览器）模型前缀，如 pg- 或 pg/"},
				{Name: "headless_model_prefix", Type: "string", Default: "pggo-", Description: "无头通道（云端 headless worker）模型前缀，如 pggo- 或 pggo/；须与 model_prefix 不同"},
				{Name: "config_file", Type: "string", Description: "可选：JSON 配置文件路径（高级项 model_map/timezone 等）；面板字段非空时优先"},
			},
		},
		// executor_model_scope=static：执行器只服务无认证的静态模型。并非必需——未声明 auth provider 时
		// both 同样允许静态模型（宿主 executorScopeAllowsStaticModels），static 只是更明确、无副作用
		Capabilities: capabilities{
			ModelRegistrar:        true,
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    "static",
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

// modelRegistrationPayload 把 models 注册两遍：页面通道前缀一遍、无头通道前缀一遍。
// 两个前缀相同时只注册页面通道（避免同名模型歧义）。
func modelRegistrationPayload() modelRegistration {
	c := currentConfig()
	out := modelRegistration{Provider: "pggo"}
	// 前缀为空 = 该通道停用，不注册（否则会把裸模型名注册进 CPA，与其它 provider 撞名）
	var prefixes []string
	if c.ModelPrefix != "" {
		prefixes = append(prefixes, c.ModelPrefix)
	}
	if c.HeadlessModelPrefix != "" && c.HeadlessModelPrefix != c.ModelPrefix {
		prefixes = append(prefixes, c.HeadlessModelPrefix)
	}
	for _, prefix := range prefixes {
		for _, m := range c.Models {
			out.Models = append(out.Models, modelInfo{
				ID:                         prefix + m,
				Object:                     "model",
				OwnedBy:                    "pggo",
				DisplayName:                prefix + m,
				SupportedGenerationMethods: []string{"chat"},
				MaxCompletionTokens:        c.MaxOutputTokens,
				UserDefined:                true,
			})
		}
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

// model.route 的请求/响应。CPA 侧是无 json tag 的 pluginapi.ModelRouteRequest/
// ModelRouteResponse（内嵌进 rpcModelRouteRequest），因此字段名为大驼峰。
// 只声明需要的字段：请求里还带着整份 base64 请求体（Body），encoding/json 对未声明的
// 字段只做扫描、不解码不分配。
type modelRouteRequest struct {
	RequestedModel string `json:"RequestedModel"`
}

type modelRouteResponse struct {
	Handled    bool   `json:"Handled"`
	TargetKind string `json:"TargetKind,omitempty"`
	Reason     string `json:"Reason,omitempty"`
}

// ownsModel 判断请求的模型是否属于本插件：命中任一已启用通道的前缀（前缀后须有模型名）。
// 与 routeModel 不同，这里不做「无前缀回落到 page 通道」——那会把别的 provider 的模型抢走。
func ownsModel(c pluginConfig, model string) bool {
	return ownsModelPrefixes(c.ModelPrefix, c.HeadlessModelPrefix, model)
}

func ownsModelPrefixes(pagePrefix, headlessPrefix, model string) bool {
	model = strings.TrimSpace(model)
	for _, prefix := range [2]string{pagePrefix, headlessPrefix} {
		if prefix != "" && len(model) > len(prefix) && strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// 路由结果固定，预先编码；调用方（C ABI 出口会拷贝）不得修改。
var (
	routeNotHandled = okEnvelope(modelRouteResponse{Handled: false})
	routeSelf       = okEnvelope(modelRouteResponse{Handled: true, TargetKind: "self", Reason: "pggo_model_prefix"})
)

// routeModelRPC 对本插件的模型声明「由自己的执行器处理」，其余一律不处理（交还宿主）。
// 宿主对所有 provider 的每个请求都会调用路由器：只解码模型名、只读两个前缀，不记日志。
func routeModelRPC(request []byte) []byte {
	model, ok := fastRequestedModel(request)
	if !ok {
		var req modelRouteRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return routeNotHandled
		}
		model = req.RequestedModel
	}
	cfgMu.RLock()
	owned := ownsModelPrefixes(cfg.ModelPrefix, cfg.HeadlessModelPrefix, model)
	cfgMu.RUnlock()
	if !owned {
		return routeNotHandled
	}
	return routeSelf
}

var requestedModelKey = []byte(`"RequestedModel":`)

// fastRequestedModel 直接定位 "RequestedModel" 的值，避免为取一个字段而扫描整份
// （含 base64 请求体的）JSON。能这样做是因为：该键名带紧随的引号与冒号，不可能出现在
// JSON 字符串值内部（字符串里的引号必须转义）；Body 是 base64 不含引号；请求里其它对象的
// 键（插件元数据字段名、snake_case 的 Metadata 键）不会叫这个名字。任何意外都返回 ok=false，
// 由调用方回落到完整的 json.Unmarshal。
func fastRequestedModel(request []byte) (string, bool) {
	i := bytes.Index(request, requestedModelKey)
	if i < 0 {
		return "", false
	}
	rest := bytes.TrimLeft(request[i+len(requestedModelKey):], " \t\r\n")
	if len(rest) == 0 || rest[0] != '"' {
		return "", false
	}
	end := -1
	for j := 1; j < len(rest); j++ {
		if rest[j] == '\\' {
			j++
			continue
		}
		if rest[j] == '"' {
			end = j
			break
		}
	}
	if end < 0 {
		return "", false
	}
	token := rest[:end+1]
	if bytes.IndexByte(token, '\\') < 0 {
		return string(token[1:end]), true
	}
	var model string
	if err := json.Unmarshal(token, &model); err != nil {
		return "", false
	}
	return model, true
}

// safeHandleRPC 是 C ABI 入口用的 handleRPC：本库跑在 CPA 进程内，任何 panic 都会带走
// 宿主（model.route 对所有请求都会调用），这里兜底成 500 错误信封并记录。
func safeHandleRPC(method string, request []byte) (out []byte, rc int) {
	return recoverRPC(method, func() ([]byte, int) { return handleRPC(method, request) })
}

func recoverRPC(method string, fn func() ([]byte, int)) (out []byte, rc int) {
	defer func() {
		if p := recover(); p != nil {
			logf("rpc %s panic（已拦截）: %v", method, p)
			out, rc = errorEnvelope("plugin_panic", fmt.Sprintf("pggo 插件内部错误: %v", p), http.StatusInternalServerError), 1
		}
	}()
	return fn()
}

func handleRPC(method string, request []byte) (out []byte, rc int) {
	if method == "model.route" {
		reviveIfShutdown() // 停用后若宿主不再注册，这是唯一的信号（每个 CPA 请求都会路由）
		return routeModelRPC(request), 0
	}
	logf("rpc %s (%d bytes)", method, len(request))
	if !strings.HasPrefix(method, "plugin.") {
		reviveIfShutdown()
	}
	switch method {
	case "plugin.register", "plugin.reconfigure":
		applyConfig(request)
		return okEnvelope(registrationPayload()), 0
	case "plugin.quiesce":
		// CPA 热更新时先对旧实例调用（internal/pluginhost/host.go callQuiesceWithOutcome），
		// 成功后才加载新实例并对其 plugin.register。c-shared 库不会被卸载，旧实例必须在此
		// 同步交出端口，否则新实例绑定失败。若新实例加载失败，CPA 回滚时会对旧实例发
		// plugin.reconfigure（callRegister 对已注册实例用 reconfigure），届时 ensureHub 重新拉起中枢。
		quiesceHub()
		logf("已静默：中枢端口已释放，WS 已以 1001 断开")
		return okEnvelope(struct{}{}), 0
	case "plugin.shutdown":
		pluginShutdown()
		return okEnvelope(struct{}{}), 0
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
	channel, upstreamModel := routeModel(c, req.Model)
	wire, binding := buildWire(upstreamModel, chat, c)
	h := activeHub()
	if h == nil {
		return nil, nil, "", rpcErrf(http.StatusServiceUnavailable, "hub_offline", "执行器中枢未启动")
	}
	raw, err := h.dispatch(channel, upstreamModel, wire, binding)
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
