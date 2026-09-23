package indexer

import (
	"context"
	"database/sql"
	"math/big"
	"strings"

	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/pkg/tronaddr"
)

// DecodeBlacklist reads one of Tether's blacklist events into the result
// TronGrid's event API gives for it, so both sources fold the same way
// (labels.TetherBlacklist). The address is indexed: it is the topic after
// the signature, not the data. DestroyedBlackFunds carries the balance
// destroyed in its data. Checked against real events on 2026-09-23:
// AddedBlackList in f47f23…e9b3, DestroyedBlackFunds in 9f4753…15c3b.
func DecodeBlacklist(name string, topics []string, data string) (map[string]string, bool) {
	if len(topics) < 2 {
		return nil, false
	}
	addr := topicAddress(topics[1])
	if addr == "" {
		return nil, false
	}
	switch name {
	case "AddedBlackList", "RemovedBlackList":
		return map[string]string{"_user": addr}, true
	case "DestroyedBlackFunds":
		data = strings.TrimPrefix(data, "0x")
		if len(data) < 64 {
			return nil, false
		}
		v, ok := new(big.Int).SetString(data[:64], 16)
		if !ok {
			return nil, false
		}
		return map[string]string{"_blackListedUser": addr, "_balance": v.String()}, true
	}
	return nil, false
}

// topicAddress reads the address in a 32-byte topic.
func topicAddress(topic string) string {
	topic = strings.TrimPrefix(topic, "0x")
	if len(topic) != 64 {
		return ""
	}
	a, err := tronaddr.HexToBase58("41" + topic[24:])
	if err != nil {
		return ""
	}
	return a
}

// BlacklistSince returns the blacklist events the index holds in blocks
// after `after`, oldest first, shaped as TronGrid's.
func BlacklistSince(ctx context.Context, ch *sql.DB, after uint64) ([]tron.ContractEvent, error) {
	rows, err := ch.QueryContext(ctx, `
		SELECT event, tx_hash, block_number, block_time, topics, data
		FROM contract_events FINAL
		WHERE chain = 'tron' AND contract = ? AND block_number > ?
		  AND event IN ('AddedBlackList', 'RemovedBlackList', 'DestroyedBlackFunds')
		ORDER BY block_number, tx_hash, position`, tron.USDTContract, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tron.ContractEvent
	for rows.Next() {
		var e tron.ContractEvent
		var block uint64
		var topics []string
		var data string
		if err := rows.Scan(&e.Name, &e.TxID, &block, &e.Time, &topics, &data); err != nil {
			return nil, err
		}
		res, ok := DecodeBlacklist(e.Name, topics, data)
		if !ok {
			continue
		}
		e.Block, e.Result, e.Time = int64(block), res, e.Time.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
