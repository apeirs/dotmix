package substrate

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
)

const aliceHex = "d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d"

func TestBlake2b(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		n    int
		want string
	}{
		{[]byte("abc"), 64, "ba80a53f981c4d0d6a2797b69f12f6e94c212f14685ac4b74b12bb6fdbffa2d17d87c5392aab792dc252d5de4533cc9518d38aa8dbf1925ab92386edd4009923"},
		{seq(200), 32, "63c3d97a9f8894d5e043a707b0fee7f7ec4c049a23bbf1079df20b4165f9e22d"}, // two blocks
		{seq(128), 16, "a74787004ef589e31149183900d0294a"},                                 // exactly one block
		{nil, 16, "cae66941d9efbd404e4d88758ea67670"},
	} {
		if got := hex.EncodeToString(Blake2b(c.in, c.n)); got != c.want {
			t.Errorf("blake2b-%d(%d bytes) = %s, want %s", c.n*8, len(c.in), got, c.want)
		}
	}
}

func seq(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func TestXXH64AndTwox(t *testing.T) {
	for in, want := range map[string]uint64{
		"":    0xef46db3751d8e999,
		"a":   0xd24ec4f1a98c6e5b,
		"abc": 0x44bc2cf5ad770999,
		"Nobody inspects the spammish repetition": 0xfbcea83c8a378bf1, // 39 bytes: the 32-byte loop
	} {
		if got := XXH64([]byte(in), 0); got != want {
			t.Errorf("xxh64(%q) = %x, want %x", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"System":   "26aa394eea5630e07c48ae0c9558cef7",
		"Account":  "b99d880ec681799c0cf30e8886371da9",
		"Balances": "c2261276cc9d1f8598ea4b6a74b15c2f",
	} {
		if got := hex.EncodeToString(Twox128([]byte(in))); got != want {
			t.Errorf("twox128(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSS58(t *testing.T) {
	for addr, prefix := range map[string]int{
		"5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY": 42,
		"15oF4uVJwmo4TdGW7VfQxNLavjCXviqxT9S1MgbjMNHr6Sp5": 0,
		"HNZata7iMYWmk5RvZRTiAsSDhV8366zq2YGb3tLH5Upf74F":  2,
	} {
		a, err := ParseAccount(addr)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		if hex.EncodeToString(a.ID[:]) != aliceHex || a.Prefix != prefix {
			t.Errorf("%s -> %x prefix %d", addr, a.ID, a.Prefix)
		}
		if got := a.SS58(prefix); got != addr {
			t.Errorf("re-encode prefix %d: %s, want %s", prefix, got, addr)
		}
	}
	a, _ := ParseAccount("0x" + aliceHex)
	// Two-byte prefixes round-trip.
	for _, p := range []int{64, 255, 1284, 16383} {
		b, err := ParseAccount(a.SS58(p))
		if err != nil || b.ID != a.ID || b.Prefix != p {
			t.Errorf("prefix %d: %v %d", p, err, b.Prefix)
		}
	}
	// A typo breaks the checksum; a short or 20-byte (EVM) key is refused.
	if _, err := ParseAccount("15oF4uVJwmo4TdGW7VfQxNLavjCXviqxT9S1MgbjMNHr6Sp6"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("typo accepted: %v", err)
	}
	for _, bad := range []string{"0x1234", "0OIl", "", "1"} {
		if _, err := ParseAccount(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestStorageKeysAndDecode(t *testing.T) {
	a, _ := ParseAccount("0x" + aliceHex)
	want := "0x26aa394eea5630e07c48ae0c9558cef7b99d880ec681799c0cf30e8886371da9de1e86a9a8c739864cf3cc5ec2bea59f" + aliceHex
	if got := SystemAccountKey(a); got != want {
		t.Fatalf("System.Account key:\n got %s\nwant %s", got, want)
	}
	if k := AssetAccountKey(1984, a); !strings.HasPrefix(k, "0x"+hex.EncodeToString(Twox128([]byte("Assets")))) || !strings.HasSuffix(k, aliceHex) || len(k) != 2+2*(32+16+4+16+32) {
		t.Fatalf("Assets.Account key %s", k)
	}
	v := make([]byte, 80)
	v[0] = 7                                     // nonce
	v[8] = 1                                     // providers
	copy(v[16:], le(big.NewInt(12_345_678_900))) // free
	copy(v[32:], le(big.NewInt(10_000_000_000))) // reserved
	info, err := DecodeAccountInfo(v)
	if err != nil || info.Nonce != 7 || info.Providers != 1 || Amount(info.Free, 10) != "1.23456789" || Amount(info.Reserved, 10) != "1" || info.Frozen.Sign() != 0 {
		t.Fatalf("%+v %v", info, err)
	}
	if _, err := DecodeAccountInfo(v[:40]); err == nil {
		t.Fatal("short value accepted")
	}
	if Amount(big.NewInt(0), 12) != "0" || Amount(big.NewInt(5), 0) != "5" || Amount(big.NewInt(1), 6) != "0.000001" {
		t.Fatal("Amount")
	}
}

func le(v *big.Int) []byte {
	b := v.Bytes()
	out := make([]byte, 16)
	for i := range b {
		out[i] = b[len(b)-1-i]
	}
	return out
}

func TestCompactAndExtrinsic(t *testing.T) {
	for _, c := range []struct {
		in   string
		v    uint64
		size int
	}{{"00", 0, 1}, {"fc", 63, 1}, {"0101", 64, 2}, {"feffffff", 1<<30 - 1, 4}, {"0300000040", 1 << 30, 5}} {
		b, _ := hex.DecodeString(c.in)
		v, n, err := Compact(b)
		if err != nil || v != c.v || n != c.size {
			t.Errorf("compact %s = %d,%d,%v", c.in, v, n, err)
		}
	}
	signed, ver, err := CheckExtrinsic([]byte{0x0c, 0x84, 0x00, 0x01})
	if err != nil || !signed || ver != 4 {
		t.Fatalf("signed v4: %v %v %d", err, signed, ver)
	}
	if s, _, err := CheckExtrinsic([]byte{0x08, 0x04, 0x00}); err != nil || s {
		t.Fatal("bare extrinsic")
	}
	for _, bad := range [][]byte{nil, {0x0c, 0x84}, {0x04, 0x84, 0x00}, {0x00}} {
		if _, _, err := CheckExtrinsic(bad); err == nil {
			t.Errorf("%x accepted", bad)
		}
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	for _, bad := range []string{"ws://x.org", "http://x.org", "wss://u:p@x.org", "wss://x.org/?a=1", "wss://x.org/#f", "x.org"} {
		if err := r.Add("custom", []string{bad}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if err := r.ParseAdd("mine=https://node.example/rpc|wss://n2.example"); err != nil {
		t.Fatal(err)
	}
	c, _ := r.Get("mine")
	if strings.Join(c.RPC, " ") != "wss://node.example/rpc wss://n2.example" || HTTPSURL(c.RPC[0]) != "https://node.example/rpc" {
		t.Fatalf("%v", c.RPC)
	}
	for _, bad := range []string{"All", "a/b", "x y", "all"} {
		if err := r.Add(bad, []string{"wss://x.org"}); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	// Overriding a built-in keeps its pinned genesis.
	if err := r.Add("polkadot", []string{"wss://my-node.example"}); err != nil {
		t.Fatal(err)
	}
	p, _ := r.Get("polkadot")
	if p.CheckGenesis("0xb0a8d493285c2df73290dfb7e61f870f17b41801197a149ca93654499ea3dafe") == nil {
		t.Fatal("Kusama genesis accepted for polkadot")
	}
	// A custom chain pins the first genesis it sees.
	if c.CheckGenesis("0xAA") != nil || c.CheckGenesis("0xaa") != nil || c.CheckGenesis("0xbb") == nil {
		t.Fatal("trust on first use")
	}
	if got := ForPrefixNames(r, 0); got != "polkadot assethub" {
		t.Fatalf("prefix 0 -> %s", got)
	}
	if got := ForPrefixNames(r, 2); got != "kusama kusama-assethub" {
		t.Fatalf("prefix 2 -> %s", got)
	}
	// Built-in data is sane: unique names, wss only, genesis hashes are 32 bytes.
	seen := map[string]bool{}
	for _, c := range Chains {
		if seen[c.Name] || ValidName(c.Name) != nil || len(c.Genesis) != 66 || len(c.RPC) == 0 {
			t.Errorf("chain %s", c.Name)
		}
		seen[c.Name] = true
		for _, u := range c.RPC {
			if n, err := NormURL(u); err != nil || n != u {
				t.Errorf("%s: %v", u, err)
			}
		}
	}
}

func ForPrefixNames(r *Registry, p int) string {
	var n []string
	for _, c := range r.ForPrefix(p) {
		n = append(n, c.Name)
	}
	return strings.Join(n, " ")
}

func TestMethodsLabel(t *testing.T) {
	for body, want := range map[string]string{
		`{"jsonrpc":"2.0","id":1,"method":"state_getStorage","params":["0x26aa"]}`:               "state_getStorage",
		`[{"method":"a_b"},{"method":"x y"},{"method":"c"}]`:                                     "a_b,?,?",
		`[{"method":"a_1"},{"method":"b_1"},{"method":"c_1"},{"method":"d_1"},{"method":"e_1"}]`: "a_1,b_1,c_1,d_1 +1 more",
		`{"method":"15oF4uVJwmo4TdGW7VfQxNLavjCXviqxT9S1MgbjMNHr6Sp5"}`:                          "?",
		`{"method":"0xd43593c7_15"}`:                                                             "?",
		`not json`:                                                                               "?",
	} {
		if got := Methods([]byte(body)); got != want {
			t.Errorf("Methods(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	for _, n := range []int{5, 300, 70000} {
		buf.Reset()
		msg := bytes.Repeat([]byte("x"), n)
		if err := WriteFrame(&buf, opText, msg, true); err != nil {
			t.Fatal(err)
		}
		raw := append([]byte(nil), buf.Bytes()...)
		f, err := ReadFrameHeader(&buf)
		if err != nil || !f.Masked || f.Len != uint64(n) || !f.Fin || f.Op != opText || !bytes.Equal(f.Raw, raw[:len(f.Raw)]) {
			t.Fatalf("n=%d: %+v %v", n, f, err)
		}
		p := buf.Bytes()
		f.Unmask(p)
		if !bytes.Equal(p, msg) {
			t.Fatalf("n=%d payload", n)
		}
	}
}

func TestRPCErrorOneLine(t *testing.T) {
	e := &RPCError{Code: 1002, Message: "Verification Error: Runtime error: wasm trap: wasm `unreachable` instruction executed\nWASM backtrace:\n    0: 0x9f1e12 - x.wasm!f",
		Data: []byte(`"RuntimeApi(\"Execution failed: wasm trap: wasm ` + "`unreachable`" + ` instruction executed\\nWASM backtrace:\")"`)}
	if got := e.Error(); got != "RPC error 1002: Verification Error: Runtime error: wasm trap: wasm `unreachable` instruction executed" {
		t.Fatalf("%q", got)
	}
	e = &RPCError{Code: 1010, Message: "Invalid Transaction", Data: []byte(`"Inability to pay some fees (e.g. account balance too low)"`)}
	if got := e.Error(); got != "RPC error 1010: Invalid Transaction (Inability to pay some fees (e.g. account balance too low))" {
		t.Fatalf("%q", got)
	}
}
