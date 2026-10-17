package wallets

// Watch-only HD derivation (BIP32 public child derivation on secp256k1) and Kaspa address
// encoding. Only public keys are handled: an extended public key (xpub/kpub) can derive
// receive/change addresses but can never spend. Pure Go (math/big); no key material is logged.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

var (
	curveP, _  = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEFFFFFC2F", 16)
	curveN, _  = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)
	curveGx, _ = new(big.Int).SetString("79BE667EF9DCBBAC55A06295CE870B07029BFCDB2DCE28D959F2815B16F81798", 16)
	curveGy, _ = new(big.Int).SetString("483ADA7726A3C4655DA4FBFC0E1108A8FD17B448A68554199C47D08FFB10D4B8", 16)
)

type point struct{ x, y *big.Int } // nil x = point at infinity

func (p point) inf() bool { return p.x == nil }

func ptAdd(a, b point) point {
	if a.inf() {
		return b
	}
	if b.inf() {
		return a
	}
	P := curveP
	var l *big.Int
	if a.x.Cmp(b.x) == 0 {
		if new(big.Int).Mod(new(big.Int).Add(a.y, b.y), P).Sign() == 0 {
			return point{}
		}
		// l = 3x^2 / 2y
		num := new(big.Int).Mul(big.NewInt(3), new(big.Int).Mul(a.x, a.x))
		den := new(big.Int).ModInverse(new(big.Int).Mul(big.NewInt(2), a.y), P)
		l = new(big.Int).Mod(new(big.Int).Mul(num, den), P)
	} else {
		num := new(big.Int).Sub(b.y, a.y)
		den := new(big.Int).ModInverse(new(big.Int).Mod(new(big.Int).Sub(b.x, a.x), P), P)
		l = new(big.Int).Mod(new(big.Int).Mul(num, den), P)
	}
	x := new(big.Int).Mod(new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Mul(l, l), a.x), b.x), P)
	y := new(big.Int).Mod(new(big.Int).Sub(new(big.Int).Mul(l, new(big.Int).Sub(a.x, x)), a.y), P)
	return point{x, y}
}

func ptMul(k *big.Int, p point) point {
	r := point{}
	for i := k.BitLen() - 1; i >= 0; i-- {
		r = ptAdd(r, r)
		if k.Bit(i) == 1 {
			r = ptAdd(r, p)
		}
	}
	return r
}

func compress(p point) []byte {
	out := make([]byte, 33)
	out[0] = 2 + byte(p.y.Bit(0))
	p.x.FillBytes(out[1:])
	return out
}

func decompress(b []byte) (point, error) {
	if len(b) != 33 || (b[0] != 2 && b[0] != 3) {
		return point{}, errors.New("not a compressed public key")
	}
	x := new(big.Int).SetBytes(b[1:])
	if x.Cmp(curveP) >= 0 {
		return point{}, errors.New("public key x out of range")
	}
	y2 := new(big.Int).Add(new(big.Int).Exp(x, big.NewInt(3), curveP), big.NewInt(7))
	e := new(big.Int).Rsh(new(big.Int).Add(curveP, big.NewInt(1)), 2)
	y := new(big.Int).Exp(y2, e, curveP)
	if new(big.Int).Exp(y, big.NewInt(2), curveP).Cmp(new(big.Int).Mod(y2, curveP)) != 0 {
		return point{}, errors.New("public key not on curve")
	}
	if y.Bit(0) != uint(b[0]&1) {
		y.Sub(curveP, y)
	}
	return point{x, y}, nil
}

// ExtPub is a BIP32 extended public key.
type ExtPub struct {
	Version   [4]byte
	Depth     byte
	ChildNum  uint32
	ChainCode []byte
	Key       []byte // 33-byte compressed public key
}

const b58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Decode(s string) ([]byte, error) {
	n := new(big.Int)
	for _, c := range s {
		i := strings.IndexRune(b58, c)
		if i < 0 {
			return nil, fmt.Errorf("invalid base58 character")
		}
		n.Mul(n, big.NewInt(58)).Add(n, big.NewInt(int64(i)))
	}
	b := n.Bytes()
	lead := 0
	for lead < len(s) && s[lead] == '1' {
		lead++
	}
	return append(make([]byte, lead), b...), nil
}

// ParseExtPub decodes a base58check extended public key (xpub, kpub, ...). The version
// bytes are returned but not interpreted; private extended keys are rejected.
func ParseExtPub(s string) (*ExtPub, error) {
	raw, err := base58Decode(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	if len(raw) != 82 {
		return nil, fmt.Errorf("extended key: %d bytes, want 82", len(raw))
	}
	h1 := sha256.Sum256(raw[:78])
	h2 := sha256.Sum256(h1[:])
	if !bytes.Equal(h2[:4], raw[78:]) {
		return nil, errors.New("extended key: bad checksum")
	}
	if raw[45] == 0 {
		return nil, errors.New("extended key is private; only a public (watch-only) key is accepted")
	}
	k := &ExtPub{Depth: raw[4], ChildNum: binary.BigEndian.Uint32(raw[9:13]), ChainCode: raw[13:45], Key: raw[45:78]}
	copy(k.Version[:], raw[:4])
	if _, err := decompress(k.Key); err != nil {
		return nil, fmt.Errorf("extended key: %w", err)
	}
	return k, nil
}

// Child derives the non-hardened child i (BIP32 CKDpub).
func (k *ExtPub) Child(i uint32) (*ExtPub, error) {
	if i >= 0x80000000 {
		return nil, errors.New("cannot derive a hardened child from a public key")
	}
	mac := hmac.New(sha512.New, k.ChainCode)
	mac.Write(k.Key)
	var ib [4]byte
	binary.BigEndian.PutUint32(ib[:], i)
	mac.Write(ib[:])
	I := mac.Sum(nil)
	il := new(big.Int).SetBytes(I[:32])
	if il.Cmp(curveN) >= 0 {
		return nil, errors.New("invalid child (IL >= n)")
	}
	parent, err := decompress(k.Key)
	if err != nil {
		return nil, err
	}
	c := ptAdd(ptMul(il, point{curveGx, curveGy}), parent)
	if c.inf() {
		return nil, errors.New("invalid child (point at infinity)")
	}
	return &ExtPub{Version: k.Version, Depth: k.Depth + 1, ChildNum: i, ChainCode: I[32:], Key: compress(c)}, nil
}

// ---------- Kaspa addresses (CashAddr-style bech32, prefix "kaspa") ----------

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// Kaspa address versions.
const (
	KaspaPubKey      byte = 0 // Schnorr, 32-byte x-only public key (most software wallets)
	KaspaPubKeyECDSA byte = 1 // 33-byte compressed ECDSA public key (some hardware wallets)
	KaspaScriptHash  byte = 8
)

func kaspaPolymod(values []byte) uint64 {
	gen := []uint64{0x98f2bc8e61, 0x79b76d99e2, 0xf33e5fb3c4, 0xae2eabe2a8, 0x1e4f43e470}
	c := uint64(1)
	for _, d := range values {
		c0 := c >> 35
		c = ((c & 0x07ffffffff) << 5) ^ uint64(d)
		for i := 0; i < 5; i++ {
			if (c0>>uint(i))&1 == 1 {
				c ^= gen[i]
			}
		}
	}
	return c ^ 1
}

func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	var acc, bits uint
	var out []byte
	maxv := uint(1)<<to - 1
	for _, v := range data {
		acc = acc<<from | uint(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxv))
		}
	} else if bits >= from || acc<<(to-bits)&maxv != 0 {
		return nil, errors.New("invalid padding")
	}
	return out, nil
}

func checksumInput(prefix string, payload5 []byte) []byte {
	in := make([]byte, 0, len(prefix)+1+len(payload5)+8)
	for i := 0; i < len(prefix); i++ {
		in = append(in, prefix[i]&0x1f)
	}
	in = append(in, 0)
	in = append(in, payload5...)
	return append(in, make([]byte, 8)...)
}

// KaspaAddress encodes version + payload as a Kaspa address with the given prefix ("kaspa").
func KaspaAddress(prefix string, version byte, payload []byte) string {
	p5, _ := convertBits(append([]byte{version}, payload...), 8, 5, true)
	cs := kaspaPolymod(checksumInput(prefix, p5))
	var sb strings.Builder
	sb.WriteString(prefix + ":")
	for _, v := range p5 {
		sb.WriteByte(bech32Charset[v])
	}
	for i := 7; i >= 0; i-- {
		sb.WriteByte(bech32Charset[(cs>>(5*uint(i)))&0x1f])
	}
	return sb.String()
}

// DecodeKaspaAddress validates the checksum and returns the version and payload.
func DecodeKaspaAddress(addr string) (prefix string, version byte, payload []byte, err error) {
	prefix, rest, ok := strings.Cut(strings.ToLower(addr), ":")
	if !ok || len(rest) < 9 {
		return "", 0, nil, errors.New("kaspa address: missing prefix or too short")
	}
	vals := make([]byte, len(rest))
	for i := 0; i < len(rest); i++ {
		j := strings.IndexByte(bech32Charset, rest[i])
		if j < 0 {
			return "", 0, nil, errors.New("kaspa address: invalid character")
		}
		vals[i] = byte(j)
	}
	p5, csv := vals[:len(vals)-8], vals[len(vals)-8:]
	var cs uint64
	for _, v := range csv {
		cs = cs<<5 | uint64(v)
	}
	if kaspaPolymod(checksumInput(prefix, p5)) != cs {
		return "", 0, nil, errors.New("kaspa address: bad checksum")
	}
	b, err := convertBits(p5, 5, 8, false)
	if err != nil || len(b) < 1 {
		return "", 0, nil, errors.New("kaspa address: bad payload")
	}
	return prefix, b[0], b[1:], nil
}

// KaspaSchnorrAddress is the P2PK (Schnorr, x-only) address of a compressed public key.
func KaspaSchnorrAddress(prefix string, compressed []byte) string {
	return KaspaAddress(prefix, KaspaPubKey, compressed[1:])
}
