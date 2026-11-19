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
	"time"
)

const maxFrameBytes = 32 << 20 // 单帧上限：历史很长时请求体会很大

// ---------------------------------------------------------------- 中枢 ----

type waiter struct {
	chunks []string
	done   chan struct{}
	err    error
}

type hub struct {
	mu      sync.Mutex
	cfg     pluginConfig
	conn    *wsConn
	connAt  time.Time
	pending map[string]*waiter
	srv     *http.Server
	ln      net.Listener
}

var (
	hubMu  sync.Mutex
	theHub *hub
)

func ensureHub(c pluginConfig) {
	hubMu.Lock()
	defer hubMu.Unlock()
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
	h := &hub{cfg: c, pending: make(map[string]*waiter)}
	if err := h.start(); err != nil {
		logf("中枢启动失败（%s）: %v", c.Listen, err)
		return
	}
	theHub = h
	logf("中枢已启动 %s（脚本分发 /api/pg/script.user.js，WS /api/pg/ws）", c.Listen)
}

func activeHub() *hub {
	hubMu.Lock()
	defer hubMu.Unlock()
	return theHub
}

func shutdownHub() {
	hubMu.Lock()
	h := theHub
	theHub = nil
	hubMu.Unlock()
	if h != nil {
		h.close()
	}
}

func (h *hub) config() pluginConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

func (h *hub) start() error {
	ln, err := net.Listen("tcp", h.cfg.Listen)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pg/ws", h.serveWS)
	mux.HandleFunc("/api/pg/script.user.js", h.serveScript)
	mux.HandleFunc("/api/pg/status", h.serveStatus)
	mux.HandleFunc("/api/pg/exec", h.serveExec)
	mux.HandleFunc("/api/pggo/push", h.servePush)
	mux.HandleFunc("/api/robin/bind-by-key", h.serveRobin)
	h.ln = ln
	h.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	go func() {
		if err := h.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("中枢 HTTP 退出: %v", err)
		}
	}()
	return nil
}

func (h *hub) close() {
	if h.srv != nil {
		_ = h.srv.Close()
	}
	if h.ln != nil {
		_ = h.ln.Close()
	}
	// 已被劫持的 WS 连接不归 http.Server 管，需手动断开；1001 不在脚本的
	// 「不重连」名单里，页面会自动重连。
	h.mu.Lock()
	conn := h.conn
	h.conn = nil
	rids := make([]string, 0, len(h.pending))
	for rid := range h.pending {
		rids = append(rids, rid)
	}
	h.mu.Unlock()
	if conn != nil {
		_ = conn.writeClose(1001, "hub restarting")
		conn.shutdown()
	}
	for _, rid := range rids {
		h.finish(rid, rpcErrf(http.StatusServiceUnavailable, "hub_restarting", "执行器中枢重启，请重试"))
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

// dispatch 把一次上游请求投给在线页面执行器，等它把上游原文拼完。
func (h *hub) dispatch(model string, wire []any, binding string) (string, error) {
	h.mu.Lock()
	conn := h.conn
	if conn == nil || conn.isClosed() {
		h.mu.Unlock()
		return "", rpcErrf(http.StatusServiceUnavailable, "executor_offline",
			"没有在线页面执行器：请用浏览器打开 AI Studio 页面并让脚本连上 WS")
	}
	rid := newID()
	w := &waiter{done: make(chan struct{})}
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
		return "", rpcErrf(http.StatusBadGateway, "executor_send_failed", "推送到页面失败: %v", err)
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
			"页面执行器 %d 秒内没有回结果（页面可能被后台冻结）", int(timeout.Seconds()))
	}
}

// finish 结束一个请求。先从 pending 摘除再关 done：页面重复发送 stream-end/error
// 或结束后迟到的分块都会变成空操作，避免重复 close 触发 panic 拖垮宿主进程。
func (h *hub) finish(rid string, err error) {
	h.mu.Lock()
	w := h.pending[rid]
	delete(h.pending, rid)
	h.mu.Unlock()
	if w == nil {
		return
	}
	w.err = err
	close(w.done)
}

func (h *hub) appendChunk(rid, chunk string) {
	h.mu.Lock()
	w := h.pending[rid]
	h.mu.Unlock()
	if w != nil {
		w.chunks = append(w.chunks, chunk)
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

func (h *hub) serveStatus(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	online := h.conn != nil && !h.conn.isClosed()
	connAt := h.connAt
	pending := len(h.pending)
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"online":       online,
		"connected_at": connAt.Unix(),
		"pending":      pending,
		"models":       h.config().Models,
	})
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
	if body.RequestJSON != "" {
		cfg := currentConfig()
		var chat chatRequest
		if err := json.Unmarshal([]byte(body.RequestJSON), &chat); err != nil {
			http.Error(w, `{"error":{"message":"request_json 不是 OpenAI chat JSON"}}`, http.StatusBadRequest)
			return
		}
		wire, binding := buildWire(resolveUpstreamModel(cfg, body.Model), chat, cfg)
		body.Wire, body.Binding = wire, binding
	}
	if len(body.Wire) == 0 {
		http.Error(w, `{"error":{"message":"缺 wire / request_json"}}`, http.StatusBadRequest)
		return
	}
	raw, err := h.dispatch(body.Model, body.Wire, body.Binding)
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
	_ = os.MkdirAll(dir, 0o700)
	name := sanitize(label) + ".json"
	_ = os.WriteFile(filepath.Join(dir, name), raw, 0o600)
	logf("Cookie 推送已存档: %s（%d 条）", name, len(body.Cookies))
	writeJSON(w, http.StatusOK, map[string]any{
		"label":      label,
		"created_at": time.Now().Unix(),
		"cookies":    len(body.Cookies),
	})
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
	conn, err := upgradeWebSocket(w, r)
	if err != nil {
		logf("WS 升级失败: %v", err)
		return
	}
	defer conn.shutdown()

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
			prev := h.conn
			h.conn = conn
			h.connAt = time.Now()
			h.mu.Unlock()
			if prev != nil && prev != conn {
				_ = prev.writeClose(4001, "replaced by new connection")
				prev.shutdown()
			}
			authed = true
			_ = conn.writeText(mustJSON(map[string]any{"type": "registered", "executorType": "playground"}))
			logf("页面执行器上线")
		case "ping":
			_ = conn.writeText(mustJSON(map[string]any{"type": "pong"}))
		case "stream-chunk":
			if authed {
				h.appendChunk(msg.RequestID, msg.ChunkBody)
			}
		case "stream-end":
			if authed {
				h.finish(msg.RequestID, nil)
			}
		case "response":
			if authed {
				h.appendChunk(msg.RequestID, msg.RawResponse)
				h.finish(msg.RequestID, nil)
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
				h.finish(msg.RequestID, rpcErrf(status, code, "%s", text))
			}
		}
	}

	h.mu.Lock()
	if h.conn == conn {
		h.conn = nil
	}
	h.mu.Unlock()
	if authed {
		logf("页面执行器下线")
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
