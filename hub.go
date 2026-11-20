package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const maxFrameBytes = 32 << 20 // 单帧上限：历史很长时请求体会很大

// ---------------------------------------------------------------- 中枢 ----

type waiter struct {
	conn   *wsConn // 请求投递到的执行器连接；只有它能续写/结束这个请求
	chunks []string
	done   chan struct{}
	err    error
}

// 执行器通道：page = 真实浏览器页面（WS 不带 channel 参数，向后兼容）；
// headless = 云端无头 worker（WS 带 ?channel=headless）。
// 每个通道各占一个执行器槽位，顶替（4001）只发生在同一通道内。
const (
	channelPage     = "page"
	channelHeadless = "headless"
)

var channels = []string{channelPage, channelHeadless}

// parseChannel 把 WS 查询参数映射为通道名；未知值返回 ok=false。
func parseChannel(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", channelPage:
		return channelPage, true
	case channelHeadless:
		return channelHeadless, true
	default:
		return "", false
	}
}

func channelLabel(ch string) string {
	if ch == channelHeadless {
		return "无头执行器"
	}
	return "页面执行器"
}

type hub struct {
	mu      sync.Mutex
	cfg     pluginConfig
	conns   map[string]*wsConn   // 每通道当前已注册的执行器
	ws      map[*wsConn]struct{} // 所有已升级的 WS 连接（含未注册的），关闭中枢时逐一断开
	connAt  map[string]time.Time
	pending map[string]*waiter
	srv     *http.Server
	ln      net.Listener
	closed  bool
}

func newHub(c pluginConfig) *hub {
	return &hub{
		cfg:     c,
		conns:   make(map[string]*wsConn),
		ws:      make(map[*wsConn]struct{}),
		connAt:  make(map[string]time.Time),
		pending: make(map[string]*waiter),
	}
}

// 全局中枢状态。锁顺序：hubMu → hub.mu（反向会死锁）。
// theHub 与 hubRetry 至多一个非 nil：要么已在监听，要么在后台等端口释放。
var (
	hubMu    sync.Mutex
	theHub   *hub
	hubRetry *bindRetry
)

// 端口被占用时的重绑节奏：前 bindRetryFastWindow 内每 bindRetryFast 一次，之后每
// bindRetrySlow 一次；失败日志至多每 bindRetryLogEvery 一条。测试里会调小。
var (
	bindRetryFast       = time.Second
	bindRetryFastWindow = 60 * time.Second
	bindRetrySlow       = 15 * time.Second
	bindRetryLogEvery   = time.Minute
)

// bindRetry 是一次后台重绑：h 是尚未监听的新中枢；stop 只由把它从 hubRetry 摘下的一方
// 关闭（唯一所有者，不会重复 close）；done 在协程退出时关闭。
type bindRetry struct {
	h    *hub
	addr string
	stop chan struct{}
	done chan struct{}
	// 创建时拷贝的节奏参数：协程不读全局变量（被放弃的协程不会被等待，读全局会与测试改参数竞争）
	fast, fastWindow, slow, logEvery time.Duration
}

// ensureHub 按配置启动/更新中枢。只由 plugin.register / plugin.reconfigure 调用——这两者
// 也是 CPA 重新激活已静默实例的方式（热更新失败回滚时会对旧实例再调 plugin.register），
// 所以静默（plugin.quiesce）后只有它们会重新拉起中枢，其它 RPC 不会。
func ensureHub(c pluginConfig) {
	hubMu.Lock()
	defer hubMu.Unlock()
	if r := hubRetry; r != nil {
		r.h.mu.Lock()
		sameAddr := r.addr == c.Listen
		if sameAddr {
			r.h.cfg = c
		}
		r.h.mu.Unlock()
		if sameAddr {
			return
		}
		// 地址变了：放弃旧地址的重试。不在持锁时等协程退出（它装载时要拿 hubMu）；
		// 协程装载前会发现自己已被摘下，自行关掉刚绑上的端口。
		logf("中枢监听地址变更 %s → %s，停止对旧地址的重试", r.addr, c.Listen)
		hubRetry = nil
		close(r.stop)
	}
	if theHub != nil {
		theHub.mu.Lock()
		sameAddr := theHub.cfg.Listen == c.Listen
		if sameAddr {
			theHub.cfg = c
		}
		theHub.mu.Unlock()
		if sameAddr {
			return
		}
		// 监听地址变了：先关旧端口再绑新端口（0.0.0.0 与 127.0.0.1 同端口会冲突）。
		// 旧连接随之断开，页面脚本会自动重连到新地址（经反代时地址对外不变）。
		logf("中枢监听地址变更 %s → %s，重绑中", theHub.cfg.Listen, c.Listen)
		theHub.close()
		theHub = nil
	}
	h := newHub(c)
	if err := h.start(); err != nil {
		if !isAddrInUse(err) {
			logf("中枢启动失败（%s）: %v", c.Listen, err)
			return
		}
		// 典型场景：插件热更新时旧实例（≤1.1.0 不支持 plugin.quiesce）仍占着端口。
		logf("中枢端口 %s 被占用，后台重试绑定（旧实例释放端口后自动接管）", c.Listen)
		r := &bindRetry{
			h: h, addr: c.Listen, stop: make(chan struct{}), done: make(chan struct{}),
			fast: bindRetryFast, fastWindow: bindRetryFastWindow, slow: bindRetrySlow, logEvery: bindRetryLogEvery,
		}
		hubRetry = r
		go r.run()
		return
	}
	theHub = h
	logf("中枢已启动 %s（脚本分发 /api/pg/script.user.js，WS /api/pg/ws）", c.Listen)
}

// isAddrInUse 识别「端口被占用」：Linux/macOS 为 EADDRINUSE，Windows 为 WSAEADDRINUSE(10048)。
func isAddrInUse(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.EADDRINUSE || errno == 10048
}

func (r *bindRetry) run() {
	defer close(r.done)
	defer func() {
		// 本库跑在 CPA 进程里，协程 panic 会带走整个宿主
		if p := recover(); p != nil {
			logf("中枢重绑协程异常（已停止重试）: %v", p)
		}
	}()
	started := time.Now()
	lastLog := started // ensureHub 刚记过一条，下一条至少隔 logEvery
	timer := time.NewTimer(r.fast)
	defer timer.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-timer.C:
		}
		ln, err := net.Listen("tcp", r.addr)
		if err == nil {
			if r.install(ln) {
				logf("中枢已启动 %s（端口释放后重绑成功，等待 %s）", r.addr, time.Since(started).Round(time.Second))
			}
			return
		}
		if now := time.Now(); now.Sub(lastLog) >= r.logEvery {
			lastLog = now
			logf("中枢仍无法绑定 %s（已等待 %s）: %v；若一直如此请重启 CPA", r.addr, now.Sub(started).Round(time.Second), err)
		}
		delay := r.fast
		if time.Since(started) >= r.fastWindow {
			delay = r.slow
		}
		timer.Reset(delay)
	}
}

// install 在仍是当前重试时把监听交给中枢并设为 theHub；已被取消则关掉端口。
func (r *bindRetry) install(ln net.Listener) bool {
	hubMu.Lock()
	defer hubMu.Unlock()
	if hubRetry != r {
		_ = ln.Close()
		return false
	}
	hubRetry = nil
	r.h.serve(ln)
	theHub = r.h
	return true
}

func activeHub() *hub {
	hubMu.Lock()
	defer hubMu.Unlock()
	return theHub
}

// shutdownHub 用于 C ABI shutdown 与 plugin.shutdown。
func shutdownHub() { stopHub("plugin shutdown") }

// stopHub 释放监听端口、以 1001 断开全部 WS（脚本会自动重连到接管端口的新实例）、
// 立即失败所有进行中的请求，并取消后台重绑。返回时端口已释放、重绑协程已退出。
// 用于 plugin.quiesce（热更新前 CPA 对旧实例调用）与 shutdown。
func stopHub(reason string) {
	hubMu.Lock()
	h := theHub
	theHub = nil
	r := hubRetry
	hubRetry = nil
	hubMu.Unlock()
	if r != nil {
		close(r.stop)
		<-r.done // 已释放 hubMu，协程的 install 不会被卡住
	}
	if h != nil {
		h.closeWith(reason)
	}
}

func (h *hub) config() pluginConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

func (h *hub) start() error {
	ln, err := net.Listen("tcp", h.config().Listen)
	if err != nil {
		return err
	}
	h.serve(ln)
	return nil
}

// serve 在已绑定的监听上启动 HTTP 服务。
func (h *hub) serve(ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pg/ws", h.serveWS)
	mux.HandleFunc("/api/pg/script.user.js", h.serveScript)
	mux.HandleFunc("/api/pg/status", h.serveStatus)
	mux.HandleFunc("/api/pg/exec", h.serveExec)
	mux.HandleFunc("/api/pggo/push", h.servePush)
	mux.HandleFunc("/api/robin/bind-by-key", h.serveRobin)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	h.mu.Lock()
	h.ln = ln
	h.srv = srv
	h.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("中枢 HTTP 退出: %v", err)
		}
	}()
}

func (h *hub) close() { h.closeWith("hub restarting") }

// closeWith 关闭中枢（可重复调用）。
func (h *hub) closeWith(reason string) {
	h.mu.Lock()
	h.closed = true
	srv, ln := h.srv, h.ln
	// 已被劫持的 WS 连接不归 http.Server 管，需手动断开（含尚未注册的）；1001 不在脚本的
	// 「不重连」名单里，页面会自动重连。
	conns := make([]*wsConn, 0, len(h.ws))
	for conn := range h.ws {
		conns = append(conns, conn)
	}
	for ch := range h.conns {
		delete(h.conns, ch)
	}
	rids := make([]string, 0, len(h.pending))
	for rid := range h.pending {
		rids = append(rids, rid)
	}
	h.mu.Unlock()
	if srv != nil {
		_ = srv.Close() // 同时关闭监听
	}
	if ln != nil {
		_ = ln.Close()
	}
	// 先结束请求再断连接：调用方拿到确定的 hub_restarting，而不是断线竞态下的 executor_disconnected
	for _, rid := range rids {
		h.finish(nil, rid, rpcErrf(http.StatusServiceUnavailable, "hub_restarting", "执行器中枢重启/停止，请重试"))
	}
	for _, conn := range conns {
		_ = conn.writeClose(1001, reason)
		conn.shutdown()
	}
}

func (h *hub) keyValid(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	for _, k := range h.config().APIKeys {
		if strings.TrimSpace(k) == key {
			return true
		}
	}
	return false
}

// dispatch 把一次上游请求投给指定通道的在线执行器，等它把上游原文拼完。
// 目标通道离线时直接 503，不静默回落到其它通道。
func (h *hub) dispatch(channel, model string, wire []any, binding string) (string, error) {
	if channel == "" {
		return "", rpcErrf(http.StatusNotFound, "model_not_routable",
			"模型 %s 不属于任何已启用的执行器通道（对应前缀为空即停用）", model)
	}
	h.mu.Lock()
	conn := h.conns[channel]
	if conn == nil || conn.isClosed() {
		h.mu.Unlock()
		if channel == channelHeadless {
			return "", rpcErrf(http.StatusServiceUnavailable, "executor_offline",
				"通道 headless 没有在线执行器：请检查无头 worker 是否运行并连上 WS（?channel=headless）")
		}
		return "", rpcErrf(http.StatusServiceUnavailable, "executor_offline",
			"通道 page 没有在线页面执行器：请用浏览器打开 AI Studio 页面并让脚本连上 WS")
	}
	rid := newID()
	w := &waiter{conn: conn, done: make(chan struct{})}
	h.pending[rid] = w
	timeout := time.Duration(h.cfg.RequestTimeoutSec) * time.Second
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, rid)
		h.mu.Unlock()
	}()

	requestBody, err := json.Marshal(map[string]any{"wire": wire, "binding": binding})
	if err != nil {
		return "", rpcErrf(http.StatusInternalServerError, "encode_failed", "wire 序列化失败: %v", err)
	}
	msg, _ := json.Marshal(map[string]any{
		"type":        "request",
		"requestId":   rid,
		"model":       model,
		"stream":      true,
		"requestBody": string(requestBody),
	})
	if err := conn.writeText(msg); err != nil {
		return "", rpcErrf(http.StatusBadGateway, "executor_send_failed", "推送到%s失败: %v", channelLabel(channel), err)
	}

	select {
	case <-w.done:
		if w.err != nil {
			return "", w.err
		}
		return strings.Join(w.chunks, ""), nil
	case <-time.After(timeout):
		_ = conn.writeText(mustJSON(map[string]any{"type": "cancel", "requestId": rid}))
		return "", rpcErrf(http.StatusGatewayTimeout, "executor_timeout",
			"%s %d 秒内没有回结果（页面可能被后台冻结）", channelLabel(channel), int(timeout.Seconds()))
	}
}

// finish 结束一个请求。先从 pending 摘除再关 done：页面重复发送 stream-end/error
// 或结束后迟到的分块都会变成空操作，避免重复 close 触发 panic 拖垮宿主进程。
// from 非 nil 时只允许请求的所属连接结束它（防止别的通道/连接按 rid 抢结束）；
// nil 表示中枢自身（关闭/断线清理）。
func (h *hub) finish(from *wsConn, rid string, err error) {
	h.mu.Lock()
	w := h.pending[rid]
	if w == nil || (from != nil && w.conn != from) {
		h.mu.Unlock()
		return
	}
	delete(h.pending, rid)
	h.mu.Unlock()
	w.err = err
	close(w.done)
}

// appendChunk 只接受所属连接的分块。分块在持锁时追加：同一 rid 只有所属连接的
// 读循环会写，持锁也让与 finish 的先后关系明确。
func (h *hub) appendChunk(from *wsConn, rid, chunk string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if w := h.pending[rid]; w != nil && w.conn == from {
		w.chunks = append(w.chunks, chunk)
	}
}

// failConn 在执行器连接断开时立即结束它名下所有未完成请求（不再干等 request_timeout_sec）。
func (h *hub) failConn(conn *wsConn, channel string) {
	h.mu.Lock()
	var rids []string
	for rid, w := range h.pending {
		if w.conn == conn {
			rids = append(rids, rid)
		}
	}
	h.mu.Unlock()
	for _, rid := range rids {
		h.finish(nil, rid, rpcErrf(http.StatusBadGateway, "executor_disconnected",
			"%s（通道 %s）在请求完成前断开", channelLabel(channel), channel))
	}
}

// ------------------------------------------------------------ HTTP 端点 ----

func (h *hub) serveScript(w http.ResponseWriter, r *http.Request) {
	path := h.config().ScriptPath
	if path == "" {
		http.Error(w, "script_path 未配置", http.StatusNotFound)
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "读取脚本失败: "+err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

// serveStatus 报告各通道在线状态。顶层 online 表示「任一通道在线」（兼容旧版）；
// connected_at 取各通道最近一次注册时间，从未连接过则省略该字段。
func (h *hub) serveStatus(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	anyOnline := false
	var latest time.Time
	chans := make(map[string]any, len(channels))
	for _, ch := range channels {
		conn := h.conns[ch]
		online := conn != nil && !conn.isClosed()
		anyOnline = anyOnline || online
		entry := map[string]any{"online": online}
		if at := h.connAt[ch]; !at.IsZero() {
			entry["connected_at"] = at.Unix()
			if at.After(latest) {
				latest = at
			}
		}
		chans[ch] = entry
	}
	pending := len(h.pending)
	h.mu.Unlock()
	out := map[string]any{
		"version":  pluginVersion,
		"online":   anyOnline,
		"pending":  pending,
		"models":   h.config().Models,
		"channels": chans,
	}
	if !latest.IsZero() {
		out["connected_at"] = latest.Unix()
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *hub) serveExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !h.keyValid(apiKeyOf(r)) {
		http.Error(w, `{"error":{"message":"API Key 无效"}}`, http.StatusUnauthorized)
		return
	}
	var body struct {
		Model       string `json:"model"`
		Wire        []any  `json:"wire"`
		Binding     string `json:"binding"`
		RequestJSON string `json:"request_json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":{"message":"body 不是 JSON"}}`, http.StatusBadRequest)
		return
	}
	cfg := currentConfig()
	channel, upstream := routeModel(cfg, body.Model)
	if body.RequestJSON != "" {
		var chat chatRequest
		if err := json.Unmarshal([]byte(body.RequestJSON), &chat); err != nil {
			http.Error(w, `{"error":{"message":"request_json 不是 OpenAI chat JSON"}}`, http.StatusBadRequest)
			return
		}
		wire, binding := buildWire(upstream, chat, cfg)
		body.Wire, body.Binding = wire, binding
	}
	if len(body.Wire) == 0 {
		http.Error(w, `{"error":{"message":"缺 wire / request_json"}}`, http.StatusBadRequest)
		return
	}
	raw, err := h.dispatch(channel, body.Model, body.Wire, body.Binding)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	result, err := decodeUpstream(raw)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": result.Text, "thoughts": result.Thoughts, "usage": result.Usage})
}

func (h *hub) servePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !h.keyValid(apiKeyOf(r)) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "API Key 无效"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "读取 body 失败"})
		return
	}
	var body struct {
		Email    string `json:"email"`
		AuthUser any    `json:"authuser"`
		Cookies  []any  `json:"cookies"`
	}
	_ = json.Unmarshal(raw, &body)
	label := body.Email
	if label == "" {
		label = "unknown"
	}
	dir := filepath.Join(h.config().DataDir, "cookies")
	name := sanitize(label) + ".json"
	if err := writeCookieFile(dir, name, raw); err != nil {
		logf("Cookie 推送落盘失败: %s: %v", name, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": "落盘失败"})
		return
	}
	logf("Cookie 推送已存档: %s（%d 条）", name, len(body.Cookies))
	writeJSON(w, http.StatusOK, map[string]any{
		"label":      label,
		"created_at": time.Now().Unix(),
		"cookies":    len(body.Cookies),
	})
}

// writeCookieFile 原子写入 Cookie 推送文件：目录 0750、文件 0640（同组可读，
// 供以 gid 0 运行的无头 worker 只读挂载读取），先写临时文件再 rename，
// worker 不会读到写了一半的文件。已存在的目录/文件也收紧到同样权限。
func writeCookieFile(dir, name string, raw []byte) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o750)
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后是空操作
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, name))
}

func (h *hub) serveRobin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !h.keyValid(apiKeyOf(r)) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "API Key 无效"})
		return
	}
	var body struct {
		Email      string `json:"email"`
		OAuthToken string `json:"oauth_token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "body 不是 JSON"})
		return
	}
	if body.Email == "" || body.OAuthToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "缺少 email / oauth_token"})
		return
	}
	dir := h.config().DataDir
	_ = os.MkdirAll(dir, 0o700)
	f, err := os.OpenFile(filepath.Join(dir, "robin.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		line, _ := json.Marshal(map[string]any{"email": body.Email, "oauth_token": body.OAuthToken, "at": time.Now().Unix()})
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": "已保存（相同邮箱重复推送自动覆盖）"})
}

func apiKeyOf(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '@' || r == '.' || r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func newID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

// ------------------------------------------------------------- WS 端点 ----

func (h *hub) serveWS(w http.ResponseWriter, r *http.Request) {
	channel, ok := parseChannel(r.URL.Query().Get("channel"))
	if !ok {
		http.Error(w, "unknown channel", http.StatusBadRequest)
		return
	}
	conn, err := upgradeWebSocket(w, r)
	if err != nil {
		logf("WS 升级失败: %v", err)
		return
	}
	defer conn.shutdown()
	// 登记连接；与 closeWith 竞态、中枢已关闭时直接 1001 断开
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.writeClose(1001, "hub closed")
		return
	}
	h.ws[conn] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.ws, conn)
		h.mu.Unlock()
	}()

	var authed bool
	for {
		opcode, payload, err := conn.readMessage()
		if err != nil {
			break
		}
		if opcode != 0x1 {
			continue
		}
		var msg struct {
			Type        string `json:"type"`
			APIKey      string `json:"apiKey"`
			RequestID   string `json:"requestId"`
			ChunkBody   string `json:"chunkBody"`
			RawResponse string `json:"rawResponse"`
			Status      int    `json:"status"`
			Error       *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "register":
			if !h.keyValid(msg.APIKey) {
				_ = conn.writeClose(4008, "invalid api key")
				return
			}
			h.mu.Lock()
			prev := h.conns[channel]
			h.conns[channel] = conn
			h.connAt[channel] = time.Now()
			h.mu.Unlock()
			if prev != nil && prev != conn {
				_ = prev.writeClose(4001, "replaced by new connection")
				prev.shutdown()
			}
			authed = true
			_ = conn.writeText(mustJSON(map[string]any{"type": "registered", "executorType": "playground"}))
			logf("%s上线（通道 %s）", channelLabel(channel), channel)
		case "ping":
			_ = conn.writeText(mustJSON(map[string]any{"type": "pong"}))
		case "stream-chunk":
			if authed {
				h.appendChunk(conn, msg.RequestID, msg.ChunkBody)
			}
		case "stream-end":
			if authed {
				h.finish(conn, msg.RequestID, nil)
			}
		case "response":
			if authed {
				h.appendChunk(conn, msg.RequestID, msg.RawResponse)
				h.finish(conn, msg.RequestID, nil)
			}
		case "error":
			if authed {
				status := msg.Status
				if status == 0 {
					status = http.StatusBadGateway
				}
				code := "executor_error"
				text := "页面执行器返回错误"
				if msg.Error != nil {
					if msg.Error.Code != "" {
						code = msg.Error.Code
					}
					if msg.Error.Message != "" {
						text = msg.Error.Message
					}
				}
				h.finish(conn, msg.RequestID, rpcErrf(status, code, "%s", text))
			}
		}
	}

	h.mu.Lock()
	if h.conns[channel] == conn {
		delete(h.conns, channel)
	}
	h.mu.Unlock()
	h.failConn(conn, channel)
	if authed {
		logf("%s下线（通道 %s）", channelLabel(channel), channel)
	}
}

// --------------------------------------------------- RFC6455 最小实现 ----

type wsConn struct {
	c      net.Conn
	br     *bufio.Reader
	wmu    sync.Mutex
	closed atomic.Bool
}

var errNotWebSocket = errors.New("不是 WebSocket 升级请求")

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errNotWebSocket
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errNotWebSocket
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("连接不可劫持")
	}
	conn, buf, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := buf.WriteString(resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := buf.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &wsConn{c: conn, br: buf.Reader}, nil
}

func (w *wsConn) isClosed() bool { return w.closed.Load() }

func (w *wsConn) shutdown() {
	if w.closed.CompareAndSwap(false, true) {
		_ = w.c.Close()
	}
}

func (w *wsConn) readMessage() (byte, []byte, error) {
	var opcode byte
	var message []byte
	for {
		head := make([]byte, 2)
		if _, err := io.ReadFull(w.br, head); err != nil {
			return 0, nil, err
		}
		fin := head[0]&0x80 != 0
		op := head[0] & 0x0f
		masked := head[1]&0x80 != 0
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
		if length < 0 || length > maxFrameBytes {
			return 0, nil, errors.New("WS 帧过大")
		}
		var mask []byte
		if masked {
			mask = make([]byte, 4)
			if _, err := io.ReadFull(w.br, mask); err != nil {
				return 0, nil, err
			}
		}
		payload := make([]byte, length)
		if length > 0 {
			if _, err := io.ReadFull(w.br, payload); err != nil {
				return 0, nil, err
			}
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x8: // close
			return 0x8, nil, io.EOF
		case 0x9: // ping
			_ = w.writeFrame(0xA, payload)
			continue
		case 0xA: // pong
			continue
		case 0x0, 0x1, 0x2:
			if op != 0x0 {
				opcode = op
			}
			message = append(message, payload...)
			if fin {
				return opcode, message, nil
			}
			continue
		default:
			continue
		}
	}
}

func (w *wsConn) writeText(payload []byte) error { return w.writeFrame(0x1, payload) }

func (w *wsConn) writeClose(code uint16, reason string) error {
	payload := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	payload = append(payload, []byte(reason)...)
	return w.writeFrame(0x8, payload)
}

func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.closed.Load() {
		return io.ErrClosedPipe
	}
	header := make([]byte, 0, 10)
	header = append(header, 0x80|opcode)
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	if _, err := w.c.Write(header); err != nil {
		return err
	}
	if n > 0 {
		if _, err := w.c.Write(payload); err != nil {
			return err
		}
	}
	return nil
}
