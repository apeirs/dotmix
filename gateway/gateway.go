// Package gateway is a local Polkadot SDK (Substrate) RPC endpoint for
// wallets and tools. Point polkadot.js apps, Talisman, SubWallet, the
// polkadot{.js} extension, @polkadot/api, PAPI, subxt or curl at
//
//	ws://127.0.0.1:9944              the default chain (polkadot)
//	ws://127.0.0.1:9944/assethub     any chain by name (also http:// for JSON-RPC POST)
//
// and every request goes to a public RPC node of that chain through the Nym
// mixnet. Only the configured upstreams of each chain can be reached, and
// each one must prove by its genesis hash that it serves that chain.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apeirs/dotmix/substrate"
)

// Gateway serves the routes above.
type Gateway struct {
	Client  *substrate.Client
	Chains  *substrate.Registry
	Default string   // chain served at the root ("" = none)
	Hosts   []string // extra accepted Host values
	Origins []string // extra accepted Origin values ("*" = any web page)
	Timeout time.Duration
	Log     func(string, ...any)

	Total, Failed, WS atomic.Int64
}

func (g *Gateway) logf(f string, a ...any) {
	if g.Log != nil {
		g.Log(f, a...)
	}
}

func (g *Gateway) hostOK(h string) bool {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	switch h {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	for _, x := range g.Hosts {
		if x == h {
			return true
		}
	}
	return false
}

// originOK: no Origin (CLI tools, node scripts, subxt), a browser extension
// (Talisman, SubWallet, polkadot{.js}), or an origin the user allowed (for
// polkadot.js apps: --allow-origin https://polkadot.js.org). Any other web
// page is refused, so a site you visit can't use your gateway.
func (g *Gateway) originOK(o string) bool {
	if o == "" {
		return true
	}
	for _, p := range []string{"chrome-extension://", "moz-extension://", "safari-web-extension://"} {
		if strings.HasPrefix(o, p) {
			return true
		}
	}
	for _, x := range g.Origins {
		if x == "*" || x == o {
			return true
		}
	}
	return false
}

// route maps the request path to a chain. Only "/" (the default chain) and
// "/<chain>" (with an optional trailing slash) exist: no part of the path is
// ever forwarded, so a caller can't steer a request anywhere but the
// configured upstream URLs.
func (g *Gateway) route(p string) (*substrate.Chain, bool) {
	name := strings.Trim(p, "/")
	if strings.Contains(name, "/") {
		return nil, false
	}
	if name == "" {
		name = g.Default
		if name == "" {
			return nil, false
		}
	}
	return g.Chains.Get(name)
}

func headerList(s string) string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		h = strings.TrimSpace(h)
		if h != "" && len(h) < 64 && strings.Trim(strings.ToLower(h), "abcdefghijklmnopqrstuvwxyz0123456789-") == "" {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return "content-type"
	}
	return strings.Join(out, ", ")
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// DNS-rebinding guard: a web page must not reach this through a rebound name.
	if !g.hostOK(r.Host) {
		http.Error(w, "bad Host header", http.StatusForbidden)
		return
	}
	origin := r.Header.Get("Origin")
	if !g.originOK(origin) {
		http.Error(w, "origin not allowed (see --allow-origin)", http.StatusForbidden)
		return
	}
	h := w.Header()
	if origin != "" {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Vary", "Origin")
	}
	if r.Method == "OPTIONS" {
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", headerList(r.Header.Get("Access-Control-Request-Headers")))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	upgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	if r.URL.Path == "/" && r.Method == "GET" && !upgrade {
		g.help(w)
		return
	}
	// RawPath (escaped) must not differ from Path: no %2F tricks.
	if r.URL.RawPath != "" {
		http.Error(w, "encoded characters are not allowed in the path", http.StatusNotFound)
		return
	}
	ch, ok := g.route(r.URL.Path)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		g.help(w)
		return
	}
	if len(ch.RPC) == 0 {
		http.Error(w, fmt.Sprintf("chain %s has no upstream (add one with --add)", ch.Name), http.StatusNotFound)
		return
	}
	switch {
	case upgrade:
		g.websocket(w, r, ch)
	case r.Method == "POST":
		g.post(w, r, ch)
	default:
		http.Error(w, "JSON-RPC: use a websocket, or POST a JSON body", http.StatusMethodNotAllowed)
	}
}

func (g *Gateway) post(w http.ResponseWriter, r *http.Request, ch *substrate.Chain) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			http.Error(w, "POST body must be application/json", http.StatusUnsupportedMediaType)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return
	}
	if !json.Valid(body) || !bytes.ContainsAny(bytes.TrimSpace(body)[:1], "{[") {
		http.Error(w, "POST body must be a JSON-RPC request (object or batch)", http.StatusBadRequest)
		return
	}
	label := substrate.Methods(body)
	g.Total.Add(1)
	tmo := g.Timeout
	if tmo == 0 {
		tmo = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), tmo)
	defer cancel()
	t0 := time.Now()
	resp, err := g.Client.Post(ctx, ch, nil, body, label)
	if err != nil {
		g.Failed.Add(1)
		g.logf("%s POST %s FAILED after %.1fs: %v", ch.Name, label, time.Since(t0).Seconds(), err)
		g.fail(w, err)
		return
	}
	defer resp.Body.Close()
	// Only the content type comes back: no Set-Cookie, no server headers.
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	w.WriteHeader(resp.Status)
	n, _ := io.Copy(w, resp.Body)
	g.logf("%s POST %s -> mixnet -> %s HTTP %d, %d bytes in %.1fs", ch.Name, label, substrate.HostOf(resp.Upstream), resp.Status, n, time.Since(t0).Seconds())
}

// fail answers 502 with a JSON-RPC error object.
func (g *Gateway) fail(w http.ResponseWriter, err error) {
	msg := fmt.Sprintf("dotmix: all upstreams failed over the mixnet (no clearnet fallback): %v", err)
	js, _ := json.Marshal(msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":%s,"data":"MixnetUnavailable"}}`, js)
}

// websocket connects a verified upstream websocket first, then answers the
// caller's upgrade and splices the two. Frames from the upstream pass
// through untouched; frames from the caller too, but their method names (not
// params) are counted for the log.
func (g *Gateway) websocket(w http.ResponseWriter, r *http.Request, ch *substrate.Chain) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Method != "GET" || key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "bad websocket handshake", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket not supported here", http.StatusInternalServerError)
		return
	}
	g.Total.Add(1)
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	up, err := g.Client.DialWS(ctx, ch, nil)
	cancel()
	if err != nil {
		g.Failed.Add(1)
		g.logf("%s websocket FAILED after %.1fs: %v", ch.Name, time.Since(t0).Seconds(), err)
		g.fail(w, err)
		return
	}
	defer up.Close()
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	// Subprotocols aren't used by Substrate RPC; none is echoed.
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + substrate.WSAccept(key) + "\r\n\r\n"
	if _, err := io.WriteString(conn, resp); err != nil {
		return
	}
	g.WS.Add(1)
	g.logf("%s websocket -> mixnet -> %s open in %.1fs", ch.Name, substrate.HostOf(up.Upstream), time.Since(t0).Seconds())
	var counts methodCounts
	var down atomic.Int64
	done := make(chan struct{}, 2)
	go func() { copyClientFrames(up.Conn, brw.Reader, &counts); done <- struct{}{} }()
	go func() { io.Copy(countWriter{conn, &down}, up.R); done <- struct{}{} }()
	<-done
	g.logf("%s websocket closed after %.0fs: %s; %d bytes down", ch.Name, time.Since(t0).Seconds(), counts.String(), down.Load())
}

// countWriter counts bytes as they pass, so the close log line is right
// whichever side hung up first.
type countWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

type methodCounts struct {
	mu sync.Mutex
	m  map[string]int
	n  int
}

func (c *methodCounts) add(body []byte) {
	label := substrate.Methods(body)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int{}
	}
	for _, x := range strings.Split(label, ",") {
		x, _, _ = strings.Cut(x, " ")
		c.m[x]++
		c.n++
	}
}

func (c *methodCounts) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == 0 {
		return "0 requests"
	}
	keys := make([]string, 0, len(c.m))
	for k := range c.m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return c.m[keys[i]] > c.m[keys[j]] || (c.m[keys[i]] == c.m[keys[j]] && keys[i] < keys[j])
	})
	var parts []string
	for i, k := range keys {
		if i == 8 {
			parts = append(parts, fmt.Sprintf("+%d more methods", len(keys)-8))
			break
		}
		parts = append(parts, fmt.Sprintf("%s x%d", k, c.m[k]))
	}
	return fmt.Sprintf("%d requests (%s)", c.n, strings.Join(parts, ", "))
}

// copyClientFrames relays the caller's frames byte for byte. Single-frame
// text messages up to 1 MiB are unmasked (on a copy) to count method names.
func copyClientFrames(dst io.Writer, src io.Reader, counts *methodCounts) {
	for {
		f, err := substrate.ReadFrameHeader(src)
		if err != nil {
			return
		}
		if _, err := dst.Write(f.Raw); err != nil {
			return
		}
		if f.Op == 0x1 && f.Fin && f.Len <= 1<<20 {
			p := make([]byte, f.Len)
			if _, err := io.ReadFull(src, p); err != nil {
				return
			}
			if _, err := dst.Write(p); err != nil {
				return
			}
			f.Unmask(p)
			counts.add(p)
			continue
		}
		if _, err := io.CopyN(dst, src, int64(f.Len)); err != nil {
			return
		}
		if f.Op == 0x8 {
			return
		}
	}
}

func (g *Gateway) help(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "dotmix gateway: Polkadot SDK JSON-RPC over the Nym mixnet (websocket or POST).\n")
	if g.Default != "" {
		fmt.Fprintf(w, "  ws://<this host>/          %s (the default chain)\n", g.Default)
	}
	fmt.Fprintf(w, "  ws://<this host>/<chain>   that chain, e.g. /assethub\n")
	fmt.Fprintf(w, "chains:\n")
	for _, c := range g.Chains.All() {
		fmt.Fprintf(w, "  %-18s %-20s upstreams %d\n", c.Name, c.Display, len(c.RPC))
	}
	fmt.Fprintf(w, "requests so far: %d, failed: %d, websockets: %d\n", g.Total.Load(), g.Failed.Load(), g.WS.Load())
}
