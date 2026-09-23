package indexer

import (
	"testing"

	"github.com/mozer/tether-risk/internal/chain/tron"
)

// Two real events, as the indexer parsed them from blocks 86482708 and
// 86299832, and the results TronGrid's event API gives for the same
// transactions.
func TestDecodeBlacklistMatchesTronGrid(t *testing.T) {
	b58 := func(hex string) string {
		a, err := tron.HexToBase58("41" + hex)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	cases := []struct {
		name   string
		topics []string
		data   string
		want   map[string]string
	}{
		{"AddedBlackList", // f47f23c9…e9b3
			[]string{"42e160154868087d6bfdc0ca23d96a1c1cfa32f1b72ba9ba27b69b98a0d819dc",
				"0000000000000000000000006f566c6d608550fb50c9365bdb6665e1c8e53caa"}, "",
			map[string]string{"_user": b58("6f566c6d608550fb50c9365bdb6665e1c8e53caa")}},
		{"DestroyedBlackFunds", // 9f4753c5…15c3b
			[]string{"61e6e66b0d6339b2980aecc6ccc0039736791f0ccde9ed512e789a7fbdd698c6",
				"000000000000000000000000c3f11bafbee0c8cfafd5a76bb10570e26dd36b40"},
			"0000000000000000000000000000000000000000000000000000000415203ee3",
			map[string]string{"_blackListedUser": b58("c3f11bafbee0c8cfafd5a76bb10570e26dd36b40"), "_balance": "17534303971"}},
	}
	for _, c := range cases {
		got, ok := DecodeBlacklist(c.name, c.topics, c.data)
		if !ok {
			t.Fatalf("%s not decoded", c.name)
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s %s = %q, want %q", c.name, k, got[k], v)
			}
		}
	}
	if _, ok := DecodeBlacklist("AddedBlackList", []string{"42e1…"}, ""); ok {
		t.Error("an event without its address topic decoded")
	}
}
