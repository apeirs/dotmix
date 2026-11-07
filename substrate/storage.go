package substrate

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// StorageKey builds pallet ++ item ++ hashed map keys (each already hashed).
func StorageKey(pallet, item string, keys ...[]byte) string {
	k := append(Twox128([]byte(pallet)), Twox128([]byte(item))...)
	for _, x := range keys {
		k = append(k, x...)
	}
	return "0x" + hex.EncodeToString(k)
}

// SystemAccountKey is the storage key of System.Account(who).
func SystemAccountKey(a Account) string {
	return StorageKey("System", "Account", Blake2_128Concat(a.ID[:]))
}

// AssetAccountKey is the storage key of Assets.Account(asset, who).
func AssetAccountKey(asset uint32, a Account) string {
	id := binary.LittleEndian.AppendUint32(nil, asset)
	return StorageKey("Assets", "Account", Blake2_128Concat(id), Blake2_128Concat(a.ID[:]))
}

// AccountInfo is frame_system::AccountInfo with pallet_balances::AccountData.
type AccountInfo struct {
	Nonce, Consumers, Providers, Sufficients uint32
	Free, Reserved, Frozen                   *big.Int
}

func u128(b []byte) *big.Int {
	be := make([]byte, 16)
	for i := 0; i < 16; i++ {
		be[i] = b[15-i]
	}
	return new(big.Int).SetBytes(be)
}

// DecodeHex decodes a 0x-hex storage value.
func DecodeHex(s string) ([]byte, error) {
	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
}

// DecodeAccountInfo decodes a System.Account value: four u32 counters, then
// free, reserved, frozen and flags as u128 (80 bytes in all).
func DecodeAccountInfo(b []byte) (AccountInfo, error) {
	var a AccountInfo
	if len(b) < 64 {
		return a, fmt.Errorf("AccountInfo too short (%d bytes)", len(b))
	}
	a.Nonce = binary.LittleEndian.Uint32(b[0:])
	a.Consumers = binary.LittleEndian.Uint32(b[4:])
	a.Providers = binary.LittleEndian.Uint32(b[8:])
	a.Sufficients = binary.LittleEndian.Uint32(b[12:])
	a.Free = u128(b[16:32])
	a.Reserved = u128(b[32:48])
	a.Frozen = u128(b[48:64])
	return a, nil
}

// DecodeAssetBalance decodes the balance (first u128) of an Assets.Account value.
func DecodeAssetBalance(b []byte) (*big.Int, error) {
	if len(b) < 16 {
		return nil, fmt.Errorf("asset account too short (%d bytes)", len(b))
	}
	return u128(b[:16]), nil
}

// Amount renders base units with the given decimals: 12345678900, 10 -> "1.23456789".
func Amount(v *big.Int, decimals int) string {
	if v == nil {
		return "0"
	}
	r := new(big.Rat).SetFrac(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)).FloatString(decimals)
	if strings.Contains(r, ".") {
		r = strings.TrimRight(strings.TrimRight(r, "0"), ".")
	}
	return r
}

// Compact decodes a SCALE compact integer, returning the value and the
// number of bytes it took.
func Compact(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, errors.New("empty")
	}
	switch b[0] & 3 {
	case 0:
		return uint64(b[0] >> 2), 1, nil
	case 1:
		if len(b) < 2 {
			return 0, 0, errors.New("short compact")
		}
		return uint64(binary.LittleEndian.Uint16(b) >> 2), 2, nil
	case 2:
		if len(b) < 4 {
			return 0, 0, errors.New("short compact")
		}
		return uint64(binary.LittleEndian.Uint32(b) >> 2), 4, nil
	default:
		n := int(b[0]>>2) + 4
		if n > 8 || len(b) < 1+n {
			return 0, 0, errors.New("compact too large")
		}
		var v uint64
		for i := n; i >= 1; i-- {
			v = v<<8 | uint64(b[i])
		}
		return v, 1 + n, nil
	}
}

// CheckExtrinsic verifies that b is one length-prefixed SCALE extrinsic (the
// shape author_submitExtrinsic takes) and reports whether it is signed.
// dotmix only forwards; it never builds or signs one.
func CheckExtrinsic(b []byte) (signed bool, version byte, err error) {
	n, l, err := Compact(b)
	if err != nil {
		return false, 0, fmt.Errorf("no SCALE length prefix: %w", err)
	}
	if uint64(len(b)-l) != n || n == 0 {
		return false, 0, fmt.Errorf("length prefix says %d bytes, %d follow: not one encoded extrinsic", n, len(b)-l)
	}
	v := b[l]
	return v&0x80 != 0, v & 0x3f, nil
}

// Header is the part of a block header dotmix shows.
type Header struct {
	Number     string `json:"number"`
	ParentHash string `json:"parentHash"`
	StateRoot  string `json:"stateRoot"`
}

// Height parses the hex block number.
func (h Header) Height() uint64 {
	var n uint64
	fmt.Sscanf(strings.TrimPrefix(h.Number, "0x"), "%x", &n)
	return n
}

// RuntimeVersion is the part of state_getRuntimeVersion dotmix shows.
type RuntimeVersion struct {
	SpecName    string `json:"specName"`
	SpecVersion int    `json:"specVersion"`
	TxVersion   int    `json:"transactionVersion"`
}
