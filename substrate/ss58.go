package substrate

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const b58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func b58Decode(s string) ([]byte, error) {
	n := new(big.Int)
	for _, c := range s {
		i := strings.IndexRune(b58Alphabet, c)
		if i < 0 {
			return nil, fmt.Errorf("invalid base58 character %q", c)
		}
		n.Mul(n, big.NewInt(58))
		n.Add(n, big.NewInt(int64(i)))
	}
	out := n.Bytes()
	for _, c := range s {
		if c != '1' {
			break
		}
		out = append([]byte{0}, out...)
	}
	return out, nil
}

func b58Encode(b []byte) string {
	n := new(big.Int).SetBytes(b)
	var out []byte
	mod := new(big.Int)
	for n.Sign() > 0 {
		n.DivMod(n, big.NewInt(58), mod)
		out = append(out, b58Alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

var ss58Pre = []byte("SS58PRE")

func ss58Checksum(data []byte) []byte {
	return Blake2b(append(append([]byte{}, ss58Pre...), data...), 64)[:2]
}

// Account is a 32-byte account id (sr25519/ed25519 public key or a derived
// pallet account) with the SS58 network prefix it was written with.
type Account struct {
	ID     [32]byte
	Prefix int // -1 when given as raw hex
}

// Hex is the account id as 0x-hex.
func (a Account) Hex() string { return "0x" + hex.EncodeToString(a.ID[:]) }

// ParseAccount accepts an SS58 address (any network prefix, 32-byte account
// ids only) or a 0x-prefixed 32-byte hex account id.
func ParseAccount(s string) (Account, error) {
	s = strings.TrimSpace(s)
	var a Account
	if strings.HasPrefix(s, "0x") {
		b, err := hex.DecodeString(s[2:])
		if err != nil || len(b) != 32 {
			return a, errors.New("hex account id must be 32 bytes (0x + 64 hex digits)")
		}
		copy(a.ID[:], b)
		a.Prefix = -1
		return a, nil
	}
	raw, err := b58Decode(s)
	if err != nil {
		return a, fmt.Errorf("not an SS58 address: %w", err)
	}
	if len(raw) < 3 {
		return a, errors.New("not an SS58 address: too short")
	}
	var prefix, plen int
	switch {
	case raw[0] < 64:
		prefix, plen = int(raw[0]), 1
	case raw[0] < 128:
		// Two-byte prefix (14 bits), as in the SS58 spec.
		if len(raw) < 4 {
			return a, errors.New("not an SS58 address: too short")
		}
		lower := (raw[0] << 2) | (raw[1] >> 6)
		upper := raw[1] & 0x3f
		prefix, plen = int(lower)|int(upper)<<8, 2
	default:
		return a, errors.New("not an SS58 address: reserved prefix byte")
	}
	body := raw[plen : len(raw)-2]
	if len(body) != 32 {
		return a, fmt.Errorf("SS58 address holds %d bytes, not a 32-byte account id", len(body))
	}
	if !bytes.Equal(ss58Checksum(raw[:len(raw)-2]), raw[len(raw)-2:]) {
		return a, errors.New("SS58 checksum mismatch (typo in the address?)")
	}
	copy(a.ID[:], body)
	a.Prefix = prefix
	return a, nil
}

// SS58 encodes the account id with a network prefix (0 Polkadot, 2 Kusama,
// 42 generic/Substrate).
func (a Account) SS58(prefix int) string {
	var data []byte
	if prefix < 64 {
		data = []byte{byte(prefix)}
	} else {
		data = []byte{byte((prefix&0xfc)>>2) | 0x40, byte(prefix>>8) | byte(prefix&3)<<6}
	}
	data = append(data, a.ID[:]...)
	return b58Encode(append(data, ss58Checksum(data)...))
}
