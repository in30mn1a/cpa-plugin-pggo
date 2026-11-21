package main

import (
	"sync"
	"sync/atomic"
)

// 中枢生命周期意图（与中枢是否真的在监听无关，后者由 theHub/hubRetry 表示）。
//
// 为什么要区分「停用」与「静默」：CPA 停用插件（面板关掉 / 配置 enabled: false）时只调 C 层
// shutdown（internal/pluginhost/host.go detachPlugin → loader_unix.go close → api.shutdown），
// c-shared 库不会卸载、Go 全局状态原样保留。再次启用时 CPA 对同一份影子副本（路径由内容摘要
// 决定）重新 dlopen（拿到同一个句柄）并再调一次 cliproxy_plugin_init，按源码随后应发
// plugin.register；但线上观察到重新启用后一条 RPC 都没收到、中枢一直没回来。所以这里不依赖
// plugin.register：再次 init、或停用后的第一条业务 RPC（model.route 每个 CPA 请求都会调），都会
// 用最后一次下发的配置重新拉起中枢。
//
// 静默（plugin.quiesce）则不同：它表示新版本正在接管端口，旧实例绝不能凭一条迟到的 RPC 抢回端口；
// 只有 plugin.register/reconfigure（CPA 回滚时）或再次 init（CPA 重新加载这份库）能重新激活它。
const (
	hubIdle     int32 = iota // 尚未收到配置：没有可复活的中枢
	hubActive                // 应在运行（plugin.register/reconfigure 或复活之后）
	hubShutdown              // 被 C shutdown / plugin.shutdown 停掉：插件被停用，库仍驻留，可能再被启用
	hubQuiesced              // 被 plugin.quiesce 静默：正被新实例替换，不得自行复活
)

var (
	// lifecycleMu 串行化所有生命周期迁移。锁顺序：lifecycleMu → hubMu → hub.mu。
	lifecycleMu sync.Mutex
	// hubState 只在持 lifecycleMu 时写；无锁读用于 RPC 热路径。
	hubState atomic.Int32
	// pluginInits 统计 cliproxy_plugin_init 调用次数：第二次起意味着宿主重新加载了驻留的库。
	pluginInits atomic.Int32
)

// activateHub 由 plugin.register / plugin.reconfigure 调用：无论之前处于何种状态都重新激活。
func activateHub(c pluginConfig) {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	hubState.Store(hubActive)
	ensureHub(c)
}

// quiesceHub 由 plugin.quiesce 调用：同步交出端口，并禁止业务 RPC 把中枢复活。
// 从未收到配置的实例保持 idle：否则之后的再次 init 会凭默认配置启动中枢。
func quiesceHub() {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if hubState.Load() != hubIdle {
		hubState.Store(hubQuiesced)
	}
	stopHub("plugin quiescing")
}

// pluginShutdown 由 C ABI shutdown 与 plugin.shutdown 调用。只有运行中的实例才标记为
// 「停用」（可复活）；已静默的实例保持静默——热更新后再停用时 CPA 也会对被替换的旧实例调 shutdown。
func pluginShutdown() {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if hubState.Load() == hubActive {
		hubState.Store(hubShutdown)
		logf("插件被宿主停用（shutdown）：中枢停止；再次启用后的首个调用会按上次配置重新拉起")
	}
	stopHub("plugin shutdown")
}

// onPluginInit 由 cliproxy_plugin_init 调用。首次 init 时还没有配置，中枢由随后的 plugin.register
// 拉起；再次 init 说明宿主在重新启用这份驻留的库，用最后一次的配置重新拉起中枢（若随后仍有
// plugin.register，同地址时 ensureHub 只更新配置）。
func onPluginInit() {
	n := pluginInits.Add(1)
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	state := hubState.Load()
	logf("cliproxy_plugin_init 第 %d 次（生命周期状态 %s）", n, hubStateName(state))
	if n == 1 {
		return
	}
	switch state {
	case hubShutdown, hubQuiesced:
		hubState.Store(hubActive)
		logf("插件被宿主重新初始化：按上次配置重新拉起中枢")
		ensureHub(currentConfig())
	}
}

func hubStateName(s int32) string {
	switch s {
	case hubIdle:
		return "idle"
	case hubActive:
		return "active"
	case hubShutdown:
		return "shutdown"
	case hubQuiesced:
		return "quiesced"
	}
	return "unknown"
}

// recoverC 用于没有 RPC 信封兜底的 C ABI 入口（init / shutdown）：本库跑在 CPA 进程内，
// panic 会带走宿主，这里拦截并记录。
func recoverC(name string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			logf("%s panic（已拦截）: %v", name, p)
		}
	}()
	fn()
}

// reviveIfShutdown 在业务 RPC 入口调用：常态下只有一次原子读，不加锁、不分配。
func reviveIfShutdown() {
	if hubState.Load() == hubShutdown {
		reviveHub()
	}
}

// reviveHub 把被停用的中枢按上次配置重新拉起；并发调用只有一个真正执行。
// 浏览器页面脚本与无头 worker 会自动重连（worker 会话结束后按退避重启浏览器），无需人工干预。
func reviveHub() {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if hubState.Load() != hubShutdown {
		return
	}
	hubState.Store(hubActive)
	logf("插件停用后再次收到调用（宿主未重新注册）：按上次配置重新拉起中枢")
	ensureHub(currentConfig())
}
