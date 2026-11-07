// Command dotmix: Polkadot SDK (Substrate) RPC through the Nym mixnet.
//
//	dotmix nym setup     start nym-socks5-client on a random active exit gateway
//	dotmix serve         local gateway: ws://127.0.0.1:9944 (polkadot), /assethub, /kusama ...
//	dotmix info|balance|rpc|submit   talk to a public node directly over the mixnet
//	dotmix probe         which public nodes answer Nym exit traffic, and how fast
//	dotmix ip            which IP do the nodes see? (a Nym exit gateway)
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/apeirs/dotmix/gateway"
	"github.com/apeirs/dotmix/mixnet"
	"github.com/apeirs/dotmix/substrate"
)

const version = "0.1.0"

var start = time.Now()

func tracef(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%7.2fs] "+format+"\n", append([]any{time.Since(start).Seconds()}, args...)...)
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "dotmix:", err)
	os.Exit(1)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error {
	if s = strings.TrimSpace(s); s != "" {
		*m = append(*m, s)
	}
	return nil
}

type common struct {
	proxy, dns, chain, rpcURL string
	add                       multi
	verbose, jsonOut          bool
	timeout                   time.Duration
	reg                       *substrate.Registry
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.proxy, "proxy", envOr("DOTMIX_PROXY", "127.0.0.1:1089"), "nym-socks5-client SOCKS5 address ($DOTMIX_PROXY)")
	fs.StringVar(&c.dns, "dns", envOr("DOTMIX_DNS", mixnet.DNSDoH), "name resolution: doh (DoH over the mixnet, connect by IPv4) or exit")
	fs.StringVar(&c.chain, "chain", envOr("DOTMIX_CHAIN", ""), "polkadot, assethub, kusama, kusama-assethub, bittensor, paseo, westend, westend-assethub, or one added with --add")
	fs.StringVar(&c.rpcURL, "rpc", "", "use this wss:// node instead of the chain's list (its genesis must still match the chain)")
	fs.Var(&c.add, "add", "chain upstreams: name=wss://node[|wss://node2] (repeatable; polkadot=... replaces the built-in list)")
	fs.BoolVar(&c.verbose, "v", false, "trace every mixnet step to stderr")
	fs.BoolVar(&c.jsonOut, "json", false, "JSON output")
	fs.DurationVar(&c.timeout, "timeout", 5*time.Minute, "overall timeout for the command")
}

func (c *common) registry() *substrate.Registry {
	if c.reg != nil {
		return c.reg
	}
	c.reg = substrate.NewRegistry()
	for _, a := range c.add {
		if err := c.reg.ParseAdd(a); err != nil {
			die(fmt.Errorf("--add: %w", err))
		}
	}
	return c.reg
}

func (c *common) dialer() *mixnet.Dialer {
	proxy, err := mixnet.ParseProxy(c.proxy)
	if err != nil {
		die(err)
	}
	if c.dns != mixnet.DNSDoH && c.dns != mixnet.DNSExit {
		die(fmt.Errorf("--dns must be doh or exit"))
	}
	d := &mixnet.Dialer{Proxy: proxy, DNS: c.dns}
	if c.verbose {
		d.Trace = tracef
	}
	return d
}

func (c *common) client() *substrate.Client {
	cl := &substrate.Client{D: c.dialer()}
	if c.verbose {
		cl.Log = tracef
	}
	return cl
}

// chainOr returns the --chain chain, or def.
func (c *common) chainOr(def string) *substrate.Chain {
	name := c.chain
	if name == "" {
		name = def
	}
	ch, ok := c.registry().Get(name)
	if !ok {
		die(fmt.Errorf("unknown chain %q (see `dotmix chains`), or add it with --add %s=wss://node", name, name))
	}
	return ch
}

func (c *common) urls() []string {
	if c.rpcURL == "" {
		return nil
	}
	u, err := substrate.NormURL(c.rpcURL)
	if err != nil {
		die(err)
	}
	return []string{u}
}

func (c *common) ctx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	return ctx, func() { stop(); cancel() }
}

func (c *common) out(v any, human string) {
	if c.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return
	}
	fmt.Println(human)
}

func (c *common) open(ctx context.Context, ch *substrate.Chain) *substrate.Session {
	s, err := c.client().Open(ctx, ch, c.urls())
	if err != nil {
		die(err)
	}
	return s
}

// parse allows flags after positional arguments. A lone "-" is positional.
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dotmix `+version+`: Polkadot SDK (Substrate) RPC through the Nym mixnet

Mixnet client (official nym-socks5-client):
  dotmix nym setup [--binary PATH] [--port 1089]   init on a random active exit gateway, then start
  dotmix nym start | stop | status | exits

Local gateway for wallets and tools (Talisman, SubWallet, polkadot.js, PAPI, subxt, curl ...):
  dotmix serve [--listen 127.0.0.1:9944] [--default polkadot] [--add name=wss://node|wss://node2]
               [--allow-origin https://polkadot.js.org]
     ws://127.0.0.1:9944            the default chain (websocket JSON-RPC, subscriptions included)
     ws://127.0.0.1:9944/assethub   any chain by name; http:// for JSON-RPC POST

Talk to a public node directly over the mixnet:
  dotmix info [--chain polkadot]             chain, runtime, best and finalized block
  dotmix balance ADDRESS [--chain C]         free/reserved/frozen, nonce; USDT/USDC on Asset Hub
  dotmix rpc METHOD [PARAM...]               any one-shot JSON-RPC call (PARAM = JSON or a string)
  dotmix submit EXTRINSIC_HEX|FILE|-         forward a signed extrinsic; holds no keys
  dotmix chains                              built-in chains and upstreams
  dotmix probe [--chain all]                 which nodes work through the mixnet, and how fast
  dotmix ip                                  which IP do the nodes see? (should be a Nym exit gateway)

Common flags: --chain NAME --rpc wss://node --proxy 127.0.0.1:1089 --dns doh|exit -v --json --timeout 5m
Every node must prove by its genesis hash that it serves the chain asked for.
dotmix never holds a private key, and there is no clearnet fallback: if the
mixnet client is down, dotmix refuses to connect.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "nym":
		cmdNym(args)
	case "serve":
		cmdServe(args)
	case "info":
		cmdInfo(args)
	case "balance":
		cmdBalance(args)
	case "rpc":
		cmdRPC(args)
	case "submit", "broadcast":
		cmdSubmit(args)
	case "chains", "networks":
		cmdChains(args)
	case "probe":
		cmdProbe(args)
	case "ip":
		cmdIP(args)
	case "version", "-version", "--version":
		fmt.Println("dotmix", version)
	default:
		usage()
		os.Exit(2)
	}
}

func cmdChains(args []string) {
	var c common
	fs := flag.NewFlagSet("chains", flag.ExitOnError)
	c.register(fs)
	parse(fs, args)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CHAIN\tTOKEN\tSS58\tGENESIS\tUPSTREAMS (in order)")
	for _, ch := range c.registry().All() {
		g := ch.GenesisPinned()
		if g == "" {
			g = "(pinned on first use)"
		} else {
			g = g[:12] + "..."
		}
		name := ch.Name
		if ch.Testnet {
			name += " (testnet)"
		}
		var hosts []string
		for _, u := range ch.RPC {
			hosts = append(hosts, strings.TrimPrefix(u, "wss://"))
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", name, ch.Token, ch.SS58, g, strings.Join(hosts, " "))
	}
	tw.Flush()
}

func loopback(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
}

func cmdServe(args []string) {
	var c common
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	c.register(fs)
	listen := fs.String("listen", "127.0.0.1:9944", "local address: the default chain at /, every chain at /<name>")
	def := fs.String("default", "polkadot", "chain served at the root (\"\" = none)")
	hosts := fs.String("allow-host", "", "extra Host header values to accept (comma separated)")
	origins := fs.String("allow-origin", "", "web origins allowed to use the gateway, comma separated, e.g. https://polkadot.js.org (browser extensions and non-browser clients are always allowed)")
	parse(fs, args)
	reg := c.registry()
	if *def != "" {
		if _, ok := reg.Get(*def); !ok {
			die(fmt.Errorf("unknown --default chain %q", *def))
		}
	}
	g := &gateway.Gateway{Client: c.client(), Chains: reg, Default: *def, Log: logf}
	if c.verbose {
		g.Client.Log = logf
	}
	if *hosts != "" {
		g.Hosts = strings.Split(*hosts, ",")
	}
	if *origins != "" {
		g.Origins = strings.Split(*origins, ",")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		die(err)
	}
	if !loopback(*listen) {
		fmt.Fprintf(os.Stderr, "WARNING: %s is not bound to loopback; anyone who can reach it can use your mixnet client\n", *listen)
	}
	logf("dotmix gateway on ws://%s  (root = %s; /<chain> for the others; SOCKS5 %s)", ln.Addr(), *def, c.proxy)
	srv := &http.Server{Handler: g, ReadHeaderTimeout: 30 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		die(err)
	}
}

func cmdInfo(args []string) {
	var c common
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	c.register(fs)
	parse(fs, args)
	ch := c.chainOr("polkadot")
	ctx, cancel := c.ctx()
	defer cancel()
	t0 := time.Now()
	s := c.open(ctx, ch)
	defer s.Close()
	res, err := s.Many(ctx,
		substrate.Req{Method: "system_chain"},
		substrate.Req{Method: "state_getRuntimeVersion"},
		substrate.Req{Method: "chain_getHeader"},
		substrate.Req{Method: "chain_getFinalizedHead"},
		substrate.Req{Method: "system_health"})
	if err != nil {
		die(err)
	}
	for _, r := range res[:4] {
		if r.Err != nil {
			die(r.Err)
		}
	}
	var name string
	var rv substrate.RuntimeVersion
	var best substrate.Header
	var fin string
	var health struct {
		Peers   int  `json:"peers"`
		Syncing bool `json:"isSyncing"`
	}
	_ = json.Unmarshal(res[0].Result, &name)
	_ = json.Unmarshal(res[1].Result, &rv)
	_ = json.Unmarshal(res[2].Result, &best)
	_ = json.Unmarshal(res[3].Result, &fin)
	_ = json.Unmarshal(res[4].Result, &health)
	var finH substrate.Header
	if err := s.Call(ctx, &finH, "chain_getHeader", fin); err != nil {
		die(err)
	}
	el := time.Since(t0).Seconds()
	out := map[string]any{"chain": ch.Name, "system_chain": name, "genesis": ch.GenesisPinned(), "spec_name": rv.SpecName,
		"spec_version": rv.SpecVersion, "best": best.Height(), "finalized": finH.Height(), "finalized_hash": fin,
		"syncing": health.Syncing, "upstream": s.Upstream(), "elapsed_s": el}
	human := fmt.Sprintf("%s: best block #%d, finalized #%d, runtime %s/%d (genesis %.12s... verified)\n  via %s over the mixnet, %.1fs",
		name, best.Height(), finH.Height(), rv.SpecName, rv.SpecVersion, ch.GenesisPinned(), substrate.HostOf(s.Upstream()), el)
	if ch.Display != "" && name != ch.Display {
		human += fmt.Sprintf("\nWARNING: expected %q, the node says %q", ch.Display, name)
	}
	if health.Syncing {
		human += "\nWARNING: that node reports it is still syncing"
	}
	c.out(out, human)
}

type balanceRow struct {
	Chain    string            `json:"chain"`
	Token    string            `json:"token"`
	Free     string            `json:"free"`
	Reserved string            `json:"reserved"`
	Frozen   string            `json:"frozen"`
	Nonce    uint32            `json:"nonce"`
	Exists   bool              `json:"exists"`
	Assets   map[string]string `json:"assets,omitempty"`
	Upstream string            `json:"upstream"`
	ElapsedS float64           `json:"elapsed_s"`
	Error    string            `json:"error,omitempty"`
	human    string
}

func cmdBalance(args []string) {
	var c common
	fs := flag.NewFlagSet("balance", flag.ExitOnError)
	c.register(fs)
	pos := parse(fs, args)
	if len(pos) != 1 {
		die(fmt.Errorf("usage: dotmix balance ADDRESS [--chain assethub]"))
	}
	acct, err := substrate.ParseAccount(pos[0])
	if err != nil {
		die(err)
	}
	var chains []*substrate.Chain
	if c.chain != "" {
		chains = []*substrate.Chain{c.chainOr("")}
	} else {
		if acct.Prefix < 0 || acct.Prefix == 42 {
			die(fmt.Errorf("a generic (prefix 42) or hex address can be on any chain: pass --chain (bittensor, paseo, westend ...)"))
		}
		chains = c.registry().ForPrefix(acct.Prefix)
		if len(chains) == 0 {
			die(fmt.Errorf("no built-in chain uses SS58 prefix %d: pass --chain", acct.Prefix))
		}
	}
	ctx, cancel := c.ctx()
	defer cancel()
	cl := c.client()
	rows := make([]balanceRow, len(chains))
	var wg sync.WaitGroup
	for i, ch := range chains {
		wg.Add(1)
		go func(i int, ch *substrate.Chain) {
			defer wg.Done()
			rows[i] = balanceOn(ctx, cl, &c, ch, acct)
		}(i, ch)
	}
	wg.Wait()
	var human []string
	failed := 0
	for _, r := range rows {
		human = append(human, r.human)
		if r.Error != "" {
			failed++
		}
	}
	c.out(map[string]any{"account": acct.Hex(), "balances": rows}, strings.Join(human, "\n"))
	if failed == len(rows) {
		os.Exit(1)
	}
}

func balanceOn(ctx context.Context, cl *substrate.Client, c *common, ch *substrate.Chain, a substrate.Account) balanceRow {
	t0 := time.Now()
	row := balanceRow{Chain: ch.Name, Token: ch.Token}
	addr := a.SS58(ch.SS58)
	s, err := cl.Open(ctx, ch, c.urls())
	if err != nil {
		row.Error = err.Error()
		row.human = fmt.Sprintf("%s on %s: FAILED %v", addr, ch.Display, err)
		return row
	}
	defer s.Close()
	row.Upstream = s.Upstream()
	reqs := []substrate.Req{{Method: "state_getStorage", Params: []any{substrate.SystemAccountKey(a)}}}
	for _, as := range ch.Assets {
		reqs = append(reqs, substrate.Req{Method: "state_getStorage", Params: []any{substrate.AssetAccountKey(as.ID, a)}})
	}
	res, err := s.Many(ctx, reqs...)
	if err == nil && res[0].Err != nil {
		err = res[0].Err
	}
	if err != nil {
		row.Error = err.Error()
		row.human = fmt.Sprintf("%s on %s: FAILED %v", addr, ch.Display, err)
		return row
	}
	row.ElapsedS = time.Since(t0).Seconds()
	var v *string
	_ = json.Unmarshal(res[0].Result, &v)
	info := substrate.AccountInfo{Free: new(big.Int), Reserved: new(big.Int), Frozen: new(big.Int)}
	if v != nil {
		b, err := substrate.DecodeHex(*v)
		if err == nil {
			info, err = substrate.DecodeAccountInfo(b)
		}
		if err != nil {
			row.Error = err.Error()
			row.human = fmt.Sprintf("%s on %s: could not decode the account (%v)", addr, ch.Display, err)
			return row
		}
		row.Exists = true
	}
	row.Free = substrate.Amount(info.Free, ch.Decimals)
	row.Reserved = substrate.Amount(info.Reserved, ch.Decimals)
	row.Frozen = substrate.Amount(info.Frozen, ch.Decimals)
	row.Nonce = info.Nonce
	h := fmt.Sprintf("%s on %s:\n  %s %s free, %s reserved, %s frozen, nonce %d", addr, ch.Display, row.Free, ch.Token, row.Reserved, row.Frozen, row.Nonce)
	if !row.Exists {
		h = fmt.Sprintf("%s on %s:\n  no account (0 %s)", addr, ch.Display, ch.Token)
	}
	for i, as := range ch.Assets {
		r := res[i+1]
		var av *string
		if r.Err != nil || json.Unmarshal(r.Result, &av) != nil {
			continue
		}
		bal := new(big.Int)
		if av != nil {
			if b, err := substrate.DecodeHex(*av); err == nil {
				if x, err := substrate.DecodeAssetBalance(b); err == nil {
					bal = x
				}
			}
		}
		if row.Assets == nil {
			row.Assets = map[string]string{}
		}
		row.Assets[as.Symbol] = substrate.Amount(bal, as.Decimals)
		h += fmt.Sprintf("\n  %s %s (asset %d)", row.Assets[as.Symbol], as.Symbol, as.ID)
	}
	row.human = h + fmt.Sprintf("\n  (via %s over the mixnet, %.1fs)", substrate.HostOf(s.Upstream()), row.ElapsedS)
	return row
}

func cmdRPC(args []string) {
	var c common
	fs := flag.NewFlagSet("rpc", flag.ExitOnError)
	c.register(fs)
	pos := parse(fs, args)
	if len(pos) < 1 {
		die(fmt.Errorf("usage: dotmix rpc METHOD [PARAM...] [--chain polkadot]   e.g. dotmix rpc chain_getBlockHash 1"))
	}
	method := pos[0]
	if substrate.SafeMethod(method) == "?" {
		die(fmt.Errorf("bad method name %q", method))
	}
	if strings.Contains(strings.ToLower(method), "subscribe") && !strings.Contains(strings.ToLower(method), "unsubscribe") {
		die(fmt.Errorf("%s is a subscription; use the gateway (dotmix serve) for those", method))
	}
	params := []any{}
	for _, p := range pos[1:] {
		var v any
		if json.Unmarshal([]byte(p), &v) == nil {
			params = append(params, json.RawMessage(p))
		} else {
			params = append(params, p)
		}
	}
	ch := c.chainOr("polkadot")
	ctx, cancel := c.ctx()
	defer cancel()
	t0 := time.Now()
	s := c.open(ctx, ch)
	defer s.Close()
	var raw json.RawMessage
	if err := s.Call(ctx, &raw, method, params...); err != nil {
		die(err)
	}
	if c.jsonOut {
		fmt.Println(string(raw))
		return
	}
	var pretty any
	_ = json.Unmarshal(raw, &pretty)
	b, _ := json.MarshalIndent(pretty, "", "  ")
	fmt.Println(string(b))
	fmt.Fprintf(os.Stderr, "(%s on %s via %s over the mixnet, %.1fs)\n", method, ch.Name, substrate.HostOf(s.Upstream()), time.Since(t0).Seconds())
}

func cmdSubmit(args []string) {
	var c common
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	c.register(fs)
	force := fs.Bool("allow-unsigned", false, "also forward an unsigned (bare) extrinsic")
	pos := parse(fs, args)
	if len(pos) != 1 {
		die(fmt.Errorf("usage: dotmix submit EXTRINSIC_HEX | FILE | - --chain assethub   (a signed, SCALE-encoded extrinsic)"))
	}
	if c.chain == "" {
		die(fmt.Errorf("pass --chain: an extrinsic is only valid on the chain it was signed for"))
	}
	raw := pos[0]
	if raw == "-" {
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
		raw = string(b)
	} else if st, err := os.Stat(raw); err == nil && !st.IsDir() {
		b, err := os.ReadFile(raw)
		if err != nil {
			die(err)
		}
		raw = string(b)
	}
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "0x")
	xt, err := hex.DecodeString(raw)
	if err != nil {
		die(errors.New("not hex"))
	}
	signed, ver, err := substrate.CheckExtrinsic(xt)
	if err != nil {
		die(err)
	}
	if !signed && !*force {
		die(fmt.Errorf("this extrinsic (format v%d) is not signed; pass --allow-unsigned to forward it anyway", ver))
	}
	ch := c.chainOr("")
	ctx, cancel := c.ctx()
	defer cancel()
	t0 := time.Now()
	s := c.open(ctx, ch)
	defer s.Close()
	var hash string
	err = s.Call(ctx, &hash, "author_submitExtrinsic", "0x"+raw)
	el := time.Since(t0).Seconds()
	var re *substrate.RPCError
	switch {
	case err == nil:
		c.out(map[string]any{"hash": hash, "chain": ch.Name, "upstream": s.Upstream(), "elapsed_s": el},
			fmt.Sprintf("submitted: %s (via %s over the mixnet, %.1fs)", hash, substrate.HostOf(s.Upstream()), el))
	case errors.As(err, &re):
		c.out(map[string]any{"rejected": true, "code": re.Code, "message": re.Message, "data": re.Data, "chain": ch.Name, "upstream": s.Upstream(), "elapsed_s": el},
			fmt.Sprintf("rejected by the node: %v (via %s over the mixnet, %.1fs)", err, substrate.HostOf(s.Upstream()), el))
		os.Exit(1)
	default:
		die(err)
	}
}

type probeRow struct {
	chain, upstream, ws, post string
	wsS, postS                float64
	ok                        bool
}

func cmdProbe(args []string) {
	var c common
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	c.register(fs)
	par := fs.Int("parallel", 4, "nodes probed at once")
	parse(fs, args)
	ctx, cancel := c.ctx()
	defer cancel()
	cl := c.client()
	var chains []*substrate.Chain
	if c.chain == "" || c.chain == "all" {
		chains = c.registry().All()
	} else {
		chains = []*substrate.Chain{c.chainOr("")}
	}
	type job struct {
		ch *substrate.Chain
		u  string
		i  int
	}
	var jobs []job
	for _, ch := range chains {
		urls := ch.RPC
		if c.rpcURL != "" {
			urls = c.urls()
		}
		for _, u := range urls {
			jobs = append(jobs, job{ch, u, len(jobs)})
		}
	}
	rows := make([]probeRow, len(jobs))
	sem := make(chan struct{}, max(1, *par))
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := probeRow{chain: j.ch.Name, upstream: strings.TrimPrefix(j.u, "wss://")}
			t0 := time.Now()
			s, err := cl.Open(ctx, j.ch, []string{j.u})
			if err != nil {
				r.ws = short(err)
			} else {
				var h substrate.Header
				if err := s.Call(ctx, &h, "chain_getHeader"); err != nil {
					r.ws = short(err)
				} else {
					r.ws = fmt.Sprintf("genesis ok, #%d", h.Height())
					r.ok = true
				}
				s.Close()
			}
			r.wsS = time.Since(t0).Seconds()
			t1 := time.Now()
			resp, err := cl.Post(ctx, j.ch, []string{j.u}, []byte(`{"jsonrpc":"2.0","id":1,"method":"chain_getHeader","params":[]}`), "chain_getHeader")
			if err != nil {
				r.post = short(err)
			} else {
				var out struct {
					Result substrate.Header    `json:"result"`
					Error  *substrate.RPCError `json:"error"`
				}
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				switch {
				case resp.Status != 200:
					r.post = fmt.Sprintf("FAIL HTTP %d", resp.Status)
				case json.Unmarshal(b, &out) != nil || out.Error != nil:
					r.post = "FAIL not a header"
				default:
					r.post = fmt.Sprintf("genesis ok, #%d", out.Result.Height())
				}
			}
			r.postS = time.Since(t1).Seconds()
			rows[j.i] = r
		}(j)
	}
	wg.Wait()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CHAIN\tNODE\tWEBSOCKET (wss)\tTIME\tHTTPS POST\tTIME")
	ok := 0
	for _, r := range rows {
		if r.ok {
			ok++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.1fs\t%s\t%.1fs\n", r.chain, r.upstream, r.ws, r.wsS, r.post, r.postS)
	}
	tw.Flush()
	fmt.Fprintf(os.Stderr, "%d of %d nodes answered over a websocket through the mixnet, with the right genesis\n", ok, len(rows))
}

func short(err error) string {
	e := strings.ReplaceAll(err.Error(), "\n", "; ")
	if len(e) > 70 {
		e = e[:70]
	}
	return "FAIL " + e
}

func cmdIP(args []string) {
	var c common
	fs := flag.NewFlagSet("ip", flag.ExitOnError)
	c.register(fs)
	noLookup := fs.Bool("no-lookup", false, "don't look the IP up in the nym-api node list")
	parse(fs, args)
	d := c.dialer()
	ctx, cancel := c.ctx()
	defer cancel()
	showIP(ctx, &c, d, *noLookup)
}
