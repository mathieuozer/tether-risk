package tron

import "github.com/mozer/tether-risk/pkg/tronaddr"

// TRON addresses appear in two encodings depending on which endpoint returned
// them: base58check ("TR7NHq...") and 21-byte hex ("41a614f8..."). Everything
// is normalised to base58check (see pkg/tronaddr, which the indexer shares).

// HexToBase58 converts a 21-byte hex TRON address to base58check.
func HexToBase58(h string) (string, error) { return tronaddr.HexToBase58(h) }

// Base58ToHex converts a base58check TRON address to its 21-byte hex form.
func Base58ToHex(addr string) (string, error) { return tronaddr.Base58ToHex(addr) }

// Normalise returns the canonical base58check form of an address in either
// encoding; empty input returns empty.
func Normalise(addr string) (string, error) { return tronaddr.Normalise(addr) }

// IsValid reports whether an address is a well-formed TRON address in either
// encoding.
func IsValid(addr string) bool { return tronaddr.IsValid(addr) }
