package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ------------------------------------------------- 测试用最小 WS 客户端 ----

type testWS struct {
	c  net.Conn
	br *bufio.Reader
}

func dialTestWS(t *testing.T, addr, path string) *testWS {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接 WS 失败: %v", err)
	}
	req := "GET " + path + " HTTP/1.1\r\nHost: " + addr + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: MDEyMzQ1Njc4OWFiY2RlZg==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatalf("发送握手失败: %v", err)
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("握手失败: %q %v", status, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读握手头失败: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	return &testWS{c: c, br: br}
}

func (w *testWS) sendText(payload string) error {
	data := []byte(payload)
	mask := []byte{1, 2, 3, 4}
	header := []byte{0x81}
	n := len(data)
	switch {
	case n < 126:
		header = append(header, byte(0x80|n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	header = append(header, mask...)
	masked := make([]byte, n)
	for i := range data {
		masked[i] = data[i] ^ mask[i%4]
	}
	if _, err := w.c.Write(append(header, masked...)); err != nil {
		return err
	}
	return nil
}

// readFrame 读一帧（服务端→客户端不掩码）。
func (w *testWS) readFrame() (byte, []byte, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(w.br, head); err != nil {
		return 0, nil, err
	}
	op := head[0] & 0x0f
	length := int(head[1] & 0x7f)
	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(w.br, ext); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(w.br, ext); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(ext))
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return 0, nil, err
		}
	}
	return op, payload, nil
}

func (w *testWS) readText(t *testing.T) string {
	t.Helper()
	for {
		_ = w.c.SetReadDeadline(time.Now().Add(5 * time.Second))
		op, payload, err := w.readFrame()
		if err != nil {
			t.Fatalf("读 WS 帧失败: %v", err)
		}
		if op == 0x1 {
			return string(payload)
		}
	}
}

func (w *testWS) readCloseCode(t *testing.T) int {
	t.Helper()
	_ = w.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		op, payload, err := w.readFrame()
		if err != nil {
			t.Fatalf("等待关闭帧失败: %v", err)
		}
		if op == 0x8 {
			if len(payload) < 2 {
				return 0
			}
			return int(binary.BigEndian.Uint16(payload[:2]))
		}
	}
}

// ------------------------------------------------------------- 测试 ----

func startTestHub(t *testing.T) *hub {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "script.user.js")
	if err := os.WriteFile(script, []byte("// ==UserScript==\n// @version 9.9.9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Listen = "127.0.0.1:0"
	cfg.APIKeys = []string{"cat-test"}
	cfg.ScriptPath = script
	cfg.DataDir = dir
	cfg.RequestTimeoutSec = 5
	h := newHub(cfg)
	if err := h.start(); err != nil {
		t.Fatalf("启动测试中枢失败: %v", err)
	}
	t.Cleanup(h.close)
	return h
}

func TestWSRejectsBadKey(t *testing.T) {
	h := startTestHub(t)
	c := dialTestWS(t, h.ln.Addr().String(), "/api/pg/ws")
	defer c.c.Close()
	if err := c.sendText(`{"type":"register","apiKey":"cat-wrong"}`); err != nil {
		t.Fatal(err)
	}
	if code := c.readCloseCode(t); code != 4008 {
		t.Fatalf("无效 Key 应关闭 4008，得到 %d", code)
	}
}

func TestDispatchRoundTrip(t *testing.T) {
	h := startTestHub(t)
	addr := h.ln.Addr().String()
	c := dialTestWS(t, addr, "/api/pg/ws")
	defer c.c.Close()
	if err := c.sendText(`{"type":"register","clientVersion":"2.21.1","apiKey":"cat-test","executorType":"playground"}`); err != nil {
		t.Fatal(err)
	}
	if got := c.readText(t); !strings.Contains(got, "registered") {
		t.Fatalf("应收到 registered，得到 %s", got)
	}

	body := marshalRoot([]any{textFrame("你好，主人"), usageFrame(10, 4)})
	type out struct {
		raw string
		err error
	}
	done := make(chan out, 1)
	go func() {
		raw, err := h.dispatch(channelPage, "gemini-3.8-flash", []any{"models/gemini-3.8-flash"}, "hi")
		done <- out{raw, err}
	}()

	msg := c.readText(t)
	var req struct {
		Type        string `json:"type"`
		RequestID   string `json:"requestId"`
		Model       string `json:"model"`
		Stream      bool   `json:"stream"`
		RequestBody string `json:"requestBody"`
	}
	if err := json.Unmarshal([]byte(msg), &req); err != nil {
		t.Fatalf("请求消息不是 JSON: %v", err)
	}
	if req.Type != "request" || req.RequestID == "" || !req.Stream {
		t.Fatalf("请求消息字段不对: %+v", req)
	}
	var rb map[string]any
	if err := json.Unmarshal([]byte(req.RequestBody), &rb); err != nil {
		t.Fatalf("requestBody 必须是 JSON 字符串: %v", err)
	}
	if rb["binding"] != "hi" || rb["wire"] == nil {
		t.Fatalf("requestBody 内容不对: %+v", rb)
	}

	// 模拟页面真流式：分两块推回
	half := len(body) / 2
	chunk1, _ := json.Marshal(map[string]any{"type": "stream-chunk", "requestId": req.RequestID, "chunkBody": body[:half]})
	chunk2, _ := json.Marshal(map[string]any{"type": "stream-chunk", "requestId": req.RequestID, "chunkBody": body[half:]})
	end, _ := json.Marshal(map[string]any{"type": "stream-end", "requestId": req.RequestID, "status": 200})
	for _, m := range []string{string(chunk1), string(chunk2), string(end)} {
		if err := c.sendText(m); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("dispatch 失败: %v", o.err)
		}
		if o.raw != body {
			t.Fatalf("分块拼接结果不一致")
		}
		res, err := decodeUpstream(o.raw)
		if err != nil {
			t.Fatalf("解码失败: %v", err)
		}
		if res.Text != "你好，主人" {
			t.Fatalf("文本错误: %q", res.Text)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("dispatch 超时")
	}
}

// 端到端：/api/pg/exec + 假执行器，走完 OpenAI 请求 → wire → 文本
func TestExecEndpointWithFakeExecutor(t *testing.T) {
	h := startTestHub(t)
	addr := h.ln.Addr().String()
	c := dialTestWS(t, addr, "/api/pg/ws")
	defer c.c.Close()
	if err := c.sendText(`{"type":"register","apiKey":"cat-test"}`); err != nil {
		t.Fatal(err)
	}
	if got := c.readText(t); !strings.Contains(got, "registered") {
		t.Fatalf("注册失败: %s", got)
	}

	go func() {
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
				RequestBody string `json:"requestBody"`
			}
			_ = json.Unmarshal(payload, &m)
			var rb struct {
				Binding string `json:"binding"`
				Wire    []any  `json:"wire"`
			}
			_ = json.Unmarshal([]byte(m.RequestBody), &rb)
			body := marshalRoot([]any{textFrame("echo:" + rb.Binding)})
			chunk, _ := json.Marshal(map[string]any{"type": "stream-chunk", "requestId": m.RequestID, "chunkBody": body})
			end, _ := json.Marshal(map[string]any{"type": "stream-end", "requestId": m.RequestID})
			_ = c.sendText(string(chunk))
			_ = c.sendText(string(end))
		}
	}()

	payload := `{"model":"pg-gemini-3.8-flash","request_json":"{\"model\":\"pg-gemini-3.8-flash\",\"messages\":[{\"role\":\"system\",\"content\":\"s\"},{\"role\":\"user\",\"content\":\"ping\"}]}"}`
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/pg/exec", strings.NewReader(payload))
	req.Header.Set("X-API-Key", "cat-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out.Text != "echo:ping" {
		t.Fatalf("文本错误: %q", out.Text)
	}
}

func TestHTTPEndpointsAuthAndStores(t *testing.T) {
	h := startTestHub(t)
	addr := h.ln.Addr().String()

	// 无 Key 的推送应 401
	resp, err := http.Post("http://"+addr+"/api/pggo/push", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("无 Key 推送应 401，得到 %d", resp.StatusCode)
	}

	// 带 Key 的 Cookie 推送
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/pggo/push", strings.NewReader(`{"email":"a@b.c","authuser":"1","cookies":[{"name":"SID"},{"name":"HSID"}]}`))
	req.Header.Set("X-API-Key", "cat-test")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var push struct {
		Label   string `json:"label"`
		Cookies int    `json:"cookies"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&push)
	resp.Body.Close()
	if resp.StatusCode != 200 || push.Cookies != 2 || push.Label != "a@b.c" {
		t.Fatalf("推送响应不符: %d %+v", resp.StatusCode, push)
	}
	if _, err := os.Stat(filepath.Join(h.cfg.DataDir, "cookies", "a@b.c.json")); err != nil {
		t.Fatalf("Cookie 未落盘: %v", err)
	}

	// Robin 绑定
	req, _ = http.NewRequest(http.MethodPost, "http://"+addr+"/api/robin/bind-by-key", strings.NewReader(`{"email":"a@b.c","oauth_token":"tok"}`))
	req.Header.Set("X-API-Key", "cat-test")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var robin struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&robin)
	resp.Body.Close()
	if robin.Status != "ok" {
		t.Fatalf("Robin 绑定响应不符: %+v", robin)
	}

	// 脚本分发
	resp, err = http.Get("http://" + addr + "/api/pg/script.user.js")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "@version") {
		t.Fatalf("脚本分发失败: %d", resp.StatusCode)
	}

	// 状态
	resp, err = http.Get("http://" + addr + "/api/pg/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Online bool `json:"online"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&status)
	resp.Body.Close()
	if status.Online {
		t.Fatal("还没有执行器注册，online 应为 false")
	}
}

func TestDispatchWithoutExecutor(t *testing.T) {
	h := startTestHub(t)
	if _, err := h.dispatch(channelPage, "m", []any{"x"}, "hi"); err == nil {
		t.Fatal("没有执行器时应报错")
	} else if re, ok := err.(*rpcError); !ok || re.status != http.StatusServiceUnavailable {
		t.Fatalf("应返回 503 语义错误，得到 %v", err)
	}
}

func TestReplacedConnectionGets4001(t *testing.T) {
	h := startTestHub(t)
	addr := h.ln.Addr().String()
	first := dialTestWS(t, addr, "/api/pg/ws")
	defer first.c.Close()
	_ = first.sendText(`{"type":"register","apiKey":"cat-test"}`)
	_ = first.readText(t)

	second := dialTestWS(t, addr, "/api/pg/ws")
	defer second.c.Close()
	_ = second.sendText(`{"type":"register","apiKey":"cat-test"}`)
	_ = second.readText(t)

	if code := first.readCloseCode(t); code != 4001 {
		t.Fatalf("旧连接应被 4001 顶替，得到 %d", code)
	}
}

// CPA 实际只下发 plugins.configs.pggo 这一节（不含顶层 api-keys），
// Key 必须从本节 api-keys 与 JSON api_keys 合并得到。
func TestApplyConfigUsesPluginSectionKeys(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "pggo.json")
	if err := os.WriteFile(jsonPath, []byte(`{"listen":"127.0.0.1:0","api_keys":["json-key"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	section := "enabled: true\npriority: 1\nconfig_file: \"" + filepath.ToSlash(jsonPath) + "\"\napi-keys:\n  - sec-key\n"
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte(section), "schema_version": 10})
	t.Cleanup(shutdownHub)
	applyConfig(req)

	c := currentConfig()
	if len(c.APIKeys) != 2 || c.APIKeys[0] != "json-key" || c.APIKeys[1] != "sec-key" {
		t.Fatalf("Key 合并错误: %v", c.APIKeys)
	}
	h := activeHub()
	if h == nil {
		t.Fatal("中枢未启动")
	}
	if !h.keyValid("sec-key") || !h.keyValid("json-key") || h.keyValid("other") {
		t.Fatal("中枢 Key 校验与配置不一致")
	}
}

// 流式响应字段名须与宿主 rpcExecutorStreamResponse 的小写 json tag 一致。
func TestStreamResponseFieldNames(t *testing.T) {
	raw, _ := json.Marshal(executorStreamResponse{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  []streamChunk{{Payload: []byte("data: x\n\n")}},
	})
	s := string(raw)
	if !strings.Contains(s, `"headers"`) || !strings.Contains(s, `"chunks"`) || !strings.Contains(s, `"Payload"`) {
		t.Fatalf("字段名不匹配: %s", s)
	}
}

// 面板保存后宿主下发的形态：数组字段可能是缩进或紧凑列表，空值键不能吞下一行。
func TestApplyConfigPanelFields(t *testing.T) {
	section := "enabled: true\npriority: 0\nlisten: 127.0.0.1:0\nscript_path:\ndata_dir: \"/data/pggo\"\napi-keys:\n- k-compact\nmodels:\n    - gemini-x\n    - gemini-y\n"
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte(section)})
	t.Cleanup(shutdownHub)
	applyConfig(req)
	c := currentConfig()
	if c.Listen != "127.0.0.1:0" || c.DataDir != "/data/pggo" || c.ScriptPath != "" {
		t.Fatalf("标量字段解析错误: listen=%q data=%q script=%q", c.Listen, c.DataDir, c.ScriptPath)
	}
	if len(c.APIKeys) != 1 || c.APIKeys[0] != "k-compact" {
		t.Fatalf("紧凑列表 api-keys 解析错误: %v", c.APIKeys)
	}
	if len(c.Models) != 2 || c.Models[1] != "gemini-y" {
		t.Fatalf("models 解析错误: %v", c.Models)
	}
}

// listen 变更时中枢应关旧端口、绑新端口，并断开旧 WS 连接。
func TestEnsureHubRebindsOnListenChange(t *testing.T) {
	t.Cleanup(shutdownHub)
	shutdownHub()
	c := defaultConfig()
	c.Listen = "127.0.0.1:0"
	c.APIKeys = []string{"cat-test"}
	ensureHub(c)
	old := activeHub()
	if old == nil {
		t.Fatal("中枢未启动")
	}
	oldAddr := old.ln.Addr().String()
	ws := dialTestWS(t, oldAddr, "/api/pg/ws")
	if err := ws.sendText(`{"type":"register","apiKey":"cat-test"}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.readFrame(); err != nil { // registered
		t.Fatal(err)
	}

	c2 := c
	c2.Listen = "127.0.0.1:0" // 同字面值不重绑
	ensureHub(c2)
	if activeHub() != old {
		t.Fatal("监听地址未变却重建了中枢")
	}

	c3 := c
	c3.Listen = "localhost:0"
	ensureHub(c3)
	nh := activeHub()
	if nh == nil || nh == old {
		t.Fatal("监听地址变更后未重建中枢")
	}
	op, payload, err := ws.readFrame()
	if err != nil || op != 0x8 || len(payload) < 2 || binary.BigEndian.Uint16(payload) != 1001 {
		t.Fatalf("旧连接应收到 1001 关闭帧: op=%x payload=%v err=%v", op, payload, err)
	}
	if _, err := net.DialTimeout("tcp", oldAddr, time.Second); err == nil {
		t.Fatal("旧端口仍在监听")
	}
}

// 页面重复发送 stream-end 不得触发重复 close panic。
func TestFinishTwiceIsNoop(t *testing.T) {
	h := &hub{pending: map[string]*waiter{"r1": {done: make(chan struct{})}}}
	h.finish(nil, "r1", nil)
	h.finish(nil, "r1", nil)
	h.appendChunk(nil, "r1", "late")
}
