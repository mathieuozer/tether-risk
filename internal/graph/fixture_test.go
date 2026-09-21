package graph

import (
	"context"
	"sort"

	"github.com/mozer/tether-risk/internal/labels"
	"github.com/shopspring/decimal"
)

// In-memory edge and label sources. SPEC.md §9 requires golden fixtures so
// traversal and scoring are testable without network access.

type fakeEdges struct {
	// out[address] and in[address] hold the neighbours in each direction.
	out map[string][]Neighbour
	in  map[string][]Neighbour
}

func newFakeEdges() *fakeEdges {
	return &fakeEdges{out: map[string][]Neighbour{}, in: map[string][]Neighbour{}}
}

// addEdge records a flow from -> to, visible in both directions.
func (f *fakeEdges) addEdge(from, to string, usd float64, transfers uint64) *fakeEdges {
	n := Neighbour{Address: to, USDValue: decimal.NewFromFloat(usd), TransferCount: transfers}
	f.out[from] = append(f.out[from], n)

	back := Neighbour{Address: from, USDValue: decimal.NewFromFloat(usd), TransferCount: transfers}
	f.in[to] = append(f.in[to], back)
	return f
}

func (f *fakeEdges) Neighbours(_ context.Context, _ string, address string, dir Direction) ([]Neighbour, error) {
	var src map[string][]Neighbour
	if dir == Outbound {
		src = f.out
	} else {
		src = f.in
	}
	// Returned in a deliberately unhelpful order, so a traversal that depends
	// on source ordering fails its determinism test rather than passing by
	// luck.
	out := append([]Neighbour(nil), src[address]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Address > out[j].Address })
	return out, nil
}

type fakeLabels struct {
	byAddress map[string]labels.Resolution
}

func newFakeLabels() *fakeLabels {
	return &fakeLabels{byAddress: map[string]labels.Resolution{}}
}

func (f *fakeLabels) add(address, entity, category, source string, confidence float64) *fakeLabels {
	f.byAddress[address] = labels.Resolution{
		Address: address, Entity: entity, Category: category,
		Source: source, Confidence: confidence, Labelled: true,
	}
	return f
}

func (f *fakeLabels) Lookup(_ context.Context, _ string, addresses []string) (map[string]labels.Resolution, error) {
	out := map[string]labels.Resolution{}
	for _, a := range addresses {
		if r, ok := f.byAddress[a]; ok {
			out[a] = r
		}
	}
	return out, nil
}
