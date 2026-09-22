// render.go: the screening API client and how its answers are formatted.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mozer/tether-risk/internal/report"
)

// screenResponse mirrors the API's response. Deliberately a separate decode
// rather than an import: the bot consumes the API as any other client would,
// so a breaking change to the response shows up here as a decode failure
// rather than being papered over by shared types.
type screenResponse struct {
	Address string  `json:"address"`
	Chain   string  `json:"chain"`
	Score   float64 `json:"score"`
	Band    string  `json:"band"`

	Coverage      float64 `json:"coverage"`
	LowConfidence bool    `json:"low_confidence"`

	SanctionsOverride bool `json:"sanctions_override"`
	BandCappedByAbuse bool `json:"band_capped_by_abuse_rule"`

	Inbound  *direction `json:"inbound"`
	Outbound *direction `json:"outbound"`

	Verdict *struct {
		Level            string `json:"level"`
		Confidence       string `json:"confidence"`
		ConfidencePct    int    `json:"confidence_pct"`
		InsufficientData bool   `json:"insufficient_data"`
		Reasons          []struct {
			Code     string  `json:"code"`
			Category string  `json:"category"`
			Pct      float64 `json:"pct"`
			Flag     string  `json:"flag"`
		} `json:"reasons"`
	} `json:"verdict"`

	Flags []struct {
		Code      string  `json:"code"`
		InUSD     float64 `json:"in_usd"`
		OutUSD    float64 `json:"out_usd"`
		VolumeUSD float64 `json:"volume_usd"`
		Days      int     `json:"days"`
		AgeDays   int     `json:"age_days"`
		Count     int     `json:"count"`
		AmountUSD float64 `json:"amount_usd"`
		Minutes   int     `json:"minutes"`
	} `json:"flags"`

	OwnLabel *struct {
		Entity   string `json:"entity"`
		Category string `json:"category"`
	} `json:"own_label"`

	Activity *struct {
		InUSD             float64 `json:"in_usd"`
		OutUSD            float64 `json:"out_usd"`
		InTransfers       uint64  `json:"in_transfers"`
		OutTransfers      uint64  `json:"out_transfers"`
		InCounterparties  uint64  `json:"in_counterparties"`
		OutCounterparties uint64  `json:"out_counterparties"`
		FirstSeen         string  `json:"first_seen"`
		LastSeen          string  `json:"last_seen"`
		Assets            []struct {
			Asset  string  `json:"asset"`
			InUSD  float64 `json:"in_usd"`
			OutUSD float64 `json:"out_usd"`
		} `json:"assets"`
		UnpricedTransfers uint64 `json:"unpriced_transfers"`
		UnpricedTokens    uint64 `json:"unpriced_tokens"`
	} `json:"activity"`

	Depth *struct {
		FetchError          string `json:"fetch_error"`
		StillFetching       bool   `json:"still_fetching"`
		FrontierPending     int    `json:"frontier_pending"`
		FrontierQueued      int    `json:"frontier_queued"`
		HistoryTruncated    bool   `json:"history_truncated"`
		Counterparties      int    `json:"counterparties"`
		Traced              int    `json:"traced"`
		TotalCounterparties int    `json:"total_counterparties"`
	} `json:"depth"`

	LabelSnapshotID int64  `json:"label_snapshot_id"`
	ConfigVersion   string `json:"config_version"`
	Disclaimer      string `json:"disclaimer"`

	Error  string `json:"error"`
	Detail string `json:"detail"`
}

type direction struct {
	Score           float64 `json:"score"`
	Coverage        float64 `json:"coverage"`
	UnattributedPct float64 `json:"unattributed_pct"`
	TotalTraced     float64 `json:"total_traced"`
	Categories      []struct {
		Category string  `json:"category"`
		Pct      float64 `json:"pct"`
	} `json:"categories"`
	TopPaths []struct {
		Explanation string `json:"explanation"`
	} `json:"top_paths"`
	Connections []struct {
		Address  string  `json:"address"`
		Entity   string  `json:"entity"`
		Category string  `json:"category"`
		Pct      float64 `json:"pct"`
		MinHops  int     `json:"min_hops"`
		Profile  *struct {
			VolumeUSD      float64  `json:"volume_usd"`
			Transfers      uint64   `json:"transfers"`
			Counterparties uint64   `json:"counterparties"`
			FirstSeen      string   `json:"first_seen"`
			LastSeen       string   `json:"last_seen"`
			Assets         []string `json:"assets"`
			Partial        bool     `json:"partial"`
		} `json:"profile"`
	} `json:"connections"`
	UnattributedReasons []struct {
		Reason string  `json:"reason"`
		Pct    float64 `json:"pct"`
	} `json:"unattributed_reasons"`
	Traversal struct {
		FanoutCapped    bool `json:"fanout_capped"`
		HopLimitReached bool `json:"hop_limit_reached"`
	} `json:"traversal"`
}

// screen runs a screen through the internal API. It returns the decoded
// result and the raw JSON, which the app passes through unchanged.
func (b *bot) screen(ctx context.Context, chain, address string) (*screenResponse, []byte, error) {
	body, err := json.Marshal(map[string]string{"chain": chain, "address": address})
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiURL+"/v1/screen", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("screening service unreachable: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, err
	}

	var out screenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, fmt.Errorf("unexpected response from the screening service: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Detail != "" {
			return nil, nil, fmt.Errorf("%s", out.Detail)
		}
		return nil, nil, fmt.Errorf("screening failed (%d)", resp.StatusCode)
	}
	return &out, raw, nil
}

func format(r *screenResponse) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Address: %s\nChain: %s\n\n", r.Address, r.Chain)
	fmt.Fprintf(&b, "Risk band: %s\nScore: %.1f / 100\n", strings.ToUpper(r.Band), r.Score)

	// Coverage is never separated from the score. A score without it is not
	// actionable, and in a chat window a reader will take whatever is on the
	// line above.
	fmt.Fprintf(&b, "Coverage: %.1f%%\n", r.Coverage*100)

	if l := r.OwnLabel; l != nil {
		fmt.Fprintf(&b, "\n*** THIS ADDRESS IS DIRECTLY LISTED ***\n%s (%s)\n", l.Entity, l.Category)
	}
	if r.SanctionsOverride {
		b.WriteString("\n*** DIRECT SANCTIONS MATCH ***\n" +
			"This address is on a sanctions list. The band is High regardless of score.\n")
	}
	if r.LowConfidence {
		fmt.Fprintf(&b, "\n*** LOW CONFIDENCE ***\n"+
			"Only %.1f%% of traced value reached a known entity. The remaining %.1f%%\n"+
			"is unknown, not clean.\n", r.Coverage*100, 100-r.Coverage*100)
	}
	if r.BandCappedByAbuse {
		b.WriteString("\nBand capped: the only evidence is unverified abuse reports.\n")
	}

	writeDirection(&b, "Inbound (where funds came from)", r.Inbound)
	writeDirection(&b, "Outbound (where funds went)", r.Outbound)

	fmt.Fprintf(&b, "\nLabel snapshot %d | config %s\n", r.LabelSnapshotID, r.ConfigVersion)
	if r.Disclaimer != "" {
		fmt.Fprintf(&b, "\n%s", r.Disclaimer)
	}
	return b.String()
}

// summary renders the compact connections list in the user's language.
// followUp says tracing continues in the background and the final result
// will be sent, instead of asking the reader to screen again.
func summary(r *screenResponse, lang string, followUp bool) string {
	in := report.ConnectionsInput{
		Lang:              lang,
		Address:           r.Address,
		Chain:             r.Chain,
		Score:             r.Score,
		Band:              r.Band,
		Coverage:          r.Coverage,
		LowConfidence:     r.LowConfidence,
		SanctionsOverride: r.SanctionsOverride,
		BandCappedByAbuse: r.BandCappedByAbuse,
		Inbound:           summaryDirection(r.Inbound),
		Outbound:          summaryDirection(r.Outbound),
		Disclaimer:        r.Disclaimer,
	}
	for _, f := range r.Flags {
		in.Flags = append(in.Flags, report.ConnectionsFlag{Code: f.Code, InUSD: f.InUSD, OutUSD: f.OutUSD,
			VolumeUSD: f.VolumeUSD, Days: f.Days, AgeDays: f.AgeDays, Count: f.Count, AmountUSD: f.AmountUSD, Minutes: f.Minutes})
	}
	if v := r.Verdict; v != nil {
		cv := &report.ConnectionsVerdict{Level: v.Level, Confidence: v.Confidence, ConfidencePct: v.ConfidencePct, Insufficient: v.InsufficientData}
		for _, x := range v.Reasons {
			cv.Reasons = append(cv.Reasons, report.ConnectionsVerdictReason{Code: x.Code, Category: x.Category, Flag: x.Flag, Pct: x.Pct})
		}
		in.Verdict = cv
	}
	if r.OwnLabel != nil {
		in.OwnLabel = &report.ConnectionsOwnLabel{Entity: r.OwnLabel.Entity, Category: r.OwnLabel.Category}
	}
	if d := r.Depth; d != nil {
		in.Depth = &report.ConnectionsDepth{
			FetchError: d.FetchError, StillFetching: d.StillFetching, HistoryTruncated: d.HistoryTruncated,
			FrontierPending: d.FrontierPending, FrontierQueued: d.FrontierQueued, FollowUp: followUp,
			Counterparties: d.Counterparties,
			Traced:         d.Traced, TotalCounterparties: d.TotalCounterparties,
		}
	}
	if a := r.Activity; a != nil {
		act := &report.ConnectionsActivity{
			InUSD: a.InUSD, OutUSD: a.OutUSD,
			InTransfers: a.InTransfers, OutTransfers: a.OutTransfers,
			InCounterparties: a.InCounterparties, OutCounterparties: a.OutCounterparties,
			FirstSeen: a.FirstSeen, LastSeen: a.LastSeen,
			UnpricedTransfers: a.UnpricedTransfers, UnpricedTokens: a.UnpricedTokens,
		}
		for _, as := range a.Assets {
			act.Assets = append(act.Assets, report.ConnectionsAsset{Asset: as.Asset, USD: as.InUSD + as.OutUSD})
		}
		in.Activity = act
	}
	return report.Connections(in)
}

func summaryDirection(d *direction) *report.ConnectionsDirection {
	if d == nil {
		return nil
	}
	out := &report.ConnectionsDirection{
		TracedWeight:    d.TotalTraced,
		UnattributedPct: d.UnattributedPct,
		FanoutCapped:    d.Traversal.FanoutCapped,
		HopLimitReached: d.Traversal.HopLimitReached,
	}
	for _, c := range d.Categories {
		out.Categories = append(out.Categories, report.ConnectionsCategory{Category: c.Category, Pct: c.Pct})
	}
	for _, c := range d.Connections {
		e := report.ConnectionsEntry{
			Address: c.Address, Entity: c.Entity, Category: c.Category, Pct: c.Pct, MinHops: c.MinHops,
		}
		if p := c.Profile; p != nil {
			e.Profile = &report.ConnectionsProfile{
				VolumeUSD: p.VolumeUSD, Transfers: p.Transfers, Counterparties: p.Counterparties,
				FirstSeen: p.FirstSeen, LastSeen: p.LastSeen, Assets: p.Assets, Partial: p.Partial,
			}
		}
		out.Entries = append(out.Entries, e)
	}
	for _, rs := range d.UnattributedReasons {
		out.Reasons = append(out.Reasons, report.ConnectionsReason{Reason: rs.Reason, Pct: rs.Pct})
	}
	return out
}

func writeDirection(b *strings.Builder, title string, d *direction) {
	if d == nil {
		return
	}
	fmt.Fprintf(b, "\n%s\n", title)

	if len(d.Categories) == 0 && d.UnattributedPct == 0 {
		b.WriteString("  no traced value\n")
		return
	}
	for _, c := range d.Categories {
		fmt.Fprintf(b, "  %-20s %6.2f%%\n", c.Category, c.Pct)
	}
	if d.UnattributedPct > 0 {
		fmt.Fprintf(b, "  %-20s %6.2f%%  (unknown)\n", "unattributed", d.UnattributedPct)
	}
	if d.Traversal.FanoutCapped {
		b.WriteString("  note: truncated at the neighbour cap\n")
	}
	if d.Traversal.HopLimitReached {
		b.WriteString("  note: hop limit reached\n")
	}
	for i, p := range d.TopPaths {
		if i == 0 {
			b.WriteString("  top paths:\n")
		}
		if i >= 3 {
			break
		}
		fmt.Fprintf(b, "    - %s\n", p.Explanation)
	}
}
