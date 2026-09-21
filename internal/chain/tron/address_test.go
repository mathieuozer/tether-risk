package tron

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"testing"
)

// Known-good pairs, each externally verifiable against a block explorer.
//
// Only vectors that can be checked against a published source belong here. An
// earlier revision of this file carried a second, hand-written pair whose two
// halves were not in fact the same address; it failed, and it was the vector
// that was wrong rather than the codec. Invented constants in a test are worse
// than no test, because they look like evidence. Coverage beyond these pairs
// comes from the round-trip property test below, which needs no vectors at all.
var knownPairs = []struct {
	base58 string
	hex    string
	note   string
}{
	{
		base58: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		hex:    "41a614f803b6fd780986a42c78ec9c7f77e6ded13c",
		note:   "USDT TRC-20 contract",
	},
}

func TestHexToBase58RoundTrip(t *testing.T) {
	for _, p := range knownPairs {
		got, err := HexToBase58(p.hex)
		if err != nil {
			t.Errorf("%s: HexToBase58(%s): %v", p.note, p.hex, err)
			continue
		}
		if got != p.base58 {
			t.Errorf("%s: HexToBase58(%s) = %s, want %s", p.note, p.hex, got, p.base58)
		}

		back, err := Base58ToHex(p.base58)
		if err != nil {
			t.Errorf("%s: Base58ToHex(%s): %v", p.note, p.base58, err)
			continue
		}
		if back != p.hex {
			t.Errorf("%s: Base58ToHex(%s) = %s, want %s", p.note, p.base58, back, p.hex)
		}
	}
}

// The reason this package exists: the same address arriving in two encodings
// must collapse to one, or its flow splits across two rows in `edges` and its
// value — and therefore its risk — is understated with no error raised.
func TestNormaliseIsIdempotentAcrossEncodings(t *testing.T) {
	for _, p := range knownPairs {
		fromHex, err := Normalise(p.hex)
		if err != nil {
			t.Fatalf("%s: normalise hex: %v", p.note, err)
		}
		fromB58, err := Normalise(p.base58)
		if err != nil {
			t.Fatalf("%s: normalise base58: %v", p.note, err)
		}
		if fromHex != fromB58 {
			t.Errorf("%s: encodings disagree: hex gave %s, base58 gave %s; "+
				"edges would split across both", p.note, fromHex, fromB58)
		}
		if fromHex != p.base58 {
			t.Errorf("%s: canonical form = %s, want base58 %s", p.note, fromHex, p.base58)
		}

		// Normalising twice must not change anything.
		twice, err := Normalise(fromHex)
		if err != nil || twice != fromHex {
			t.Errorf("%s: Normalise is not idempotent: %s then %s (%v)", p.note, fromHex, twice, err)
		}
	}
}

func TestNormaliseHandlesPrefixAndWhitespace(t *testing.T) {
	want := knownPairs[0].base58
	for _, in := range []string{
		"41a614f803b6fd780986a42c78ec9c7f77e6ded13c",
		"0x41a614f803b6fd780986a42c78ec9c7f77e6ded13c",
		"  41a614f803b6fd780986a42c78ec9c7f77e6ded13c  ",
		"  TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t ",
	} {
		got, err := Normalise(in)
		if err != nil {
			t.Errorf("Normalise(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Normalise(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestNormaliseEmptyIsEmpty(t *testing.T) {
	got, err := Normalise("")
	if err != nil || got != "" {
		t.Errorf(`Normalise("") = %q, %v; want "", nil`, got, err)
	}
}

// A corrupted address that still looks plausible must be rejected rather than
// stored. A bad address in `edges` is untraceable once it is there.
func TestNormaliseRejectsCorruption(t *testing.T) {
	cases := []struct {
		in  string
		why string
	}{
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6u", "last character altered, checksum fails"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj60", "contains '0', not in the base58 alphabet"},
		{"41a614f803b6fd780986a42c78ec9c7f77e6ded1", "20 bytes, one short"},
		{"42a614f803b6fd780986a42c78ec9c7f77e6ded13c", "wrong prefix byte"},
		{"not-hex-at-all", "not an address in any encoding"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6", "33 characters, truncated"},
	}
	for _, tc := range cases {
		if _, err := Normalise(tc.in); err == nil {
			t.Errorf("Normalise(%q) succeeded; expected rejection: %s", tc.in, tc.why)
		}
		if IsValid(tc.in) {
			t.Errorf("IsValid(%q) = true; expected false: %s", tc.in, tc.why)
		}
	}
}

func TestIsValidAcceptsGoodAddresses(t *testing.T) {
	for _, p := range knownPairs {
		if !IsValid(p.base58) {
			t.Errorf("IsValid(%s) = false, want true (%s)", p.base58, p.note)
		}
		if !IsValid(p.hex) {
			t.Errorf("IsValid(%s) = false, want true (%s)", p.hex, p.note)
		}
	}
	if IsValid("") {
		t.Error(`IsValid("") = true, want false`)
	}
}

// Round-trip property over the whole address space. This needs no external
// vectors: it asserts that encoding and decoding are exact inverses for every
// payload, which is the property the edge table actually depends on.
func TestRoundTripIsLossless(t *testing.T) {
	// Deterministic pseudo-random payloads: a fixed seed keeps failures
	// reproducible, which matters more here than fresh randomness per run.
	rng := rand.New(rand.NewSource(1))

	for i := 0; i < 2000; i++ {
		payload := make([]byte, addressLen)
		payload[0] = mainnetPrefix
		rng.Read(payload[1:])

		b58 := base58CheckEncode(payload)

		decoded, err := base58CheckDecode(b58)
		if err != nil {
			t.Fatalf("iteration %d: decode(encode(%x)) failed: %v", i, payload, err)
		}
		if !bytes.Equal(decoded, payload) {
			t.Fatalf("iteration %d: round trip changed the address: %x -> %s -> %x",
				i, payload, b58, decoded)
		}

		// And through the public API, in both encodings.
		hexForm := hex.EncodeToString(payload)
		viaHex, err := Normalise(hexForm)
		if err != nil {
			t.Fatalf("iteration %d: Normalise(%s): %v", i, hexForm, err)
		}
		viaB58, err := Normalise(b58)
		if err != nil {
			t.Fatalf("iteration %d: Normalise(%s): %v", i, b58, err)
		}
		if viaHex != viaB58 {
			t.Fatalf("iteration %d: encodings disagree: %s vs %s", i, viaHex, viaB58)
		}
	}
}

// Every mainnet address encodes to 34 characters starting with 'T'. A codec
// that quietly produced a shorter string for some payloads would break the
// length check in Normalise for exactly those addresses.
func TestEncodedAddressesAreAlwaysWellFormed(t *testing.T) {
	rng := rand.New(rand.NewSource(2))

	for i := 0; i < 2000; i++ {
		payload := make([]byte, addressLen)
		payload[0] = mainnetPrefix
		rng.Read(payload[1:])

		b58 := base58CheckEncode(payload)
		if len(b58) != 34 {
			t.Fatalf("iteration %d: encoded %x to %q (%d chars), want 34",
				i, payload, b58, len(b58))
		}
		if b58[0] != 'T' {
			t.Fatalf("iteration %d: encoded %x to %q, want a leading 'T'", i, payload, b58)
		}
		if !IsValid(b58) {
			t.Fatalf("iteration %d: own output %q failed IsValid", i, b58)
		}
	}
}

// Flipping any single character of a valid address must be caught by the
// checksum. This is what stops a corrupted address reaching `edges`, where it
// would be untraceable.
func TestSingleCharacterCorruptionIsCaught(t *testing.T) {
	valid := knownPairs[0].base58

	var missed int
	for i := 0; i < len(valid); i++ {
		for _, repl := range base58Alphabet {
			if byte(repl) == valid[i] {
				continue
			}
			corrupted := valid[:i] + string(repl) + valid[i+1:]
			if IsValid(corrupted) {
				missed++
				t.Errorf("corruption at index %d (%c -> %c) was not caught: %s",
					i, valid[i], repl, corrupted)
			}
		}
	}
	if missed > 0 {
		t.Errorf("%d single-character corruptions passed validation", missed)
	}
}
