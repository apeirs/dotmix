package substrate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apeirs/dotmix/mixnet"
)

// UserAgent is sent upstream instead of whatever the local caller used.
const UserAgent = "Mozilla/5.0"

// Client opens JSON-RPC sessions to upstream nodes, always through the
// mixnet Dialer. There is no clearnet fallback.
type Client struct {
	D       *mixnet.Dialer
	Timeout time.Duration // per upstream attempt (connect, TLS, upgrade, genesis check)
	Log     func(format string, args ...any)

	once     sync.Once
	http     *http.Client
	mu       sync.Mutex
	verified map[string]bool // HTTPS upstreams whose genesis was checked
	Calls    atomic.Int64
}

func (c *Client) logf(f string, a ...any) {
	if c.Log != nil {
		c.Log(f, a...)
	}
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 50 * time.Second
}

// RPCError is a JSON-RPC error object from a node.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error is one line: node errors can carry whole WASM backtraces (the full
// text stays in --json output).
func (e *RPCError) Error() string {
	msg := firstLine(e.Message, 200)
	s := fmt.Sprintf("RPC error %d: %s", e.Code, msg)
	if len(e.Data) > 0 && string(e.Data) != "null" {
		d := string(e.Data)
		var str string
		if json.Unmarshal(e.Data, &str) == nil {
			d = str
		}
		// Skip data that only repeats the message (RuntimeApi("...") does).
		tail := msg
		if i := strings.LastIndex(tail, ": "); i >= 0 {
			tail = tail[i+2:]
		}
		if d = firstLine(d, 160); d != "" && !strings.Contains(d, firstLine(tail, 40)) {
			s += " (" + d + ")"
		}
	}
	return s
}

func firstLine(s string, max int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s, _, _ = strings.Cut(s, `\n`) // an escaped newline inside Rust Debug text
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

type rpcResp struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
	Method string          `json:"method"` // set on subscription notifications
}

// genesisID is the request id dotmix uses for its own genesis check, before a
// websocket is handed to the caller.
const genesisID = `"dotmix-genesis"`

var genesisReq = []byte(`{"jsonrpc":"2.0","id":` + genesisID + `,"method":"chain_getBlockHash","params":[0]}`)

// ---------------------------------------------------------------- websocket

// DialWS opens a websocket to the first upstream of ch that completes the
// upgrade over the mixnet and proves, by its block-0 hash, that it serves ch.
// The returned stream is clean: the check's answer has been consumed.
func (c *Client) DialWS(ctx context.Context, ch *Chain, urls []string) (*WSConn, error) {
	if len(urls) == 0 {
		urls = ch.RPC
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("chain %s has no upstream (add one with --add)", ch.Name)
	}
	var errs []error
	for _, u := range urls {
		t0 := time.Now()
		actx, cancel := context.WithTimeout(ctx, c.timeout())
		ws, err := c.wsOnce(actx, ch, u)
		cancel()
		if err != nil {
			c.logf("%-4s %s websocket failed after %.2fs: %v", "up", HostOf(u), time.Since(t0).Seconds(), err)
			errs = append(errs, fmt.Errorf("%s: %w", HostOf(u), err))
			if ctx.Err() != nil || errors.Is(err, mixnet.ErrProxyUnreachable) {
				break // the next upstream would need the same missing proxy
			}
			continue
		}
		c.logf("%-4s %s websocket open, genesis verified (%s) in %.2fs", "up", HostOf(u), ch.Name, time.Since(t0).Seconds())
		return ws, nil
	}
	return nil, errors.Join(errs...)
}

func (c *Client) wsOnce(ctx context.Context, ch *Chain, upstream string) (*WSConn, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Scheme != "wss" {
		return nil, fmt.Errorf("not a wss:// upstream")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	tc, err := c.D.DialTLS(ctx, net.JoinHostPort(u.Hostname(), port), &tls.Config{
		ServerName: u.Hostname(), RootCAs: c.D.RootCAs, NextProtos: []string{"http/1.1"}})
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			tc.Close()
		}
	}()
	if dl, has := ctx.Deadline(); has {
		_ = tc.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = tc.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	key := NewWSKey()
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	// Only the websocket headers and a fixed User-Agent go upstream: no
	// Origin, cookies, or anything else from the local caller.
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nUser-Agent: %s\r\n\r\n", path, u.Host, key, UserAgent)
	if _, err := io.WriteString(tc, req); err != nil {
		return nil, err
	}
	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		resp.Body.Close()
		return nil, fmt.Errorf("upgrade refused: HTTP %d", resp.StatusCode)
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != WSAccept(key) {
		return nil, errors.New("upstream sent a wrong Sec-WebSocket-Accept")
	}
	ws := &WSConn{Conn: tc, R: br, Upstream: upstream}
	if err := ws.Send(genesisReq); err != nil {
		return nil, err
	}
	for {
		msg, err := ws.Recv()
		if err != nil {
			return nil, fmt.Errorf("genesis check: %w", err)
		}
		var r rpcResp
		if json.Unmarshal(msg, &r) != nil || string(r.ID) != genesisID {
			continue // not ours (nothing else was asked yet, but be lenient)
		}
		if r.Error != nil {
			return nil, fmt.Errorf("genesis check: %w", r.Error)
		}
		var h string
		if json.Unmarshal(r.Result, &h) != nil || len(h) != 66 {
			return nil, errors.New("genesis check: not a block hash")
		}
		if err := ch.CheckGenesis(h); err != nil {
			return nil, err
		}
		break
	}
	_ = tc.SetDeadline(time.Time{})
	ok = true
	return ws, nil
}

// Session is a JSON-RPC session over a verified websocket.
type Session struct {
	ws    *WSConn
	Chain *Chain
	next  int
	c     *Client
}

// Open starts a session on the first working upstream of ch (or of urls).
func (c *Client) Open(ctx context.Context, ch *Chain, urls []string) (*Session, error) {
	ws, err := c.DialWS(ctx, ch, urls)
	if err != nil {
		return nil, err
	}
	return &Session{ws: ws, Chain: ch, c: c}, nil
}

// Upstream is the wss:// URL in use.
func (s *Session) Upstream() string { return s.ws.Upstream }

// Close ends the session.
func (s *Session) Close() { s.ws.CloseWS() }

// Req is one call in a pipelined batch.
type Req struct {
	Method string
	Params []any
}

// Call sends one request and decodes its result into v (if non-nil).
func (s *Session) Call(ctx context.Context, v any, method string, params ...any) error {
	res, err := s.Many(ctx, Req{method, params})
	if err != nil {
		return err
	}
	if res[0].Err != nil {
		return res[0].Err
	}
	if v != nil {
		return json.Unmarshal(res[0].Result, v)
	}
	return nil
}

// Result is one answer of Many.
type Result struct {
	Result json.RawMessage
	Err    error
}

// Many pipelines the requests on the websocket (all sent at once, each with
// its own id) and waits for every answer: one mixnet round trip instead of
// one per call. Subscription notifications are skipped.
func (s *Session) Many(ctx context.Context, reqs ...Req) ([]Result, error) {
	ids := map[string]int{}
	for i, r := range reqs {
		s.next++
		id := fmt.Sprint(s.next)
		ids[id] = i
		params := r.Params
		if params == nil {
			params = []any{}
		}
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.next, "method": r.Method, "params": params})
		if err != nil {
			return nil, err
		}
		if err := s.ws.Send(b); err != nil {
			return nil, err
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = s.ws.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	out := make([]Result, len(reqs))
	left := len(reqs)
	for left > 0 {
		msg, err := s.ws.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		var r rpcResp
		if json.Unmarshal(msg, &r) != nil {
			continue
		}
		i, ok := ids[string(r.ID)]
		if !ok {
			continue
		}
		delete(ids, string(r.ID))
		left--
		s.c.Calls.Add(1)
		if r.Error != nil {
			out[i].Err = r.Error
		} else {
			out[i].Result = r.Result
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- HTTPS POST

func (c *Client) client() *http.Client {
	c.once.Do(func() {
		// Keep-alives on: a mixnet stream plus a TLS handshake is expensive.
		c.http = &http.Client{
			Transport: &http.Transport{
				Proxy:               nil, // never an environment proxy
				DialContext:         c.D.DialContext,
				TLSHandshakeTimeout: 90 * time.Second,
				TLSClientConfig:     &tls.Config{RootCAs: c.D.RootCAs},
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 4,
			},
			// JSON-RPC has no business redirecting. A redirect is treated as a
			// failed upstream, and its Location never reaches the caller.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	})
	return c.http
}

// Response is an upstream answer to a JSON-RPC POST. The caller closes Body.
type Response struct {
	Status      int
	ContentType string
	Body        io.ReadCloser
	Upstream    string
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

func (c *Client) post(ctx context.Context, upstream string, body []byte) (*http.Response, error) {
	hr, err := http.NewRequestWithContext(ctx, "POST", HTTPSURL(upstream), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header = http.Header{}
	hr.Header.Set("User-Agent", UserAgent)
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(hr)
	if err != nil {
		// *url.Error carries the URL; keep only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	return resp, nil
}

// verifyHTTP checks once per upstream that its HTTPS endpoint serves ch.
func (c *Client) verifyHTTP(ctx context.Context, ch *Chain, upstream string) error {
	c.mu.Lock()
	done := c.verified[upstream+"|"+ch.Name]
	c.mu.Unlock()
	if done {
		return nil
	}
	resp, err := c.post(ctx, upstream, genesisReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("genesis check: HTTP %d", resp.StatusCode)
	}
	var r rpcResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return fmt.Errorf("genesis check: %w", err)
	}
	if r.Error != nil {
		return fmt.Errorf("genesis check: %w", r.Error)
	}
	var h string
	if json.Unmarshal(r.Result, &h) != nil || len(h) != 66 {
		return errors.New("genesis check: not a block hash")
	}
	if err := ch.CheckGenesis(h); err != nil {
		return err
	}
	c.mu.Lock()
	if c.verified == nil {
		c.verified = map[string]bool{}
	}
	c.verified[upstream+"|"+ch.Name] = true
	c.mu.Unlock()
	return nil
}

func retryable(status int) bool {
	return status >= 500 || status == 429 || (status >= 300 && status < 400)
}

// Post forwards a JSON-RPC request body (byte for byte) to the first upstream
// of ch that is verified and answers without a server error. label is for
// logs only (method names, never params). If every upstream answered
// 5xx/429, the last such answer is returned.
func (c *Client) Post(ctx context.Context, ch *Chain, urls []string, body []byte, label string) (*Response, error) {
	if len(urls) == 0 {
		urls = ch.RPC
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("chain %s has no upstream", ch.Name)
	}
	var errs []error
	var fallback *Response
	for _, u := range urls {
		host := HostOf(u)
		actx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(c.timeout(), cancel)
		t0 := time.Now()
		err := c.verifyHTTP(actx, ch, u)
		var resp *http.Response
		if err == nil {
			resp, err = c.post(actx, u, body)
		}
		stopped := timer.Stop()
		if err != nil {
			cancel()
			if !stopped {
				err = fmt.Errorf("no answer within %s", c.timeout())
			}
			c.logf("%-4s %s %s failed after %.2fs: %v", "up", host, label, time.Since(t0).Seconds(), err)
			errs = append(errs, fmt.Errorf("%s: %w", host, err))
			if ctx.Err() != nil || errors.Is(err, mixnet.ErrProxyUnreachable) {
				break
			}
			continue
		}
		if retryable(resp.StatusCode) {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			c.logf("%-4s %s %s answered HTTP %d in %.2fs, trying the next upstream", "up", host, label, resp.StatusCode, time.Since(t0).Seconds())
			errs = append(errs, fmt.Errorf("%s: HTTP %d", host, resp.StatusCode))
			if resp.StatusCode >= 500 || resp.StatusCode == 429 {
				fallback = &Response{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: io.NopCloser(bytes.NewReader(b)), Upstream: u}
			}
			continue
		}
		c.Calls.Add(1)
		c.logf("%-4s %s %s answered HTTP %d in %.2fs", "up", host, label, resp.StatusCode, time.Since(t0).Seconds())
		return &Response{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"),
			Body: cancelBody{resp.Body, cancel}, Upstream: u}, nil
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, errors.Join(errs...)
}

// ---------------------------------------------------------------- log labels

// Methods extracts the method names of a JSON-RPC request or batch for logs.
// Only names made of letters, digits and _ are kept; params never are.
func Methods(body []byte) string {
	var one struct {
		Method string `json:"method"`
	}
	var many []struct {
		Method string `json:"method"`
	}
	var names []string
	t := bytes.TrimSpace(body)
	switch {
	case len(t) > 0 && t[0] == '[':
		if json.Unmarshal(t, &many) != nil {
			return "?"
		}
		for _, m := range many {
			names = append(names, SafeMethod(m.Method))
		}
	case json.Unmarshal(t, &one) == nil:
		names = append(names, SafeMethod(one.Method))
	default:
		return "?"
	}
	if len(names) > 4 {
		return fmt.Sprintf("%s +%d more", strings.Join(names[:4], ","), len(names)-4)
	}
	return strings.Join(names, ",")
}

// SafeMethod returns m if it looks like a Substrate RPC method name
// (namespace_name: state_getStorage, chainHead_v1_follow), else "?". Anything
// else could be a param in the wrong field, so it is never logged.
func SafeMethod(m string) string {
	if m == "" || len(m) > 64 || m[0] < 'a' || m[0] > 'z' || !strings.Contains(m, "_") ||
		strings.Trim(m, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" {
		return "?"
	}
	return m
}
