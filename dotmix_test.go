package dotmix_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apeirs/dotmix/gateway"
	"github.com/apeirs/dotmix/internal/fakes"
	"github.com/apeirs/dotmix/mixnet"
	"github.com/apeirs/dotmix/substrate"
)

const (
	gPolkadot = "0x91b171bb158e2d3848fa23a9f1c25182fb8e20313b2c1eb49219da7a70ce90c3"
	gKusama   = "0xb0a8d493285c2df73290dfb7e61f870f17b41801197a149ca93654499ea3dafe"
	alice     = "15oF4uVJwmo4TdGW7VfQxNLavjCXviqxT9S1MgbjMNHr6Sp5"
	testXt    = "0x0c84deadbeef"
)

var aliceKey = func() string {
	a, _ := substrate.ParseAccount(alice)
	return substrate.SystemAccountKey(a)
}()

// seen is one request (an HTTP POST or a websocket message) as a fake node received it.
type seen struct {
	Host, Kind, Path, Body string
	Header                 http.Header
}

type world struct {
	socks *fakes.SOCKS
	d     *mixnet.Dialer
	cl    *substrate.Client
	mu    sync.Mutex
	got   []seen
	logs  []string
}

func (w *world) add(s seen) {
	w.mu.Lock()
	w.got = append(w.got, s)
	w.mu.Unlock()
}

func (w *world) all(host string) []seen {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []seen
	for _, s := range w.got {
		if s.Host == host {
			out = append(out, s)
		}
	}
	return out
}

func (w *world) logf(f string, a ...any) {
	w.mu.Lock()
	w.logs = append(w.logs, fmt.Sprintf(f, a...))
	w.mu.Unlock()
}

func (w *world) allLogs() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.logs, "\n")
}

// answer is a fake Substrate node's JSON-RPC logic.
func answer(genesis string, req []byte) []byte {
	var r struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params []json.RawMessage
	}
	if json.Unmarshal(req, &r) != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)
	}
	res := func(v string) []byte { return []byte(`{"jsonrpc":"2.0","id":` + string(r.ID) + `,"result":` + v + `}`) }
	switch r.Method {
	case "chain_getBlockHash":
		return res(`"` + genesis + `"`)
	case "system_chain":
		return res(`"Polkadot"`)
	case "chain_getHeader":
		return res(`{"number":"0x1f4","parentHash":"0x00","stateRoot":"0x00"}`)
	case "state_getStorage":
		if len(r.Params) == 1 && string(r.Params[0]) == `"`+aliceKey+`"` {
			v := make([]byte, 80)
			v[0] = 3
			b := big.NewInt(25_000_000_000).Bytes()
			for i := range b {
				v[16+i] = b[len(b)-1-i]
			}
			return res(`"0x` + hex.EncodeToString(v) + `"`)
		}
		return res(`null`)
	case "author_submitExtrinsic":
		return []byte(`{"jsonrpc":"2.0","id":` + string(r.ID) + `,"error":{"code":1002,"message":"Verification Error: Runtime error: Execution failed","data":"RuntimeApi(\"Could not decode\")"}}`)
	case "test_echo":
		return res(string(req))
	}
	return []byte(`{"jsonrpc":"2.0","id":` + string(r.ID) + `,"error":{"code":-32601,"message":"Method not found"}}`)
}

// node returns a handler for a fake node: websocket JSON-RPC (with a
// chain_subscribeNewHeads that sends one notification) and HTTPS POST.
func (w *world) node(name, genesis string) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.add(seen{name, "upgrade", r.URL.RequestURI(), "", r.Header.Clone()})
			conn, brw, err := rw.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", substrate.WSAccept(r.Header.Get("Sec-WebSocket-Key")))
			for {
				f, err := substrate.ReadFrameHeader(brw.Reader)
				if err != nil {
					return
				}
				p := make([]byte, f.Len)
				if _, err := io.ReadFull(brw.Reader, p); err != nil {
					return
				}
				f.Unmask(p)
				if f.Op == 0x8 {
					return
				}
				w.add(seen{name, "ws", r.URL.Path, string(p), nil})
				if strings.Contains(string(p), "chain_subscribeNewHeads") {
					var q struct{ ID json.RawMessage }
					json.Unmarshal(p, &q)
					substrate.WriteFrame(conn, 0x1, []byte(`{"jsonrpc":"2.0","id":`+string(q.ID)+`,"result":"sub1"}`), false)
					substrate.WriteFrame(conn, 0x1, []byte(`{"jsonrpc":"2.0","method":"chain_newHead","params":{"subscription":"sub1","result":{"number":"0x1f5"}}}`), false)
					continue
				}
				substrate.WriteFrame(conn, 0x1, answer(genesis, p), false)
			}
		}
		b, _ := io.ReadAll(r.Body)
		w.add(seen{name, "post", r.URL.Path, string(b), r.Header.Clone()})
		rw.Header().Set("Set-Cookie", "track=1")
		rw.Header().Set("X-Node-Secret", "1")
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		if strings.HasPrefix(strings.TrimSpace(string(b)), "[") {
			var batch []json.RawMessage
			json.Unmarshal(b, &batch)
			var out []string
			for _, q := range batch {
				out = append(out, string(answer(genesis, q)))
			}
			io.WriteString(rw, "["+strings.Join(out, ",")+"]")
			return
		}
		rw.Write(answer(genesis, b))
	}
}

// The fake world: rpc.good is a Polkadot node at path /ws, rpc.wrong serves
// Kusama, rpc.bad answers 500 and refuses upgrades, rpc.busy 429, rpc.slow
// never answers, rpc.moved redirects. All are TLS, reachable only through the
// fake SOCKS proxy, with names known only to the fake DoH resolver.
func newWorld(t *testing.T) *world {
	t.Helper()
	ca := fakes.NewCA()
	w := &world{}
	serve := func(h http.HandlerFunc, name string) string {
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{ca.Leaf(name)}, NextProtos: []string{"http/1.1"}})
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: h}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
		return ln.Addr().String()
	}
	good := serve(w.node("rpc.good", gPolkadot), "rpc.good")
	wrong := serve(w.node("rpc.wrong", gKusama), "rpc.wrong")
	bad := serve(func(rw http.ResponseWriter, r *http.Request) {
		w.add(seen{"rpc.bad", "any", r.URL.Path, "", r.Header.Clone()})
		http.Error(rw, "boom", 500)
	}, "rpc.bad")
	busy := serve(func(rw http.ResponseWriter, r *http.Request) {
		w.add(seen{"rpc.busy", "any", r.URL.Path, "", r.Header.Clone()})
		http.Error(rw, "slow down", 429)
	}, "rpc.busy")
	slow := serve(func(rw http.ResponseWriter, r *http.Request) {
		w.add(seen{"rpc.slow", "any", r.URL.Path, "", r.Header.Clone()})
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}, "rpc.slow")
	moved := serve(func(rw http.ResponseWriter, r *http.Request) {
		w.add(seen{"rpc.moved", "any", r.URL.Path, "", r.Header.Clone()})
		http.Redirect(rw, r, "https://rpc.good/ws", http.StatusTemporaryRedirect)
	}, "rpc.moved")
	// rpc.flaky passes the genesis check, then answers 503 to everything else.
	goodNode := w.node("rpc.flaky", gPolkadot)
	flaky := serve(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "dotmix-genesis") {
			r.Body = io.NopCloser(strings.NewReader(string(b)))
			goodNode(rw, r)
			return
		}
		w.add(seen{"rpc.flaky", "post", r.URL.Path, string(b), r.Header.Clone()})
		http.Error(rw, "unavailable", 503)
	}, "rpc.flaky")
	doh := fakes.DoH(ca, "1.1.1.1", map[string][]string{
		"rpc.good": {"192.0.2.9"}, "rpc.wrong": {"192.0.2.10"}, "rpc.bad": {"192.0.2.8"},
		"rpc.busy": {"192.0.2.7"}, "rpc.slow": {"192.0.2.6"}, "rpc.moved": {"192.0.2.5"}, "rpc.flaky": {"192.0.2.4"}})
	w.socks = fakes.NewSOCKS(map[string]string{"1.1.1.1:443": doh, "192.0.2.9:443": good, "192.0.2.10:443": wrong,
		"192.0.2.8:443": bad, "192.0.2.7:443": busy, "192.0.2.6:443": slow, "192.0.2.5:443": moved, "192.0.2.4:443": flaky})
	t.Cleanup(w.socks.Close)
	w.d = &mixnet.Dialer{Proxy: w.socks.Addr, DNS: mixnet.DNSDoH, RootCAs: ca.Pool, Resolvers: []string{"1.1.1.1"}, Timeout: 5 * time.Second}
	w.cl = &substrate.Client{D: w.d, Timeout: 10 * time.Second, Log: w.logf}
	return w
}

func (w *world) registry(t *testing.T) *substrate.Registry {
	reg := substrate.NewRegistry()
	for _, a := range []string{"polkadot=wss://rpc.wrong/ws|wss://rpc.bad/ws|wss://rpc.good/ws", "dev=wss://rpc.good/ws", "kusama=wss://rpc.wrong/ws"} {
		if err := reg.ParseAdd(a); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func (w *world) gateway(t *testing.T) (*gateway.Gateway, *httptest.Server) {
	g := &gateway.Gateway{Client: w.cl, Chains: w.registry(t), Default: "polkadot", Log: w.logf}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

func do(t *testing.T, method, url string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// wsClient is a test websocket client of the gateway.
type wsClient struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialGW(t *testing.T, srvURL, path string, hdr map[string]string) (*wsClient, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srvURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	key := substrate.NewWSKey()
	req := "GET " + path + " HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\n"
	for k, v := range hdr {
		req += k + ": " + v + "\r\n"
	}
	io.WriteString(conn, req+"\r\n")
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == 101 && resp.Header.Get("Sec-WebSocket-Accept") != substrate.WSAccept(key) {
		t.Fatal("gateway sent a wrong Sec-WebSocket-Accept")
	}
	t.Cleanup(func() { conn.Close() })
	return &wsClient{conn, r}, resp
}

func (c *wsClient) call(t *testing.T, msg string) string {
	t.Helper()
	if err := substrate.WriteFrame(c.conn, 0x1, []byte(msg), true); err != nil {
		t.Fatal(err)
	}
	return c.read(t)
}

func (c *wsClient) read(t *testing.T) string {
	t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := substrate.ReadFrameHeader(c.r)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, f.Len)
	io.ReadFull(c.r, p)
	return string(p)
}

// assertOnlyMixnet: every connection went through the SOCKS proxy, by IPv4
// literal after DoH over it; no name was resolved locally.
func assertOnlyMixnet(t *testing.T, w *world, want ...string) {
	t.Helper()
	got := map[string]bool{}
	for _, r := range w.socks.Seen() {
		got[r.Host] = true
		if r.ATYP != 1 {
			t.Errorf("SOCKS CONNECT %v: DoH mode must connect by IPv4 literal", r)
		}
	}
	for _, h := range append(want, "1.1.1.1") {
		if !got[h] {
			t.Errorf("SOCKS never saw %s (saw %v)", h, got)
		}
	}
}

// A websocket through the gateway: the wrong-chain node and the broken node
// are skipped, then requests, answers and subscription notifications pass
// through the verified node untouched.
func TestWebsocketThroughGateway(t *testing.T) {
	w := newWorld(t)
	g, srv := w.gateway(t)
	c, resp := dialGW(t, srv.URL, "/", map[string]string{"Origin": "chrome-extension://talisman", "Cookie": "c=1", "User-Agent": "wallet/9"})
	if resp.StatusCode != 101 {
		t.Fatalf("upgrade: %d", resp.StatusCode)
	}
	q := `{"jsonrpc":"2.0","id":7,"method":"state_getStorage","params":["` + aliceKey + `"]}`
	if a := c.call(t, q); !strings.Contains(a, `"id":7`) || !strings.Contains(a, `"0x03000000`) {
		t.Fatalf("storage: %s", a)
	}
	if a := c.call(t, `{"jsonrpc":"2.0","id":8,"method":"chain_subscribeNewHeads","params":[]}`); !strings.Contains(a, `"sub1"`) {
		t.Fatalf("subscribe: %s", a)
	}
	if n := c.read(t); !strings.Contains(n, "chain_newHead") || !strings.Contains(n, "0x1f5") {
		t.Fatalf("notification: %s", n)
	}
	// A 300 KiB message (runtime metadata is bigger) in both directions.
	big := `{"jsonrpc":"2.0","id":9,"method":"test_echo","params":["` + strings.Repeat("ab", 150_000) + `"]}`
	if a := c.call(t, big); !strings.Contains(a, strings.Repeat("ab", 150_000)) {
		t.Fatalf("big echo: %d bytes", len(a))
	}
	got := w.all("rpc.good")
	var upgrades, msgs []seen
	for _, s := range got {
		if s.Kind == "upgrade" {
			upgrades = append(upgrades, s)
		} else if s.Kind == "ws" {
			msgs = append(msgs, s)
		}
	}
	if len(upgrades) != 1 || upgrades[0].Path != "/ws" {
		t.Fatalf("upgrades %v", upgrades)
	}
	h := upgrades[0].Header
	for _, k := range []string{"Origin", "Cookie", "Sec-Websocket-Extensions", "X-Forwarded-For"} {
		if h.Get(k) != "" {
			t.Errorf("%s forwarded upstream", k)
		}
	}
	if h.Get("User-Agent") != substrate.UserAgent {
		t.Errorf("User-Agent %q", h.Get("User-Agent"))
	}
	// First message is dotmix's own genesis check, then the caller's, byte for byte.
	if len(msgs) != 4 || !strings.Contains(msgs[0].Body, "chain_getBlockHash") || msgs[1].Body != q || msgs[3].Body != big {
		t.Fatalf("upstream messages: %d", len(msgs))
	}
	if len(w.all("rpc.wrong")) == 0 || len(w.all("rpc.bad")) == 0 {
		t.Fatal("failover order not followed")
	}
	for _, s := range w.all("rpc.wrong") {
		if s.Kind == "ws" && !strings.Contains(s.Body, "chain_getBlockHash") {
			t.Fatalf("the wrong-chain node got a caller request: %s", s.Body)
		}
	}
	c.conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(w.allLogs(), "websocket closed") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if l := w.allLogs(); !strings.Contains(l, "state_getStorage x1") || !strings.Contains(l, "chain_subscribeNewHeads x1") {
		t.Fatalf("method counts not logged:\n%s", l)
	}
	if g.WS.Load() != 1 {
		t.Fatal("WS counter")
	}
	assertOnlyMixnet(t, w, "192.0.2.9", "192.0.2.10", "192.0.2.8")
}

// HTTP JSON-RPC: bodies byte for byte (single and batch), the genesis is
// checked once per node, and only Content-Type comes back.
func TestPostThroughGateway(t *testing.T) {
	w := newWorld(t)
	_, srv := w.gateway(t)
	q := `{"jsonrpc":"2.0","id":1,"method":"state_getStorage","params":["` + aliceKey + `"]}`
	resp, body := do(t, "POST", srv.URL+"/dev", map[string]string{"Content-Type": "application/json", "Origin": "moz-extension://subwallet", "Cookie": "a=1", "Authorization": "Bearer x"}, q)
	if resp.StatusCode != 200 || !strings.Contains(body, "0x03000000") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Node-Secret") != "" || resp.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("response headers leaked: %v", resp.Header)
	}
	batch := `[{"jsonrpc":"2.0","id":1,"method":"system_chain","params":[]},{"jsonrpc":"2.0","id":2,"method":"chain_getHeader","params":[]}]`
	if resp, body = do(t, "POST", srv.URL+"/dev/", nil, batch); resp.StatusCode != 200 || !strings.Contains(body, "Polkadot") || !strings.Contains(body, "0x1f4") {
		t.Fatalf("batch %d %s", resp.StatusCode, body)
	}
	posts := w.all("rpc.good")
	if len(posts) != 3 || !strings.Contains(posts[0].Body, "chain_getBlockHash") || posts[1].Body != q || posts[2].Body != batch {
		t.Fatalf("upstream saw %d posts", len(posts))
	}
	for _, p := range posts {
		if p.Path != "/ws" || p.Header.Get("Cookie") != "" || p.Header.Get("Authorization") != "" || p.Header.Get("Origin") != "" || p.Header.Get("User-Agent") != substrate.UserAgent {
			t.Fatalf("upstream request %s %v", p.Path, p.Header)
		}
	}
	// Refused before anything goes out: wrong type, not JSON, a scalar.
	before := len(w.socks.Seen())
	for _, c := range []struct{ ct, body string }{{"text/plain", q}, {"application/json", "garbage"}, {"application/json", `"x"`}} {
		if resp, _ := do(t, "POST", srv.URL+"/", map[string]string{"Content-Type": c.ct}, c.body); resp.StatusCode != 415 && resp.StatusCode != 400 {
			t.Errorf("%s %q: %d", c.ct, c.body, resp.StatusCode)
		}
	}
	if len(w.socks.Seen()) != before {
		t.Fatal("a refused request still opened a stream")
	}
	assertOnlyMixnet(t, w, "192.0.2.9")
}

// Only "/" and "/<chain>" exist; nothing in the path is forwarded.
func TestRoutes(t *testing.T) {
	w := newWorld(t)
	_, srv := w.gateway(t)
	q := `{"jsonrpc":"2.0","id":1,"method":"system_chain","params":[]}`
	for _, p := range []string{"/dev/x", "/nochain", "/dev/%2e%2e/x", "/%64ev", "/dev/../../etc"} {
		if resp, _ := do(t, "POST", srv.URL+p, nil, q); resp.StatusCode != 404 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	for _, s := range w.all("rpc.good") {
		if s.Path != "/ws" {
			t.Errorf("upstream path %q", s.Path)
		}
	}
	if resp, body := do(t, "GET", srv.URL+"/", nil, ""); resp.StatusCode != 200 || !strings.Contains(body, "dev ") {
		t.Fatalf("help: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", srv.URL+"/dev", nil, ""); resp.StatusCode != 405 {
		t.Fatalf("GET chain: %d", resp.StatusCode)
	}
}

// Every node must prove its genesis; a chain whose only node serves another
// chain fails with 502, and the caller's request never reaches that node.
func TestGenesisGuard(t *testing.T) {
	w := newWorld(t)
	reg := w.registry(t)
	reg.ParseAdd("polkadot=wss://rpc.wrong/ws")
	srv := httptest.NewServer(&gateway.Gateway{Client: w.cl, Chains: reg, Default: "polkadot", Log: w.logf})
	defer srv.Close()
	q := `{"jsonrpc":"2.0","id":1,"method":"author_submitExtrinsic","params":["` + testXt + `"]}`
	resp, body := do(t, "POST", srv.URL+"/", nil, q)
	if resp.StatusCode != 502 || !strings.Contains(body, "different chain") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if _, r := dialGW(t, srv.URL, "/polkadot", nil); r.StatusCode != 502 {
		t.Fatalf("websocket to wrong chain: %d", r.StatusCode)
	}
	for _, s := range w.all("rpc.wrong") {
		if strings.Contains(s.Body, "author_submitExtrinsic") {
			t.Fatal("extrinsic sent to a node of the wrong chain")
		}
	}
	// A custom chain pins the genesis of its first node; a second node of
	// another chain is then refused.
	reg2 := substrate.NewRegistry()
	reg2.ParseAdd("mine=wss://rpc.good/ws|wss://rpc.wrong/ws")
	ch, _ := reg2.Get("mine")
	ctx := context.Background()
	if _, err := w.cl.Open(ctx, ch, []string{"wss://rpc.good/ws"}); err != nil {
		t.Fatal(err)
	}
	if ch.GenesisPinned() != gPolkadot {
		t.Fatalf("pinned %q", ch.GenesisPinned())
	}
	if _, err := w.cl.Open(ctx, ch, []string{"wss://rpc.wrong/ws"}); err == nil || !strings.Contains(err.Error(), "different chain") {
		t.Fatalf("second chain accepted: %v", err)
	}
}

func TestFailover(t *testing.T) {
	w := newWorld(t)
	reg := w.registry(t)
	ch, _ := reg.Get("polkadot")
	ctx := context.Background()
	q := []byte(`{"jsonrpc":"2.0","id":1,"method":"system_chain","params":[]}`)
	// 429, 500, a redirect, then a good node.
	resp, err := w.cl.Post(ctx, ch, []string{"wss://rpc.busy", "wss://rpc.bad", "wss://rpc.moved", "wss://rpc.good/ws"}, q, "system_chain")
	if err != nil || resp.Upstream != "wss://rpc.good/ws" {
		t.Fatalf("failover: %v", err)
	}
	resp.Body.Close()
	// Only 5xx/429 (here already at the genesis check): an error naming them. A redirect alone is an error too.
	if _, err = w.cl.Post(ctx, ch, []string{"wss://rpc.busy", "wss://rpc.bad"}, q, "x"); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("all-5xx: %v", err)
	}
	// A verified node answering 503: the next node serves; if it is the only
	// one, its answer comes back as it is.
	if resp, err = w.cl.Post(ctx, ch, []string{"wss://rpc.flaky", "wss://rpc.good/ws"}, q, "x"); err != nil || resp.Upstream != "wss://rpc.good/ws" {
		t.Fatalf("503 failover: %v", err)
	}
	if resp, err = w.cl.Post(ctx, ch, []string{"wss://rpc.flaky"}, q, "x"); err != nil || resp.Status != 503 {
		t.Fatalf("lone 503: %v", err)
	}
	if _, err = w.cl.Post(ctx, ch, []string{"wss://rpc.moved"}, q, "x"); err == nil {
		t.Fatal("redirect followed or passed on")
	}
	// A node that never answers: timeout, then the next one.
	fast := &substrate.Client{D: w.d, Timeout: 700 * time.Millisecond, Log: w.logf}
	t0 := time.Now()
	resp, err = fast.Post(ctx, ch, []string{"wss://rpc.slow", "wss://rpc.good/ws"}, q, "x")
	if err != nil || resp.Upstream != "wss://rpc.good/ws" || time.Since(t0) > 4*time.Second {
		t.Fatalf("timeout failover: %v after %s", err, time.Since(t0))
	}
	if s, err := fast.Open(ctx, ch, []string{"wss://rpc.slow", "wss://rpc.bad", "wss://rpc.good/ws"}); err != nil || s.Upstream() != "wss://rpc.good/ws" {
		t.Fatalf("websocket failover: %v", err)
	}
}

func TestHostAndOriginGuard(t *testing.T) {
	w := newWorld(t)
	g, srv := w.gateway(t)
	q := `{"jsonrpc":"2.0","id":1,"method":"system_chain","params":[]}`
	if resp, _ := do(t, "POST", srv.URL+"/", map[string]string{"Host": "evil.example"}, q); resp.StatusCode != 403 {
		t.Fatalf("foreign Host: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", srv.URL+"/", map[string]string{"Origin": "https://evil.example"}, q); resp.StatusCode != 403 {
		t.Fatalf("web origin: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "OPTIONS", srv.URL+"/", map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"}, ""); resp.StatusCode != 403 {
		t.Fatalf("preflight: %d", resp.StatusCode)
	}
	if _, r := dialGW(t, srv.URL, "/", map[string]string{"Origin": "https://evil.example"}); r.StatusCode != 403 {
		t.Fatalf("websocket from a web page: %d", r.StatusCode)
	}
	if len(w.socks.Seen()) != 0 {
		t.Fatal("a refused request opened a stream")
	}
	g.Origins = []string{"https://polkadot.js.org"}
	if _, r := dialGW(t, srv.URL, "/", map[string]string{"Origin": "https://polkadot.js.org"}); r.StatusCode != 101 {
		t.Fatalf("allowed origin: %d", r.StatusCode)
	}
	resp, _ := do(t, "OPTIONS", srv.URL+"/", map[string]string{"Origin": "https://polkadot.js.org", "Access-Control-Request-Headers": "content-type, x-evil"}, "")
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "https://polkadot.js.org" {
		t.Fatalf("allowed preflight: %d", resp.StatusCode)
	}
}

// Mixnet client down: 502, and no direct connection is ever attempted, in
// either DNS mode, for websockets and POSTs, even when a direct connection
// would work (the node is a real TLS server on localhost).
func TestNoClearnetFallback(t *testing.T) {
	ca := fakes.NewCA()
	w := &world{}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{ca.Leaf("localhost", "127.0.0.1")}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: w.node("direct", gPolkadot)}
	go srv.Serve(ln)
	defer srv.Close()
	dl, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := dl.Addr().String()
	dl.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	reg := substrate.NewRegistry()
	reg.Add("polkadot", []string{"wss://localhost:" + port, "wss://127.0.0.1:" + port})
	ch, _ := reg.Get("polkadot")
	for _, mode := range []string{mixnet.DNSDoH, mixnet.DNSExit} {
		cl := &substrate.Client{D: &mixnet.Dialer{Proxy: dead, DNS: mode, RootCAs: ca.Pool, Timeout: time.Second}, Timeout: 3 * time.Second, Log: w.logf}
		gw := httptest.NewServer(&gateway.Gateway{Client: cl, Chains: reg, Default: "polkadot", Log: w.logf})
		resp, body := do(t, "POST", gw.URL+"/", nil, `{"jsonrpc":"2.0","id":1,"method":"author_submitExtrinsic","params":["`+testXt+`"]}`)
		if resp.StatusCode != 502 || !strings.Contains(body, "no SOCKS5 proxy") || !strings.Contains(body, "MixnetUnavailable") {
			t.Errorf("dns=%s POST: %d %s", mode, resp.StatusCode, body)
		}
		if _, r := dialGW(t, gw.URL, "/", nil); r.StatusCode != 502 {
			t.Errorf("dns=%s websocket: %d", mode, r.StatusCode)
		}
		gw.Close()
		if _, err := cl.Open(context.Background(), ch, nil); err == nil {
			t.Errorf("dns=%s: CLI session opened without the mixnet", mode)
		}
		if strings.Count(w.allLogs(), "no SOCKS5 proxy") > 12 {
			t.Errorf("kept trying nodes with no proxy")
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.got) != 0 {
		t.Fatalf("the node was reached directly %d times", len(w.got))
	}
}

// Logs and error answers carry method names only: no storage keys (they
// hold the account id), addresses, extrinsic bytes or other params, on
// success and on failure.
func TestLogsCarryNoParams(t *testing.T) {
	w := newWorld(t)
	_, srv := w.gateway(t)
	a, _ := substrate.ParseAccount(alice)
	secrets := []string{aliceKey[len(aliceKey)-64:], a.Hex()[2:], strings.TrimPrefix(testXt, "0x"), alice}
	do(t, "POST", srv.URL+"/", nil, `{"jsonrpc":"2.0","id":1,"method":"state_getStorage","params":["`+aliceKey+`"]}`)
	do(t, "POST", srv.URL+"/", nil, `{"jsonrpc":"2.0","id":2,"method":"author_submitExtrinsic","params":["`+testXt+`"]}`)
	do(t, "POST", srv.URL+"/", nil, `{"jsonrpc":"2.0","id":3,"method":"`+alice+`","params":[]}`)
	c, _ := dialGW(t, srv.URL, "/dev", nil)
	c.call(t, `{"jsonrpc":"2.0","id":4,"method":"state_getStorage","params":["`+aliceKey+`"]}`)
	c.conn.Close()
	// Failure path: no proxy.
	dl, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := dl.Addr().String()
	dl.Close()
	cl := &substrate.Client{D: &mixnet.Dialer{Proxy: dead, Timeout: time.Second}, Timeout: 3 * time.Second, Log: w.logf}
	gw := httptest.NewServer(&gateway.Gateway{Client: cl, Chains: w.registry(t), Default: "polkadot", Log: w.logf})
	defer gw.Close()
	_, body := do(t, "POST", gw.URL+"/", nil, `{"jsonrpc":"2.0","id":5,"method":"state_getStorage","params":["`+aliceKey+`"]}`)
	time.Sleep(200 * time.Millisecond)
	all := w.allLogs()
	if !strings.Contains(all, "state_getStorage") {
		t.Fatalf("no method in logs:\n%s", all)
	}
	for _, s := range secrets {
		if strings.Contains(all+body, s) {
			t.Errorf("%q in logs or error answer:\n%s", s, all)
		}
	}
}

// The CLI path: a pipelined session decodes Alice's account.
func TestSessionBalance(t *testing.T) {
	w := newWorld(t)
	reg := w.registry(t)
	ch, _ := reg.Get("dev")
	ctx := context.Background()
	s, err := w.cl.Open(ctx, ch, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := substrate.ParseAccount(alice)
	res, err := s.Many(ctx, substrate.Req{Method: "state_getStorage", Params: []any{substrate.SystemAccountKey(a)}},
		substrate.Req{Method: "system_chain"}, substrate.Req{Method: "author_submitExtrinsic", Params: []any{testXt}})
	if err != nil {
		t.Fatal(err)
	}
	var v string
	json.Unmarshal(res[0].Result, &v)
	b, _ := substrate.DecodeHex(v)
	info, err := substrate.DecodeAccountInfo(b)
	if err != nil || substrate.Amount(info.Free, 10) != "2.5" || info.Nonce != 3 {
		t.Fatalf("%v %+v", err, info)
	}
	var name string
	if json.Unmarshal(res[1].Result, &name); name != "Polkadot" {
		t.Fatal(name)
	}
	if re, ok := res[2].Err.(*substrate.RPCError); !ok || re.Code != 1002 || !strings.Contains(re.Error(), "Could not decode") {
		t.Fatalf("submit error: %v", res[2].Err)
	}
}
