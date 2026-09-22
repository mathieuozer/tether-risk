package screen

import "testing"

func TestLookalikeKeyMatchesPoisonedCopy(t *testing.T) {
	real := "TBkgikXXXXXXXXXXXXXXXXXXXXXXXXEtN8"
	copy := "TBkgYYYYYYYYYYYYYYYYYYYYYYYYYYEtN8"
	other := "TBkgikXXXXXXXXXXXXXXXXXXXXXXXXEtN9"
	if lookalikeKey(real) != lookalikeKey(copy) {
		t.Fatalf("a copy with the same first and last four characters must share the key")
	}
	if lookalikeKey(real) == lookalikeKey(other) {
		t.Fatalf("a different last character must not share the key")
	}
}
