package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func registerTestWS(t *testing.T, addr, path string) *testWS {
	t.Helper()
	c := dialTestWS(t, addr, path)
	t.Cleanup(func() { _ = c.c.Close() })
	if err := c.sendText(`{"type":"register","apiKey":"cat-test"}`); err != nil {
		t.Fatal(err)
	}
	if got := c.readText(t); !strings.Contains(got, "registered") {
		t.Fatalf("应收到 registered，得到 %s", got)
	}
	return c
}

// expectNoClose 在短窗口内确认连接没有收到关闭帧（读超时即视为仍在线）。
func expectNoClose(t *testing.T, c *testWS, what string) {
	t.Helper()
	_ = c.c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		op, payload, err := c.readFrame()
		if err != nil {
			return // 超时：没有关闭帧
		}
		if op == 0x8 {
			t.Fatalf("%s 不应被关闭，收到关闭帧 %v", what, payload)
		}
	}
}

type testStatus struct {
	Online      bool   `json:"online"`
	ConnectedAt *int64 `json:"connected_at"`
	Channels    map[string]struct {
		Online      bool   `json:"online"`
		ConnectedAt *int64 `json:"connected_at"`
	} `json:"channels"`
}

func getTestStatus(t *testing.T, addr string) (testStatus, string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/api/pg/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var st testStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("status 不是 JSON: %v %s", err, raw)
	}
	return st, string(raw)
}

// 同一 Key 的真实页面与无头 worker 可同时在线；4001 只在同通道内发生。
func TestChannelIsolationSameKey(t *testing.T) {
	h := startTestHub(t)
	addr := h.ln.Addr().String()
	page := registerTestWS(t, addr, "/api/pg/ws")
	headless := registerTestWS(t, addr, "/api/pg/ws?channel=headless")
	expectNoClose(t, page, "page 连接")
	expectNoClose(t, headless, "headless 连接")

	st, raw := getTestStatus(t, addr)
	if !st.Online || !st.Channels["page"].Online || !st.Channels["headless"].Online {
		t.Fatalf("两个通道都应在线: %s", raw)
	}

	// 第二个 headless 顶替第一个（4001），page 不受影响
	headless2 := registerTestWS(t, addr, "/api/pg/ws?channel=headless")
	if code := headless.readCloseCode(t); code != 4001 {
		t.Fatalf("旧 headless 应被 4001 顶替，得到 %d", code)
	}
	expectNoClose(t, page, "page 连接（headless 顶替后）")

	// 第二个 page 顶替第一个 page（4001），headless2 不受影响
	page2 := registerTestWS(t, addr, "/api/pg/ws?channel=page")
	if code := page.readCloseCode(t); code != 4001 {
		t.Fatalf("旧 page 应被 4001 顶替，得到 %d", code)
	}
	expectNoClose(t, headless2, "headless2 连接（page 顶替后）")
	_ = page2
}

// dispatch 只投给目标通道；目标通道离线时 503 并点名通道，不回落。
func TestDispatchRoutesToChannel(t *testing.T) {
	h := startTestHub(t)
	addr := h.ln.Addr().String()
	page := registerTestWS(t, addr, "/api/pg/ws")

	if _, err := h.dispatch(channelHeadless, "m", []any{"x"}, "hi"); err == nil {
		t.Fatal("headless 离线时应报错")
	} else if re, ok := err.(*rpcError); !ok || re.status != http.StatusServiceUnavailable || re.code != "executor_offline" || !strings.Contains(re.msg, "headless") {
		t.Fatalf("应返回点名 headless 的 503 executor_offline，得到 %#v", err)
	}
	expectNoClose(t, page, "page 连接")

	headless := registerTestWS(t, addr, "/api/pg/ws?channel=headless")
	done := make(chan error, 1)
	go func() {
		_, err := h.dispatch(channelHeadless, "m", []any{"x"}, "hi")
		done <- err
	}()
	msg := headless.readText(t)
	var req struct {
		Type      string `json:"type"`
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal([]byte(msg), &req)
	if req.Type != "request" || req.RequestID == "" {
		t.Fatalf("headless 应收到 request，得到 %s", msg)
	}
	// page 不应收到这条请求
	_ = page.c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if op, payload, err := page.readFrame(); err == nil && op == 0x1 {
		t.Fatalf("page 不应收到 headless 的请求: %s", payload)
	}
	end, _ := json.Marshal(map[string]any{"type": "stream-end", "requestId": req.RequestID})
	_ = headless.sendText(string(end))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("headless dispatch 失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("headless dispatch 超时")
	}
}

func TestUnknownChannelRejected(t *testing.T) {
	h := startTestHub(t)
	req, _ := http.NewRequest(http.MethodGet, "http://"+h.ln.Addr().String()+"/api/pg/ws?channel=bogus", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "MDEyMzQ1Njc4OWFiY2RlZg==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知通道应 400，得到 %d", resp.StatusCode)
	}
}

// 从未连接时不输出 Go 零值时间；连接后输出各通道状态。
func TestStatusJSONChannels(t *testing.T) {
	h := startTestHub(t)
	addr := h.ln.Addr().String()
	st, raw := getTestStatus(t, addr)
	if st.Online || st.ConnectedAt != nil || strings.Contains(raw, "-62135596800") {
		t.Fatalf("从未连接时不应有 connected_at: %s", raw)
	}
	for _, ch := range []string{"page", "headless"} {
		c, ok := st.Channels[ch]
		if !ok || c.Online || c.ConnectedAt != nil {
			t.Fatalf("通道 %s 状态不对: %s", ch, raw)
		}
	}

	headless := registerTestWS(t, addr, "/api/pg/ws?channel=headless")
	st, raw = getTestStatus(t, addr)
	if !st.Online || st.ConnectedAt == nil || *st.ConnectedAt <= 0 {
		t.Fatalf("headless 在线后顶层应 online 且有 connected_at: %s", raw)
	}
	if st.Channels["page"].Online || st.Channels["page"].ConnectedAt != nil {
		t.Fatalf("page 从未连接: %s", raw)
	}
	if hc := st.Channels["headless"]; !hc.Online || hc.ConnectedAt == nil {
		t.Fatalf("headless 应在线: %s", raw)
	}

	// 断开后 online=false，但保留最近一次 connected_at
	_ = headless.c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, raw = getTestStatus(t, addr)
		if !st.Channels["headless"].Online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("断开后 headless 应离线: %s", raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Online || st.Channels["headless"].ConnectedAt == nil {
		t.Fatalf("断开后状态不对: %s", raw)
	}
}

func TestRouteModelPrefixes(t *testing.T) {
	c := defaultConfig()
	c.ModelPrefix = "pg/"
	c.HeadlessModelPrefix = "pggo/"
	c.ModelMap = map[string]string{"pggo/alias": "gemini-real"}
	cases := []struct {
		in, ch, up string
	}{
		{"pg/gemini-3.8-flash", channelPage, "gemini-3.8-flash"},
		{"pggo/gemini-3.8-flash", channelHeadless, "gemini-3.8-flash"},
		{"pggo/models/gemini-2.5-pro", channelHeadless, "gemini-2.5-pro"},
		{"pggo/alias", channelHeadless, "gemini-real"},
		{"gemini-bare", channelPage, "gemini-bare"},
	}
	for _, tc := range cases {
		ch, up := routeModel(c, tc.in)
		if ch != tc.ch || up != tc.up {
			t.Errorf("routeModel(%q) = (%s, %s)，期望 (%s, %s)", tc.in, ch, up, tc.ch, tc.up)
		}
	}

	// 一个前缀是另一个的前缀时按最长匹配：pg 与 pggo
	c.ModelPrefix, c.HeadlessModelPrefix = "pg", "pggo"
	if ch, up := routeModel(c, "pggo-x"); ch != channelHeadless || up != "-x" {
		t.Errorf("最长匹配失败: %s %s", ch, up)
	}
	if ch, up := routeModel(c, "pg-x"); ch != channelPage || up != "-x" {
		t.Errorf("短前缀匹配失败: %s %s", ch, up)
	}
	// 反过来：页面前缀更长
	c.ModelPrefix, c.HeadlessModelPrefix = "pggo", "pg"
	if ch, _ := routeModel(c, "pggo-x"); ch != channelPage {
		t.Errorf("页面前缀更长时应走 page，得到 %s", ch)
	}

	// 默认前缀 pg- / pggo-
	d := defaultConfig()
	if ch, up := routeModel(d, "pggo-gemini-2.5-pro"); ch != channelHeadless || up != "gemini-2.5-pro" {
		t.Errorf("默认无头前缀: %s %s", ch, up)
	}
	if ch, up := routeModel(d, "pg-gemini-2.5-pro"); ch != channelPage || up != "gemini-2.5-pro" {
		t.Errorf("默认页面前缀: %s %s", ch, up)
	}
}

// 面板字段 model_prefix / headless_model_prefix 覆盖默认值，并注册两套模型。
func TestApplyConfigPrefixFieldsAndRegistration(t *testing.T) {
	section := "enabled: true\nlisten: 127.0.0.1:0\nmodel_prefix: \"pg/\"\nheadless_model_prefix: pggo/\napi-keys:\n  - k\nmodels:\n  - gemini-x\n  - gemini-y\n"
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte(section)})
	t.Cleanup(shutdownHub)
	applyConfig(req)
	c := currentConfig()
	if c.ModelPrefix != "pg/" || c.HeadlessModelPrefix != "pggo/" {
		t.Fatalf("前缀解析错误: %q %q", c.ModelPrefix, c.HeadlessModelPrefix)
	}
	got := map[string]bool{}
	for _, m := range modelRegistrationPayload().Models {
		got[m.ID] = true
	}
	for _, id := range []string{"pg/gemini-x", "pg/gemini-y", "pggo/gemini-x", "pggo/gemini-y"} {
		if !got[id] {
			t.Fatalf("缺少注册模型 %s: %v", id, got)
		}
	}
	if len(got) != 4 {
		t.Fatalf("注册模型数量应为 4: %v", got)
	}

	// 未填写时回落默认 pg- / pggo-
	section = "enabled: true\nlisten: 127.0.0.1:0\nmodel_prefix:\nheadless_model_prefix:\napi-keys:\n  - k\n"
	req, _ = json.Marshal(map[string]any{"config_yaml": []byte(section)})
	applyConfig(req)
	c = currentConfig()
	if c.ModelPrefix != "pg-" || c.HeadlessModelPrefix != "pggo-" {
		t.Fatalf("默认前缀错误: %q %q", c.ModelPrefix, c.HeadlessModelPrefix)
	}

	// 两个前缀相同：只注册一套，避免歧义
	section = "enabled: true\nlisten: 127.0.0.1:0\nmodel_prefix: same-\nheadless_model_prefix: same-\nmodels:\n  - m1\n"
	req, _ = json.Marshal(map[string]any{"config_yaml": []byte(section)})
	applyConfig(req)
	if n := len(modelRegistrationPayload().Models); n != 1 {
		t.Fatalf("前缀相同时应只注册一套，得到 %d", n)
	}
}

// /api/pg/exec 按模型前缀路由到对应通道。
func TestExecEndpointRoutesHeadless(t *testing.T) {
	cfgMu.Lock()
	saved := cfg
	cfg.ModelPrefix, cfg.HeadlessModelPrefix = "pg/", "pggo/"
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg = saved
		cfgMu.Unlock()
	})

	h := startTestHub(t)
	addr := h.ln.Addr().String()
	page := registerTestWS(t, addr, "/api/pg/ws")
	headless := registerTestWS(t, addr, "/api/pg/ws?channel=headless")

	serve := func(c *testWS, tag string) {
		for {
			op, payload, err := c.readFrame()
			if err != nil {
				return
			}
			if op != 0x1 || !strings.Contains(string(payload), `"request"`) {
				continue
			}
			var m struct {
				RequestID   string `json:"requestId"`
				Model       string `json:"model"`
				RequestBody string `json:"requestBody"`
			}
			_ = json.Unmarshal(payload, &m)
			var rb struct {
				Wire []any `json:"wire"`
			}
			_ = json.Unmarshal([]byte(m.RequestBody), &rb)
			model, _ := rb.Wire[0].(string)
			body := marshalRoot([]any{textFrame(tag + ":" + model)})
			chunk, _ := json.Marshal(map[string]any{"type": "stream-chunk", "requestId": m.RequestID, "chunkBody": body})
			end, _ := json.Marshal(map[string]any{"type": "stream-end", "requestId": m.RequestID})
			_ = c.sendText(string(chunk))
			_ = c.sendText(string(end))
		}
	}
	_ = page.c.SetReadDeadline(time.Time{})
	_ = headless.c.SetReadDeadline(time.Time{})
	go serve(page, "page")
	go serve(headless, "headless")

	call := func(model string) string {
		payload, _ := json.Marshal(map[string]any{
			"model":        model,
			"request_json": `{"messages":[{"role":"user","content":"ping"}]}`,
		})
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/pg/exec", strings.NewReader(string(payload)))
		req.Header.Set("X-API-Key", "cat-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Text string `json:"text"`
		}
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s 状态码 %d: %s", model, resp.StatusCode, raw)
		}
		_ = json.Unmarshal(raw, &out)
		return out.Text
	}
	if got := call("pg/gemini-x"); got != "page:models/gemini-x" {
		t.Fatalf("pg/ 应路由到 page: %q", got)
	}
	if got := call("pggo/gemini-x"); got != "headless:models/gemini-x" {
		t.Fatalf("pggo/ 应路由到 headless: %q", got)
	}
}
