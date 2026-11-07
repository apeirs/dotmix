package substrate

import (
	"encoding/binary"
	"math/bits"
)

// The two hashers Substrate uses for storage keys and SS58 checksums, in pure
// Go so dotmix needs nothing outside the standard library: BLAKE2b (RFC 7693)
// with any output length up to 64 bytes, and xxHash64 ("twox").

var blakeIV = [8]uint64{
	0x6a09e667f3bcc908, 0xbb67ae8584caa73b, 0x3c6ef372fe94f82b, 0xa54ff53a5f1d36f1,
	0x510e527fade682d1, 0x9b05688c2b3e6c1f, 0x1f83d9abfb41bd6b, 0x5be0cd19137e2179,
}

var blakeSigma = [12][16]byte{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	{14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
	{11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4},
	{7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8},
	{9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13},
	{2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9},
	{12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11},
	{13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10},
	{6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5},
	{10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0},
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	{14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
}

func blakeCompress(h *[8]uint64, block []byte, t uint64, last bool) {
	var m [16]uint64
	for i := range m {
		m[i] = binary.LittleEndian.Uint64(block[i*8:])
	}
	var v [16]uint64
	copy(v[:8], h[:])
	copy(v[8:], blakeIV[:])
	v[12] ^= t
	if last {
		v[14] = ^v[14]
	}
	g := func(a, b, c, d int, x, y uint64) {
		v[a] = v[a] + v[b] + x
		v[d] = bits.RotateLeft64(v[d]^v[a], -32)
		v[c] = v[c] + v[d]
		v[b] = bits.RotateLeft64(v[b]^v[c], -24)
		v[a] = v[a] + v[b] + y
		v[d] = bits.RotateLeft64(v[d]^v[a], -16)
		v[c] = v[c] + v[d]
		v[b] = bits.RotateLeft64(v[b]^v[c], -63)
	}
	for r := 0; r < 12; r++ {
		s := &blakeSigma[r]
		g(0, 4, 8, 12, m[s[0]], m[s[1]])
		g(1, 5, 9, 13, m[s[2]], m[s[3]])
		g(2, 6, 10, 14, m[s[4]], m[s[5]])
		g(3, 7, 11, 15, m[s[6]], m[s[7]])
		g(0, 5, 10, 15, m[s[8]], m[s[9]])
		g(1, 6, 11, 12, m[s[10]], m[s[11]])
		g(2, 7, 8, 13, m[s[12]], m[s[13]])
		g(3, 4, 9, 14, m[s[14]], m[s[15]])
	}
	for i := 0; i < 8; i++ {
		h[i] ^= v[i] ^ v[i+8]
	}
}

// Blake2b returns the unkeyed BLAKE2b hash of data with an outLen-byte digest
// (1..64).
func Blake2b(data []byte, outLen int) []byte {
	if outLen < 1 || outLen > 64 {
		panic("blake2b: bad output length")
	}
	h := blakeIV
	h[0] ^= 0x01010000 ^ uint64(outLen)
	var t uint64
	for len(data) > 128 {
		t += 128
		blakeCompress(&h, data[:128], t, false)
		data = data[128:]
	}
	var last [128]byte
	copy(last[:], data)
	t += uint64(len(data))
	blakeCompress(&h, last[:], t, true)
	var out [64]byte
	for i := 0; i < 8; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], h[i])
	}
	return out[:outLen]
}

const (
	xxP1 uint64 = 11400714785074694791
	xxP2 uint64 = 14029467366897019727
	xxP3 uint64 = 1609587929392839161
	xxP4 uint64 = 9650029242287828579
	xxP5 uint64 = 2870177450012600261
)

func xxRound(acc, in uint64) uint64 {
	acc += in * xxP2
	acc = bits.RotateLeft64(acc, 31)
	return acc * xxP1
}

func xxMerge(acc, val uint64) uint64 {
	acc ^= xxRound(0, val)
	return acc*xxP1 + xxP4
}

// XXH64 is xxHash64 of b with the given seed.
func XXH64(b []byte, seed uint64) uint64 {
	n := len(b)
	var h uint64
	if n >= 32 {
		v1, v2, v3, v4 := seed+xxP1+xxP2, seed+xxP2, seed, seed-xxP1
		for len(b) >= 32 {
			v1 = xxRound(v1, binary.LittleEndian.Uint64(b[0:]))
			v2 = xxRound(v2, binary.LittleEndian.Uint64(b[8:]))
			v3 = xxRound(v3, binary.LittleEndian.Uint64(b[16:]))
			v4 = xxRound(v4, binary.LittleEndian.Uint64(b[24:]))
			b = b[32:]
		}
		h = bits.RotateLeft64(v1, 1) + bits.RotateLeft64(v2, 7) + bits.RotateLeft64(v3, 12) + bits.RotateLeft64(v4, 18)
		h = xxMerge(h, v1)
		h = xxMerge(h, v2)
		h = xxMerge(h, v3)
		h = xxMerge(h, v4)
	} else {
		h = seed + xxP5
	}
	h += uint64(n)
	for len(b) >= 8 {
		h ^= xxRound(0, binary.LittleEndian.Uint64(b))
		h = bits.RotateLeft64(h, 27)*xxP1 + xxP4
		b = b[8:]
	}
	if len(b) >= 4 {
		h ^= uint64(binary.LittleEndian.Uint32(b)) * xxP1
		h = bits.RotateLeft64(h, 23)*xxP2 + xxP3
		b = b[4:]
	}
	for _, c := range b {
		h ^= uint64(c) * xxP5
		h = bits.RotateLeft64(h, 11) * xxP1
	}
	h ^= h >> 33
	h *= xxP2
	h ^= h >> 29
	h *= xxP3
	h ^= h >> 32
	return h
}

// Twox128 is Substrate's 128-bit xxHash: XXH64 with seeds 0 and 1, each
// little-endian. Pallet and storage item names are hashed with it.
func Twox128(b []byte) []byte {
	out := make([]byte, 16)
	binary.LittleEndian.PutUint64(out[0:], XXH64(b, 0))
	binary.LittleEndian.PutUint64(out[8:], XXH64(b, 1))
	return out
}

// Blake2_128Concat is the map hasher used for account keys: blake2b-128 of
// the key, followed by the key itself.
func Blake2_128Concat(b []byte) []byte {
	return append(Blake2b(b, 16), b...)
}
