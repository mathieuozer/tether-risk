package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mozer/tether-risk/internal/scoring"
)

// Checking a payment before it is sent (docs/DECISIONS.md D34).

type preSendRequest struct {
	Chain string `json:"chain"`
	// From is the paying wallet. Optional: without it only the recipient is
	// screened.
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

type preSendResponse struct {
	// Decision is "do_not_send" or "send": do not send when the recipient
	// is risky or imitates one of From's counterparties. A first payment alone
	// does not decide it.
	Decision string `json:"decision"`

	// LookalikeOf is a real counterparty of From that To imitates: To is
	// almost certainly an address-poisoning copy.
	LookalikeOf  string  `json:"lookalike_of,omitempty"`
	LookalikeUSD float64 `json:"lookalike_usd,omitempty"`

	PaidBeforeUSD float64 `json:"paid_before_usd"`
	FirstPayment  bool    `json:"first_payment"`
	// SenderKnown is false when From was not given or its history could not
	// be read; then the look-alike and first-payment checks did not run.
	SenderKnown bool `json:"sender_known"`

	Recipient screenResponse `json:"recipient"`
}

func (s *server) handlePreSend(w http.ResponseWriter, r *http.Request) {
	var req preSendRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Chain == "" {
		req.Chain = "tron"
	}
	req.From, req.To = strings.TrimSpace(req.From), strings.TrimSpace(req.To)
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "to is required")
		return
	}
	ps, err := s.svc.PreSend(r.Context(), req.Chain, req.From, req.To)
	if err != nil {
		if strings.HasPrefix(err.Error(), "chain_unavailable") {
			writeError(w, http.StatusServiceUnavailable, "chain_unavailable", err.Error())
			return
		}
		s.log.Error("presend failed", "to", req.To, "error", err)
		writeError(w, http.StatusInternalServerError, "presend_failed", err.Error())
		return
	}
	out := preSendResponse{
		Decision:      "send",
		LookalikeOf:   ps.LookalikeOf,
		LookalikeUSD:  ps.LookalikeUSD,
		PaidBeforeUSD: ps.PaidBefore,
		FirstPayment:  ps.FirstPayment,
		SenderKnown:   ps.SenderKnown,
		Recipient:     toResponse(ps.Recipient),
	}
	if ps.LookalikeOf != "" || (ps.Recipient.Verdict != nil && ps.Recipient.Verdict.Level == scoring.VerdictHighRisk) {
		out.Decision = "do_not_send"
	}
	writeJSON(w, http.StatusOK, out)
}
