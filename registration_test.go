package main

import (
	"encoding/json"
	"testing"
)

// 注册载荷的形态决定 CPA 能否把请求路由到本插件：必须声明 model_router（无认证执行器的
// 唯一入口），执行器作用域为 static，且不能声明 auth_provider（否则宿主改走 OAuth 认证路径）。
func TestRegistrationPayloadShape(t *testing.T) {
	raw := okEnvelope(registrationPayload())
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			SchemaVersion int            `json:"schema_version"`
			Metadata      map[string]any `json:"metadata"`
			Capabilities  map[string]any `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("信封解析失败: %v %s", err, raw)
	}
	caps := env.Result.Capabilities
	for _, key := range []string{"model_registrar", "model_router", "executor"} {
		if caps[key] != true {
			t.Fatalf("capabilities.%s 应为 true: %s", key, raw)
		}
	}
	if caps["executor_model_scope"] != "static" {
		t.Fatalf("executor_model_scope 应为 static: %v", caps["executor_model_scope"])
	}
	for _, key := range []string{"auth_provider", "model_provider", "scheduler"} {
		if v, ok := caps[key]; ok && v != false {
			t.Fatalf("不应声明 %s: %s", key, raw)
		}
	}
	for _, key := range []string{"executor_input_formats", "executor_output_formats"} {
		list, _ := caps[key].([]any)
		if len(list) != 1 || list[0] != "chat-completions" {
			t.Fatalf("%s 应为 [chat-completions]: %v", key, caps[key])
		}
	}
	if env.Result.Metadata["Name"] != "pggo" || env.Result.Metadata["Version"] != pluginVersion {
		t.Fatalf("metadata 错误: %v", env.Result.Metadata)
	}
}

// model.route 的请求按 CPA rpcModelRouteRequest 的真实编码构造（内嵌结构体无 tag → 大驼峰，
// Body 为 base64，外加 host_callback_id）；本插件前缀的模型返回 self，其余不处理。
func TestModelRouteRPC(t *testing.T) {
	setTestConfig(t, func(c *pluginConfig) {
		c.ModelPrefix = "pg/"
		c.HeadlessModelPrefix = "pggo/"
	})
	route := func(model string) modelRouteResponse {
		t.Helper()
		req, _ := json.Marshal(map[string]any{
			"Plugin":             map[string]any{"Name": "pggo"},
			"PluginID":           "pggo",
			"SourceFormat":       "openai",
			"RequestedModel":     model,
			"Stream":             true,
			"Headers":            map[string][]string{"Content-Type": {"application/json"}},
			"Query":              nil,
			"Body":               []byte(`{"model":"x","messages":[]}`),
			"Metadata":           map[string]any{},
			"AvailableProviders": []string{"gemini"},
			"host_callback_id":   "cb-1",
		})
		out, rc := handleRPC("model.route", req)
		if rc != 0 {
			t.Fatalf("rc=%d: %s", rc, out)
		}
		var env struct {
			OK     bool               `json:"ok"`
			Result modelRouteResponse `json:"result"`
		}
		if err := json.Unmarshal(out, &env); err != nil || !env.OK {
			t.Fatalf("响应解析失败: %v %s", err, out)
		}
		return env.Result
	}
	for _, model := range []string{"pggo/gemini-4-argon", "pg/gemini-2.5-pro"} {
		if r := route(model); !r.Handled || r.TargetKind != "self" {
			t.Fatalf("%s 应路由到 self: %+v", model, r)
		}
	}
	for _, model := range []string{"gemini-2.5-pro", "pggo/", "gpt-5", ""} {
		if r := route(model); r.Handled || r.TargetKind != "" {
			t.Fatalf("%s 不应被处理: %+v", model, r)
		}
	}
	// 坏请求不处理也不报错（报错会让宿主记警告，且对别的模型毫无意义）
	if out, rc := handleRPC("model.route", []byte("{")); rc != 0 || string(out) != `{"ok":true,"result":{"Handled":false}}` {
		t.Fatalf("坏请求应返回 Handled=false: rc=%d %s", rc, out)
	}

	// 前缀为空 = 通道停用，不再认领
	setTestConfig(t, func(c *pluginConfig) {
		c.ModelPrefix = ""
		c.HeadlessModelPrefix = "pggo/"
	})
	if r := route("pg/gemini-2.5-pro"); r.Handled {
		t.Fatalf("停用通道的模型不应被处理: %+v", r)
	}
	if r := route("pggo/gemini-2.5-pro"); !r.Handled {
		t.Fatalf("启用通道的模型应被处理: %+v", r)
	}
}

// setTestConfig 临时替换全局配置（不启动中枢），测试结束后恢复。
func setTestConfig(t *testing.T, mutate func(*pluginConfig)) {
	t.Helper()
	cfgMu.Lock()
	saved := cfg
	next := defaultConfig()
	mutate(&next)
	cfg = next
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg = saved
		cfgMu.Unlock()
	})
}
