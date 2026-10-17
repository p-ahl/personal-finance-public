package wallets

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// BIP32 test vector 1: m/0H/1/2H (public) -> m/0H/1/2H/2 (non-hardened public derivation).
func TestCKDpubVector(t *testing.T) {
	parent, err := ParseExtPub("xpub6D4BDPcP2GT577Vvch3R8wDkScZWzQzMMUm3PWbmWvVJrZwQY4VUNgqFJPMM3No2dFDFGTsxxpG5uJh7n7epu4trkrX7x7DogT5Uv6fcLW5")
	if err != nil {
		t.Fatal(err)
	}
	c, err := parent.Child(2)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := ParseExtPub("xpub6FHa3pjLCk84BayeJxFW2SP4XRrFd1JYnxeLeU8EqN3vDfZmbqBqaGJAyiLjTAwm6ZLRQUMv1ZACTj37sR62cfN7fe5JnJ7dh8zL4fiyLHV")
	if !bytes.Equal(c.Key, want.Key) || !bytes.Equal(c.ChainCode, want.ChainCode) {
		t.Fatalf("child mismatch: %x", c.Key)
	}
	if _, err := ParseExtPub("xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi"); err == nil {
		t.Fatal("private extended key must be rejected")
	}
}

func TestKaspaAddressRoundTrip(t *testing.T) {
	// all-zero Schnorr key (well-known test address from the Kaspa address tests)
	a := KaspaAddress("kaspa", KaspaPubKey, make([]byte, 32))
	if a != "kaspa:qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqkx9awp4e" {
		t.Fatalf("zero address: %s", a)
	}
	pub, _ := hex.DecodeString("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	for _, v := range []byte{KaspaPubKey, KaspaPubKeyECDSA} {
		payload := pub
		if v == KaspaPubKey {
			payload = pub[1:]
		}
		s := KaspaAddress("kaspa", v, payload)
		p, gv, gp, err := DecodeKaspaAddress(s)
		if err != nil || p != "kaspa" || gv != v || !bytes.Equal(gp, payload) {
			t.Fatalf("round trip %d: %s %v", v, s, err)
		}
		bad := []byte(s)
		bad[len(bad)-1] ^= 1
		if _, _, _, err := DecodeKaspaAddress(string(bad)); err == nil {
			t.Fatal("corrupted address accepted")
		}
	}
}
