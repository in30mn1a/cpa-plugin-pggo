package main

import (
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetLifecycle 让用例从「库刚被 dlopen、尚未 init」的状态开始，结束时恢复，避免影响其它用例。
func resetLifecycle(t *testing.T) {
	t.Helper()
	shutdownHub()
	hubState.Store(hubIdle)
	pluginInits.Store(0)
	t.Cleanup(func() {
		shutdownHub()
		hubState.Store(hubIdle)
		pluginInits.Store(0)
	})
}

// registerWith 模拟宿主的 plugin.register（只下发 plugins.configs.pggo 一节）。
func registerWith(t *testing.T, method, addr string) {
	t.Helper()
	section := "listen: " + addr + "\napi-keys:\n  - cat-test\n"
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte(section), "schema_version": 10})
	if out, rc := handleRPC(method, req); rc != 0 {
		t.Fatalf("%s 失败: %s", method, out)
	}
}

func listening(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func expectListening(t *testing.T, addr, what string) {
	t.Helper()
	if activeHub() == nil || !listening(addr) {
		t.Fatalf("%s：中枢应在 %s 监听", what, addr)
	}
}

func expectStopped(t *testing.T, addr, what string) {
	t.Helper()
	if activeHub() != nil || retrying() {
		t.Fatalf("%s：中枢不应运行", what)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("%s：端口 %s 应空闲: %v", what, addr, err)
	}
	_ = ln.Close()
}

var routeOtherModel = []byte(`{"RequestedModel":"gpt-other"}`)

// 停用（C shutdown）→ 再启用时宿主重新调 cliproxy_plugin_init：中枢按上次配置回来，
// 即使之后没有 plugin.register。
func TestReinitAfterShutdownRestartsHub(t *testing.T) {
	resetLifecycle(t)
	addr := freeAddr(t)
	onPluginInit() // 首次加载
	if activeHub() != nil {
		t.Fatal("首次 init 时还没有配置，不应启动中枢")
	}
	registerWith(t, "plugin.register", addr)
	expectListening(t, addr, "plugin.register 后")

	pluginShutdown() // 停用：C 层 shutdown
	expectStopped(t, addr, "shutdown 后")

	onPluginInit() // 再启用：同一句柄再次 init
	expectListening(t, addr, "再次 init 后")
	if hubState.Load() != hubActive {
		t.Fatalf("再次 init 后状态应为 active，实为 %d", hubState.Load())
	}
	// 若宿主随后照常 plugin.register，同地址只更新配置
	before := activeHub()
	registerWith(t, "plugin.register", addr)
	if activeHub() != before {
		t.Fatal("同地址的 plugin.register 不应重建中枢")
	}
}

// 停用 → 再启用时宿主什么都不调：第一条业务 RPC（model.route 每个请求都会走）拉起中枢。
func TestRPCAfterShutdownRestartsHub(t *testing.T) {
	for _, tc := range []struct {
		method string
		req    []byte
	}{
		{"model.route", routeOtherModel},
		{"model.register", nil},
		{"executor.identifier", nil},
		{"executor.count_tokens", []byte(`{}`)},
	} {
		t.Run(tc.method, func(t *testing.T) {
			resetLifecycle(t)
			addr := freeAddr(t)
			onPluginInit()
			registerWith(t, "plugin.register", addr)
			pluginShutdown()
			expectStopped(t, addr, "shutdown 后")

			_, _ = handleRPC(tc.method, tc.req)
			expectListening(t, addr, tc.method+" 后")
		})
	}
}

// plugin.shutdown RPC 与 C shutdown 同义；生命周期 RPC 本身不会触发复活。
func TestPluginShutdownRPCIsRevivable(t *testing.T) {
	resetLifecycle(t)
	addr := freeAddr(t)
	registerWith(t, "plugin.register", addr)
	if out, rc := handleRPC("plugin.shutdown", []byte("{}")); rc != 0 || !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("plugin.shutdown 应回 OK: %s", out)
	}
	_, _ = handleRPC("plugin.shutdown", []byte("{}"))
	expectStopped(t, addr, "重复 plugin.shutdown 后")
	_, _ = handleRPC("model.route", routeOtherModel)
	expectListening(t, addr, "model.route 后")
}

// 静默（热更新中被新实例替换）后：业务 RPC 与随后的 shutdown 都不能让中枢抢回端口；
// 只有 plugin.reconfigure（宿主回滚）能重新激活。
func TestQuiescedHubNotRevivedByRPC(t *testing.T) {
	resetLifecycle(t)
	addr := freeAddr(t)
	onPluginInit()
	registerWith(t, "plugin.register", addr)
	if _, rc := handleRPC("plugin.quiesce", []byte("{}")); rc != 0 {
		t.Fatal("plugin.quiesce 失败")
	}
	expectStopped(t, addr, "quiesce 后")

	for _, m := range []string{"model.route", "model.register", "executor.identifier"} {
		_, _ = handleRPC(m, routeOtherModel)
		expectStopped(t, addr, "quiesce 后 "+m)
	}
	// 热更新完成后再停用插件：宿主也会对被替换的旧实例调 shutdown，旧实例必须保持静默
	pluginShutdown()
	_, _ = handleRPC("model.route", routeOtherModel)
	expectStopped(t, addr, "quiesce+shutdown 后 model.route")
	if hubState.Load() != hubQuiesced {
		t.Fatalf("静默实例被 shutdown 后应保持 quiesced，实为 %d", hubState.Load())
	}

	registerWith(t, "plugin.reconfigure", addr) // 宿主回滚
	expectListening(t, addr, "plugin.reconfigure 后")
}

// 尚未收到任何配置时 shutdown（如 init 后 plugin.register 失败、宿主随即卸下）不会让之后的 RPC 凭默认配置启动中枢。
func TestShutdownBeforeRegisterDoesNotStartHub(t *testing.T) {
	resetLifecycle(t)
	onPluginInit()
	pluginShutdown()
	_, _ = handleRPC("model.route", routeOtherModel)
	if activeHub() != nil || retrying() || hubState.Load() != hubIdle {
		t.Fatal("未注册过的实例不应被 RPC 拉起中枢")
	}
	onPluginInit()
	if activeHub() != nil || retrying() {
		t.Fatal("未注册过的实例再次 init 也不应启动中枢")
	}
}

// 并发：大量 model.route 同时触发复活只建一个中枢；与 shutdown/init 交错时无数据竞争
// （go test -race）。
func TestConcurrentReviveSingleHub(t *testing.T) {
	resetLifecycle(t)
	addr := freeAddr(t)
	onPluginInit()
	registerWith(t, "plugin.register", addr)
	pluginShutdown()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 20; j++ {
				_, _ = handleRPC("model.route", routeOtherModel)
			}
		}()
	}
	close(start)
	wg.Wait()
	expectListening(t, addr, "并发复活后")

	// 与 shutdown / 再次 init 交错
	stop := make(chan struct{})
	var bg sync.WaitGroup
	for i := 0; i < 8; i++ {
		bg.Add(1)
		go func() {
			defer bg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = handleRPC("model.route", routeOtherModel)
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		pluginShutdown()
		if i%2 == 0 {
			onPluginInit()
		}
	}
	close(stop)
	bg.Wait()

	// 收尾：停用后下一条 RPC 必定把中枢拉回来
	pluginShutdown()
	expectStopped(t, addr, "收尾 shutdown 后")
	_, _ = handleRPC("model.route", routeOtherModel)
	expectListening(t, addr, "收尾 model.route 后")
}

// 降级/回滚：被静默替换的旧实例被宿主重新加载（同一句柄再次 init）时，按上次配置拉起中枢。
func TestReinitRevivesQuiescedInstance(t *testing.T) {
	resetLifecycle(t)
	addr := freeAddr(t)
	onPluginInit()
	registerWith(t, "plugin.register", addr)
	if _, rc := handleRPC("plugin.quiesce", []byte("{}")); rc != 0 {
		t.Fatal("plugin.quiesce 失败")
	}
	pluginShutdown() // 之后被宿主卸下：仍保持静默
	expectStopped(t, addr, "quiesce+shutdown 后")

	onPluginInit()
	expectListening(t, addr, "重新加载（再次 init）后")
	if hubState.Load() != hubActive {
		t.Fatalf("再次 init 后状态应为 active，实为 %d", hubState.Load())
	}
}

// 从未收到配置的实例被静默后再 init：不得凭默认配置（:8788）启动中枢。
func TestIdleQuiesceThenInitNoHub(t *testing.T) {
	resetLifecycle(t)
	onPluginInit()
	if _, rc := handleRPC("plugin.quiesce", []byte("{}")); rc != 0 {
		t.Fatal("plugin.quiesce 失败")
	}
	if hubState.Load() != hubIdle {
		t.Fatalf("未配置实例被静默后应保持 idle，实为 %d", hubState.Load())
	}
	onPluginInit()
	_, _ = handleRPC("model.route", routeOtherModel)
	if activeHub() != nil || retrying() {
		t.Fatal("未配置过的实例不应启动中枢")
	}
}

// C ABI 入口的 panic 被拦截，不会冒到宿主。
func TestRecoverCSwallowsPanic(t *testing.T) {
	ran := false
	recoverC("test", func() { ran = true; panic("boom") })
	if !ran {
		t.Fatal("fn 未执行")
	}
}
