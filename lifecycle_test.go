package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// freeAddr 返回一个当前空闲的本机端口地址。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func lifecycleConfig(addr string) pluginConfig {
	c := defaultConfig()
	c.Listen = addr
	c.APIKeys = []string{"cat-test"}
	c.RequestTimeoutSec = 10
	return c
}

// fastBindRetry 把重绑节奏调到毫秒级；须在 ensureHub 之前调用（之后由 Cleanup 恢复，
// 恢复发生在 shutdownHub 之后，届时重绑协程已退出，不会与之竞争）。
func fastBindRetry(t *testing.T) {
	t.Helper()
	fast, window, slow, every := bindRetryFast, bindRetryFastWindow, bindRetrySlow, bindRetryLogEvery
	t.Cleanup(func() {
		bindRetryFast, bindRetryFastWindow, bindRetrySlow, bindRetryLogEvery = fast, window, slow, every
	})
	bindRetryFast, bindRetryFastWindow, bindRetrySlow, bindRetryLogEvery = 5*time.Millisecond, 50*time.Millisecond, 20*time.Millisecond, time.Hour
}

func retrying() bool {
	hubMu.Lock()
	defer hubMu.Unlock()
	return hubRetry != nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("超时：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func expectClose1001(t *testing.T, ws *testWS, what string) {
	t.Helper()
	_ = ws.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		op, payload, err := ws.readFrame()
		if err != nil {
			t.Fatalf("%s：未收到关闭帧: %v", what, err)
		}
		if op == 0x8 {
			if len(payload) < 2 || binary.BigEndian.Uint16(payload) != 1001 {
				t.Fatalf("%s：关闭码应为 1001: %v", what, payload)
			}
			return
		}
	}
}

// plugin.quiesce：回 OK、释放端口（第二个中枢能绑同一端口）、已注册与未注册的 WS 都收到
// 1001、进行中的请求立即失败；之后 plugin.register（ensureHub）能重新拉起中枢。
func TestQuiesceReleasesPortForNewHub(t *testing.T) {
	t.Cleanup(shutdownHub)
	shutdownHub()
	addr := freeAddr(t)
	c := lifecycleConfig(addr)
	ensureHub(c)
	h := activeHub()
	if h == nil {
		t.Fatal("中枢未启动")
	}

	exec := registerTestWS(t, addr, "/api/pg/ws")
	idle := dialTestWS(t, addr, "/api/pg/ws?channel=headless") // 已升级但从未 register
	defer exec.c.Close()
	defer idle.c.Close()
	waitFor(t, "未注册连接登记", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.ws) == 2
	})

	errc := make(chan error, 1)
	go func() {
		_, err := h.dispatch(channelPage, "gemini-x", []any{"w"}, "")
		errc <- err
	}()
	if msg := exec.readText(t); !strings.Contains(msg, `"type":"request"`) {
		t.Fatalf("执行器应先收到请求: %s", msg)
	}

	out, rc := handleRPC("plugin.quiesce", []byte("{}"))
	if rc != 0 || string(out) != `{"ok":true,"result":{}}` {
		t.Fatalf("quiesce 应回 OK: rc=%d %s", rc, out)
	}
	if activeHub() != nil {
		t.Fatal("quiesce 后不应再有活动中枢")
	}
	select {
	case err := <-errc:
		var re *rpcError
		if !errors.As(err, &re) || re.code != "hub_restarting" {
			t.Fatalf("进行中的请求应以 hub_restarting 失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("进行中的请求未被立即失败")
	}
	expectClose1001(t, exec, "已注册连接")
	expectClose1001(t, idle, "未注册连接")

	// 新实例（另一份 .so 的独立中枢）能绑定同一端口
	next := newHub(c)
	if err := next.start(); err != nil {
		t.Fatalf("quiesce 后第二个中枢应能绑定 %s: %v", addr, err)
	}
	next.close()

	// 后续非激活类 RPC 不会重启中枢
	_, _ = handleRPC("executor.identifier", nil)
	if activeHub() != nil {
		t.Fatal("非 register/reconfigure 的 RPC 不应重启中枢")
	}
	// CPA 回滚时对旧实例再调 plugin.register → 中枢恢复
	ensureHub(c)
	if activeHub() == nil {
		t.Fatal("重新激活后中枢应恢复")
	}
}

// 端口被占用时后台重试；同地址的配置更新作用于等待中的中枢；端口释放后自动接管，
// /api/pg/status 带 version。
func TestBindRetryAfterPortFrees(t *testing.T) {
	fastBindRetry(t)
	t.Cleanup(shutdownHub)
	shutdownHub()
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := blocker.Addr().String()
	ensureHub(lifecycleConfig(addr))
	if activeHub() != nil || !retrying() {
		_ = blocker.Close()
		t.Fatal("端口被占用时应进入后台重试")
	}
	time.Sleep(80 * time.Millisecond) // 越过快速窗口，进入慢速节奏
	c2 := lifecycleConfig(addr)
	c2.APIKeys = []string{"cat-new"}
	ensureHub(c2) // 同地址：只更新配置，不新开重试
	if !retrying() {
		t.Fatal("同地址配置更新不应取消重试")
	}
	_ = blocker.Close()
	waitFor(t, "端口释放后接管", func() bool { return activeHub() != nil })
	if retrying() {
		t.Fatal("接管后重试状态应清空")
	}
	h := activeHub()
	if !h.keyValid("cat-new") || h.keyValid("cat-test") {
		t.Fatal("重试期间的配置更新应生效")
	}
	resp, err := http.Get("http://" + addr + "/api/pg/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st["version"] != pluginVersion {
		t.Fatalf("status.version = %v，应为 %s", st["version"], pluginVersion)
	}
}

// quiesce / shutdown 取消后台重试：返回时协程已退出，端口释放后也不会被抢占。
func TestBindRetryCancelledByQuiesce(t *testing.T) {
	fastBindRetry(t)
	t.Cleanup(shutdownHub)
	for _, method := range []string{"plugin.quiesce", "plugin.shutdown"} {
		shutdownHub()
		blocker, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := blocker.Addr().String()
		ensureHub(lifecycleConfig(addr))
		if !retrying() {
			_ = blocker.Close()
			t.Fatalf("%s: 应在重试中", method)
		}
		if out, rc := handleRPC(method, []byte("{}")); rc != 0 || !strings.Contains(string(out), `"ok":true`) {
			t.Fatalf("%s 应回 OK: %s", method, out)
		}
		if retrying() {
			t.Fatalf("%s 后重试应已取消", method)
		}
		_ = blocker.Close()
		time.Sleep(60 * time.Millisecond) // 若协程仍在，会在此期间抢到端口
		if activeHub() != nil {
			t.Fatalf("%s 后不应再接管端口", method)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("%s 后端口应空闲: %v", method, err)
		}
		_ = ln.Close()
	}
}

// 重试期间监听地址变更：放弃旧地址（之后不会去占它），立即在新地址启动。
func TestBindRetryListenChange(t *testing.T) {
	fastBindRetry(t)
	t.Cleanup(shutdownHub)
	shutdownHub()
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	oldAddr := blocker.Addr().String()
	ensureHub(lifecycleConfig(oldAddr))
	if !retrying() {
		_ = blocker.Close()
		t.Fatal("应在重试中")
	}
	newAddr := freeAddr(t)
	ensureHub(lifecycleConfig(newAddr))
	h := activeHub()
	if h == nil || retrying() {
		_ = blocker.Close()
		t.Fatal("新地址应立即启动且不再重试")
	}
	_ = blocker.Close()
	time.Sleep(60 * time.Millisecond)
	if activeHub() != h {
		t.Fatal("旧地址的重试不应顶替新中枢")
	}
	ln, err := net.Listen("tcp", oldAddr)
	if err != nil {
		t.Fatalf("旧地址不应被占用: %v", err)
	}
	_ = ln.Close()
	if resp, err := http.Get("http://" + newAddr + "/api/pg/status"); err != nil {
		t.Fatalf("新地址应在服务: %v", err)
	} else {
		resp.Body.Close()
	}
}

func TestIsAddrInUse(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	_, err = net.Listen("tcp", blocker.Addr().String())
	if err == nil || !isAddrInUse(err) {
		t.Fatalf("应识别为端口占用: %v", err)
	}
	if _, err := net.Listen("tcp", "256.0.0.1:1"); err == nil || isAddrInUse(err) {
		t.Fatalf("非占用错误不应识别为占用: %v", err)
	}
}
