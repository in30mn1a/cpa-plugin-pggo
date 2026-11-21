package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// 半开的执行器连接（对端从不读取）上卡住的写操作，不能拖住 plugin.quiesce：
// 关闭流程不等写锁，直接断开底层连接，卡住的 dispatch 随之返回。
func TestQuiesceNotBlockedByStuckWriter(t *testing.T) {
	t.Cleanup(shutdownHub)
	shutdownHub()
	server, client := net.Pipe() // client 端从不读取：服务端的写会一直阻塞
	defer client.Close()
	conn := &wsConn{c: server, br: bufio.NewReader(server)}

	c := defaultConfig()
	c.RequestTimeoutSec = 60
	h := newHub(c)
	h.conns[channelPage] = conn
	h.ws[conn] = struct{}{}
	hubMu.Lock()
	theHub = h
	hubMu.Unlock()

	errc := make(chan error, 1)
	go func() {
		_, err := h.dispatch(channelPage, "gemini-x", []any{"w"}, "")
		errc <- err
	}()
	waitFor(t, "dispatch 进入阻塞写", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.pending) == 1
	})
	time.Sleep(50 * time.Millisecond) // 让 dispatch 真正卡在 Write 里并持有写锁

	start := time.Now()
	out, rc := handleRPC("plugin.quiesce", []byte("{}"))
	if rc != 0 || !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("quiesce 应回 OK: %s", out)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("quiesce 被卡住的写者拖了 %s", d)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("卡住的 dispatch 应以错误返回")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("断开连接后卡住的 dispatch 仍未返回")
	}
}

// 普通写操作有写超时：对端不读时不会永久卡住。
func TestWriteFrameHasDeadline(t *testing.T) {
	saved := wsWriteTimeout
	wsWriteTimeout = 50 * time.Millisecond
	t.Cleanup(func() { wsWriteTimeout = saved })
	server, client := net.Pipe()
	defer client.Close()
	defer server.Close()
	conn := &wsConn{c: server, br: bufio.NewReader(server)}
	done := make(chan error, 1)
	go func() { done <- conn.writeText([]byte("hello")) }()
	select {
	case err := <-done:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("应以写超时返回: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("写操作没有超时")
	}
}

// C ABI 入口的 panic 兜底：返回 500 错误信封而不是让 panic 穿过 cgo 带走宿主。
func TestRecoverRPCConvertsPanic(t *testing.T) {
	out, rc := recoverRPC("model.route", func() ([]byte, int) { panic("boom") })
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil || rc != 1 || env.OK || env.Error.Code != "plugin_panic" || env.Error.HTTPStatus != 500 {
		t.Fatalf("应返回 500 plugin_panic: rc=%d %s", rc, out)
	}
	if out, rc := safeHandleRPC("executor.identifier", nil); rc != 0 || !bytes.Contains(out, []byte(`"pggo"`)) {
		t.Fatalf("正常调用应透传: rc=%d %s", rc, out)
	}
}

// 构造一份带大请求体的 model.route 请求（CPA 编码：Body 为 base64）。
func routeRequest(model string, bodyBytes int) []byte {
	body := bytes.Repeat([]byte("x"), bodyBytes)
	raw, _ := json.Marshal(map[string]any{
		"PluginID": "pggo", "SourceFormat": "openai", "RequestedModel": model,
		"Stream": true, "Body": body, "Metadata": map[string]any{"k": "v"}, "host_callback_id": "cb",
	})
	return raw
}

// 非本插件模型的路由路径：不解码 Body（分配量与请求体大小无关）、不记日志。
func TestModelRouteNotOursAllocsIndependentOfBody(t *testing.T) {
	setTestConfig(t, func(c *pluginConfig) {})
	small, large := routeRequest("gpt-5", 16), routeRequest("gpt-5", 256<<10)
	allocs := func(req []byte) float64 {
		return testing.AllocsPerRun(50, func() { _, _ = handleRPC("model.route", req) })
	}
	a, b := allocs(small), allocs(large)
	if b > a+1 || b > 8 {
		t.Fatalf("分配次数不应随请求体增长: small=%.0f large=%.0f", a, b)
	}
	if out, _ := handleRPC("model.route", large); string(out) != `{"ok":true,"result":{"Handled":false}}` {
		t.Fatalf("非本插件模型应不处理: %s", out)
	}
}

// 快速路径与完整解码结果一致；拿不准时回落（ok=false）。
func TestFastRequestedModel(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		want   string
		wantOK bool
	}{
		{`{"PluginID":"pggo","RequestedModel":"pggo/gemini-4-argon","Body":"eA=="}`, "pggo/gemini-4-argon", true},
		{`{"RequestedModel": "pg/gemini \"x\""}`, `pg/gemini "x"`, true},
		{`{"RequestedModel":""}`, "", true},
		{`{"RequestedModel":null}`, "", false},
		{`{"RequestedModel":"unterminated`, "", false},
		{`{"requested_model":"x"}`, "", false},
	} {
		got, ok := fastRequestedModel([]byte(tc.raw))
		if got != tc.want || ok != tc.wantOK {
			t.Fatalf("%s → (%q,%v)，应为 (%q,%v)", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
	req := routeRequest("pggo/gemini-x", 1024)
	got, ok := fastRequestedModel(req)
	var full modelRouteRequest
	_ = json.Unmarshal(req, &full)
	if !ok || got != full.RequestedModel {
		t.Fatalf("快速路径 %q 与完整解码 %q 不一致", got, full.RequestedModel)
	}
}

func BenchmarkModelRouteNotOurs(b *testing.B) {
	req := routeRequest("gpt-5", 64<<10)
	b.ReportAllocs()
	b.SetBytes(int64(len(req)))
	for i := 0; i < b.N; i++ {
		_, _ = handleRPC("model.route", req)
	}
}
