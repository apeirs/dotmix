// Package substrate talks to public Polkadot SDK (Substrate) RPC nodes
// through the Nym mixnet: JSON-RPC over websocket (subscriptions included)
// and over HTTPS POST.
package substrate

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Asset is a pallet-assets token worth showing in `dotmix balance`.
type Asset struct {
	ID       uint32
	Symbol   string
	Decimals int
}

// Chain is a network with public, keyless RPC nodes (tried in order).
type Chain struct {
	Name     string
	Display  string // what system_chain answers
	Token    string
	Decimals int
	SS58     int    // address prefix (0 Polkadot, 2 Kusama, 42 generic)
	Genesis  string // block 0 hash; every upstream must prove it serves this chain
	Testnet  bool
	RPC      []string // wss:// URLs; HTTPS POST goes to the same URL as https://
	Assets   []Asset
}

// Chains are the built-in chains. Each upstream answered both wss and https
// from the operator's network on 2026-11-07, and every one is checked again
// through the mixnet by `dotmix probe`. Add your own node, or any other
// Polkadot SDK chain, with --add name=wss://host|wss://host2.
//
// Since the 2025 Asset Hub migration, DOT and KSM balances, transfers and
// USDT/USDC live on the Asset Hub chains, not on the relay chains.
var Chains = []Chain{
	{"polkadot", "Polkadot", "DOT", 10, 0, "0x91b171bb158e2d3848fa23a9f1c25182fb8e20313b2c1eb49219da7a70ce90c3", false,
		[]string{"wss://rpc.polkadot.io", "wss://polkadot-rpc.publicnode.com", "wss://rpc-polkadot.luckyfriday.io", "wss://polkadot.api.onfinality.io/public"}, nil},
	{"assethub", "Polkadot Asset Hub", "DOT", 10, 0, "0x68d56f15f85d3136970ec16946040bc1752654e906147f7e43e9d539d7c3de2f", false,
		[]string{"wss://polkadot-asset-hub-rpc.polkadot.io", "wss://rpc-asset-hub-polkadot.luckyfriday.io", "wss://statemint.api.onfinality.io/public"},
		[]Asset{{1984, "USDT", 6}, {1337, "USDC", 6}}},
	{"kusama", "Kusama", "KSM", 12, 2, "0xb0a8d493285c2df73290dfb7e61f870f17b41801197a149ca93654499ea3dafe", false,
		[]string{"wss://kusama-rpc.polkadot.io", "wss://kusama-rpc.publicnode.com", "wss://rpc-kusama.luckyfriday.io", "wss://kusama.api.onfinality.io/public"}, nil},
	{"kusama-assethub", "Kusama Asset Hub", "KSM", 12, 2, "0x48239ef607d7928874027a43a67689209727dfb3d3dc5e5b03a39bdc2eda771a", false,
		[]string{"wss://kusama-asset-hub-rpc.polkadot.io", "wss://rpc-asset-hub-kusama.luckyfriday.io"},
		[]Asset{{1984, "USDT", 6}}},
	{"bittensor", "Bittensor", "TAO", 9, 42, "0x2f0555cc76fc2840a25a6ea3b9637146806f1f44b090c175ffde2a7e5ab36c03", false,
		[]string{"wss://entrypoint-finney.opentensor.ai", "wss://lite.chain.opentensor.ai", "wss://bittensor-finney.api.onfinality.io/public"}, nil},
	{"paseo", "Paseo", "PAS", 10, 42, "0x374057be67b355151f271ff70c3db98308c62c8adc48dc6724b6a009a1a014fd", true,
		[]string{"wss://pas-rpc.stakeworld.io", "wss://paseo-rpc.n.dwellir.com"}, nil},
	{"westend", "Westend", "WND", 12, 42, "0xe143f23803ac50e8f6f8e62695d1ce9e4e1d68aa36c1cd2cfd15340213f3423e", true,
		[]string{"wss://westend-rpc.polkadot.io"}, nil},
	{"westend-assethub", "Westend Asset Hub", "WND", 12, 42, "0x67f9723393ef76214df0118c34bbbd3dbebc8ed46a10973a8c969d48fe7598c9", true,
		[]string{"wss://westend-asset-hub-rpc.polkadot.io"}, nil},
}

// Registry holds the built-in chains plus custom ones. A custom chain has no
// pinned genesis: the first upstream that answers pins it (trust on first
// use), and every later upstream of that chain must match.
type Registry struct {
	mu sync.Mutex
	m  map[string]*Chain
}

// NewRegistry returns a registry with the built-in chains.
func NewRegistry() *Registry {
	r := &Registry{m: map[string]*Chain{}}
	for i := range Chains {
		c := Chains[i]
		c.RPC = append([]string(nil), c.RPC...)
		r.m[c.Name] = &c
	}
	return r
}

// ValidName reports whether s can name a chain in gateway paths.
func ValidName(s string) error {
	if s == "" || len(s) > 40 || strings.Trim(s, "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
		return fmt.Errorf("chain name %q: use lowercase letters, digits, - and _", s)
	}
	switch s {
	case "all", "health", "metrics", "favicon.ico":
		return fmt.Errorf("chain name %q is reserved", s)
	}
	return nil
}

// NormURL accepts wss:// or https:// (same node, same path) without
// credentials, query or fragment, and returns the wss:// form.
func NormURL(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "wss" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%s: only wss:// (or https://) upstreams are allowed", s)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s: no credentials, query or fragment in upstream URLs", s)
	}
	u.Scheme = "wss"
	return u.String(), nil
}

// HTTPSURL is the HTTPS POST endpoint of a wss:// upstream.
func HTTPSURL(wss string) string {
	return "https" + strings.TrimPrefix(wss, "wss")
}

// HostOf returns the host of an upstream URL, for logs.
func HostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

// Add sets the upstreams of a chain, creating it if new. Custom upstreams
// replace the built-in list (--add polkadot=wss://my-node keeps Polkadot's
// pinned genesis, so a wrong node is refused).
func (r *Registry) Add(name string, urls []string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := ValidName(name); err != nil {
		return err
	}
	var clean []string
	for _, u := range urls {
		if strings.TrimSpace(u) == "" {
			continue
		}
		n, err := NormURL(u)
		if err != nil {
			return err
		}
		clean = append(clean, n)
	}
	if len(clean) == 0 {
		return fmt.Errorf("chain %q: no upstream URL", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.m[name]
	if !ok {
		c = &Chain{Name: name, SS58: 42, Decimals: 12, Token: "UNIT"}
		r.m[name] = c
	}
	c.RPC = clean
	return nil
}

// ParseAdd parses "name=wss://a|wss://b".
func (r *Registry) ParseAdd(spec string) error {
	n, u, ok := strings.Cut(spec, "=")
	if !ok {
		return fmt.Errorf("want name=wss://url[|wss://url2], got %q", spec)
	}
	return r.Add(n, strings.Split(u, "|"))
}

// Get finds a chain by name (case-insensitive).
func (r *Registry) Get(name string) (*Chain, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.m[strings.ToLower(strings.TrimSpace(name))]
	return c, ok
}

// All returns the chains, built-ins first, then custom ones by name.
func (r *Registry) All() []*Chain {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Chain
	seen := map[string]bool{}
	for _, b := range Chains {
		out = append(out, r.m[b.Name])
		seen[b.Name] = true
	}
	var extra []string
	for n := range r.m {
		if !seen[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	for _, n := range extra {
		out = append(out, r.m[n])
	}
	return out
}

// ForPrefix lists the chains whose addresses use this SS58 prefix (mainnets
// only), e.g. 0 -> polkadot, assethub.
func (r *Registry) ForPrefix(p int) []*Chain {
	var out []*Chain
	for _, c := range r.All() {
		if c.SS58 == p && !c.Testnet && c.Genesis != "" {
			out = append(out, c)
		}
	}
	return out
}

// genesisMu guards Chain.Genesis for custom chains (pinned on first use).
var genesisMu sync.Mutex

// CheckGenesis compares an upstream's block-0 hash with the chain's. A chain
// without one pins it.
func (c *Chain) CheckGenesis(got string) error {
	got = strings.ToLower(got)
	genesisMu.Lock()
	defer genesisMu.Unlock()
	if c.Genesis == "" {
		c.Genesis = got
		return nil
	}
	if !strings.EqualFold(c.Genesis, got) {
		return fmt.Errorf("serves a different chain (genesis %.18s..., want %.18s...)", got, c.Genesis)
	}
	return nil
}

// GenesisPinned returns the chain's genesis hash ("" before first use).
func (c *Chain) GenesisPinned() string {
	genesisMu.Lock()
	defer genesisMu.Unlock()
	return c.Genesis
}
