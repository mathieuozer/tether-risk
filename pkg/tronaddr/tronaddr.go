// Package tronaddr converts TRON addresses between their base58check and
// 21-byte hex encodings. It depends on nothing else in this repository, so it
// can be used on its own (docs/INDEXER_PLAN.md).
package tronaddr

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// TRON addresses appear in two encodings depending on which endpoint returned
// them. TronGrid's TRC-20 endpoint gives base58check ("TR7NHq..."), while raw
// transaction data gives 21-byte hex with a 0x41 prefix ("41a614f8...").
//
// These are the same address. Storing both forms would split one address's
// flow across two rows in `edges`, halving its apparent value and understating
// its risk — a failure that would never raise an error. Everything is
// normalised to base58check, the form a user recognises and can paste into an
// explorer.

const (
	base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

	// mainnetPrefix is the 0x41 byte that precedes every TRON mainnet address.
	mainnetPrefix = 0x41

	addressLen = 21 // prefix + 20 bytes
)

var bigRadix = big.NewInt(58)

// HexToBase58 converts a 21-byte hex TRON address to base58check.
func HexToBase58(h string) (string, error) {
	h = strings.TrimPrefix(strings.TrimSpace(h), "0x")
	raw, err := hex.DecodeString(h)
	if err != nil {
		return "", fmt.Errorf("tron address %q is not hex: %w", h, err)
	}
	if len(raw) != addressLen {
		return "", fmt.Errorf("tron address %q has %d bytes, want %d", h, len(raw), addressLen)
	}
	if raw[0] != mainnetPrefix {
		return "", fmt.Errorf("tron address %q has prefix 0x%02x, want 0x41", h, raw[0])
	}
	return base58CheckEncode(raw), nil
}

// Base58ToHex converts a base58check TRON address to its 21-byte hex form.
func Base58ToHex(addr string) (string, error) {
	raw, err := base58CheckDecode(addr)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// Normalise returns the canonical base58check form of an address in either
// encoding. Empty input returns empty, so callers can pass through the
// "no counterparty" case without special-casing it.
func Normalise(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", nil
	}
	if strings.HasPrefix(addr, "T") && len(addr) == 34 {
		// Already base58check. Verify rather than trust: a corrupted address
		// that still looks plausible would poison the edge table.
		if _, err := base58CheckDecode(addr); err != nil {
			return "", err
		}
		return addr, nil
	}
	return HexToBase58(addr)
}

// IsValid reports whether an address is a well-formed TRON address in either
// encoding.
func IsValid(addr string) bool {
	_, err := Normalise(addr)
	return err == nil && addr != ""
}

func checksum(payload []byte) []byte {
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	return second[:4]
}

// Encode returns the base58check form of a 21-byte payload.
func Encode(payload []byte) string { return base58CheckEncode(payload) }

func base58CheckEncode(payload []byte) string {
	full := append(append([]byte{}, payload...), checksum(payload)...)
	return base58Encode(full)
}

// Decode returns the 21-byte payload of a base58check address.
func Decode(s string) ([]byte, error) { return base58CheckDecode(s) }

func base58CheckDecode(s string) ([]byte, error) {
	full, err := base58Decode(s)
	if err != nil {
		return nil, err
	}
	if len(full) != addressLen+4 {
		return nil, fmt.Errorf("tron address %q decodes to %d bytes, want %d", s, len(full), addressLen+4)
	}
	payload, sum := full[:addressLen], full[addressLen:]
	if !bytes.Equal(checksum(payload), sum) {
		return nil, fmt.Errorf("tron address %q has an invalid checksum", s)
	}
	if payload[0] != mainnetPrefix {
		return nil, fmt.Errorf("tron address %q has prefix 0x%02x, want 0x41", s, payload[0])
	}
	return payload, nil
}

func base58Encode(b []byte) string {
	x := new(big.Int).SetBytes(b)
	mod := new(big.Int)

	var out []byte
	for x.Sign() > 0 {
		x.DivMod(x, bigRadix, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}

	// Leading zero bytes are not represented by the big.Int, so they are
	// re-added as the alphabet's zero digit.
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, base58Alphabet[0])
	}

	// The digits were produced least-significant first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func base58Decode(s string) ([]byte, error) {
	x := big.NewInt(0)
	for _, r := range s {
		idx := strings.IndexRune(base58Alphabet, r)
		if idx < 0 {
			return nil, fmt.Errorf("tron address %q contains invalid base58 character %q", s, r)
		}
		x.Mul(x, bigRadix)
		x.Add(x, big.NewInt(int64(idx)))
	}

	decoded := x.Bytes()

	var leading int
	for _, r := range s {
		if r != rune(base58Alphabet[0]) {
			break
		}
		leading++
	}
	return append(make([]byte, leading), decoded...), nil
}
