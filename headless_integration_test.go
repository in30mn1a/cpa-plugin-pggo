package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestHeadlessWorkerIntegration 是与无头 worker 容器联调的本地集成测试，默认跳过。
//
// 用法（见 headless/README 段落「本地集成测试」）：
//
//	PGGO_HEADLESS_IT=1 PGGO_HEADLESS_IT_LISTEN=0.0.0.0:18788 PGGO_HEADLESS_IT_DATA=<dir> \
//	  go test -count=1 -run TestHeadlessWorkerIntegration -v -timeout 10m .
//
// 然后在另一个终端启动 worker 容器（指向本中枢、挂载 <dir>/cookies 与桩页面入口）。
// 测试会：推送一份假 Cookie（走 /api/pggo/push 真实落盘路径）→ 等 headless 通道注册 →
// 同 Key 再挂一个 page 执行器验证双通道共存 → 用 pggo- 模型调 /api/pg/exec，
// 断言请求确实到达注入的用户脚本并由脚本回包（桩页面无法完成引导，预期是脚本侧错误而非超时/离线）。
func TestHeadlessWorkerIntegration(t *testing.T) {
	if os.Getenv("PGGO_HEADLESS_IT") == "" {
		t.Skip("设置 PGGO_HEADLESS_IT=1 才运行（需要 worker 容器配合）")
	}
	listen := os.Getenv("PGGO_HEADLESS_IT_LISTEN")
	if listen == "" {
		listen = "0.0.0.0:18788"
	}
	dataDir := os.Getenv("PGGO_HEADLESS_IT_DATA")
	if dataDir == "" {
		dataDir = t.TempDir()
	}
	c := defaultConfig()
	c.Listen = listen
	c.APIKeys = []string{"cat-it"}
	c.DataDir = dataDir
	c.RequestTimeoutSec = 150
	cfgMu.Lock()
	saved := cfg
	cfg = c
	cfgMu.Unlock()
	t.Cleanup(func() {
		cfgMu.Lock()
		cfg = saved
		cfgMu.Unlock()
	})
	h := newHub(c)
	if err := h.start(); err != nil {
		t.Fatalf("启动中枢失败: %v", err)
	}
	t.Cleanup(h.close)
	addr := strings.Replace(h.ln.Addr().String(), "0.0.0.0", "127.0.0.1", 1)
	addr = strings.Replace(addr, "[::]", "127.0.0.1", 1)

	// 1. 假 Cookie 推送（字段形态与用户脚本 pushCookiesToCloud 一致；值是假的）
	exp := float64(time.Now().Add(24 * time.Hour).Unix())
	var cookies []map[string]any
	for _, n := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID", "__Secure-1PSID", "__Secure-3PSID", "SID", "HSID", "SSID"} {
		cookies = append(cookies, map[string]any{
			"name": n, "value": "fake-" + strings.ToLower(n), "domain": ".google.com", "path": "/",
			"expirationDate": exp, "httpOnly": strings.HasPrefix(n, "__Secure-") || n == "SID" || n == "HSID",
			"secure": n != "HSID" && n != "SID", "sameSite": "unspecified",
		})
	}
	push, _ := json.Marshal(map[string]any{
		"email":    "it@gmail.com",
		"authuser": "0",
		"fingerprint": map[string]any{
			"userAgent":           "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
			"platform":            "Win32",
			"language":            "zh-CN",
			"timezone":            "Asia/Shanghai",
			"hardwareConcurrency": 12,
			"screenWidth":         1920, "screenHeight": 1080,
			"innerWidth": 1600, "innerHeight": 900,
			"devicePixelRatio": 1,
		},
		"cookies": cookies,
	})
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/pggo/push", strings.NewReader(string(push)))
	req.Header.Set("X-API-Key", "cat-it")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("推送失败: %d", resp.StatusCode)
	}
	t.Logf("假 Cookie 已推送到 %s/cookies，等待 headless worker 注册…", dataDir)

	// 2. 等 headless 通道注册
	deadline := time.Now().Add(5 * time.Minute)
	for {
		st, _ := getTestStatus(t, addr)
		if st.Channels["headless"].Online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("5 分钟内 headless 通道未注册")
		}
		time.Sleep(time.Second)
	}
	t.Log("headless 通道已注册")

	// 3. 同 Key 再挂一个 page 执行器：两通道应同时在线
	page := dialTestWS(t, addr, "/api/pg/ws")
	defer page.c.Close()
	_ = page.sendText(`{"type":"register","apiKey":"cat-it"}`)
	if got := page.readText(t); !strings.Contains(got, "registered") {
		t.Fatalf("page 注册失败: %s", got)
	}
	st, raw := getTestStatus(t, addr)
	if !st.Channels["page"].Online || !st.Channels["headless"].Online {
		t.Fatalf("两个通道应同时在线: %s", raw)
	}
	t.Logf("status: %s", raw)

	// 4. pggo- 模型请求经 headless 通道到达脚本
	payload, _ := json.Marshal(map[string]any{
		"model":        "pggo-gemini-3.8-flash",
		"request_json": `{"messages":[{"role":"user","content":"ping"}]}`,
	})
	req, _ = http.NewRequest(http.MethodPost, "http://"+addr+"/api/pg/exec", strings.NewReader(string(payload)))
	req.Header.Set("X-API-Key", "cat-it")
	started := time.Now()
	resp, err = (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("exec 返回 %d（%s）: %s", resp.StatusCode, time.Since(started).Round(time.Second), body)
	s := string(body)
	if strings.Contains(s, "没有在线") || strings.Contains(s, "秒内没有回结果") {
		t.Fatalf("请求没有被脚本处理: %s", s)
	}
	// page 通道不应收到这条请求
	_ = page.c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if op, p, err := page.readFrame(); err == nil && op == 0x1 {
		t.Fatalf("page 不应收到 headless 请求: %s", p)
	}

	// 5. 浏览器重新推送 Cookie → worker 应检测到文件变更、重启会话并重新注册
	st, _ = getTestStatus(t, addr)
	before := *st.Channels["headless"].ConnectedAt
	time.Sleep(1100 * time.Millisecond) // connected_at 精度为秒
	req, _ = http.NewRequest(http.MethodPost, "http://"+addr+"/api/pggo/push", strings.NewReader(string(push)))
	req.Header.Set("X-API-Key", "cat-it")
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	deadline = time.Now().Add(2 * time.Minute)
	for {
		st, raw = getTestStatus(t, addr)
		if hc := st.Channels["headless"]; hc.Online && hc.ConnectedAt != nil && *hc.ConnectedAt > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("重新推送 Cookie 后 2 分钟内 headless 未重新注册: %s", raw)
		}
		time.Sleep(time.Second)
	}
	t.Log("重新推送 Cookie 后 headless 已重新注册")

	// 可选：保持中枢在线一段时间，便于手动重启 worker 验证状态文件复用等行为
	if hold, _ := time.ParseDuration(os.Getenv("PGGO_HEADLESS_IT_HOLD")); hold > 0 {
		t.Logf("保持中枢在线 %s", hold)
		time.Sleep(hold)
	}
}
