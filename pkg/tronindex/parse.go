// Package tronindex reads TRON blocks from a node and turns them into
// token and TRX transfers and contract events, ready to store in an index of
// one's own (docs/INDEXER_PLAN.md).
//
// It depends on nothing else in this repository except pkg/tronaddr. The
// pieces are interfaces, so it can be reused: a Source (a local java-tron node
// or a hosted RPC), a Sink (where blocks are written), and a CursorStore (how
// far each run has got). The AML engine is one user of it.
package tronindex

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/big"
	"strings"
	"time"

	"github.com/mozer/tether-risk/pkg/tronaddr"
)

// Transfer is one movement of TRX or a TRC-20 token.
type Transfer struct {
	TxID  string
	Index uint32 // unique within the transaction, as the KeyScheme defines it
	Block uint64
	Time  time.Time
	From  string // base58check
	To    string
	// Token is the TRC-20 contract (base58check); empty for native TRX.
	Token string
	Value *big.Int // in the asset's smallest unit
}

// Event is one contract log kept whole, for callers that read more than
// transfers: a blacklist, a pool's reserves.
type Event struct {
	TxID     string
	Block    uint64
	Time     time.Time
	Contract string   // base58check
	Topics   []string // hex, topic 0 first
	Data     string   // hex
	Position int      // the log's position within its transaction
}

// BlockData is what one block yields.
type BlockData struct {
	Number    uint64
	Time      time.Time
	Transfers []Transfer
	Events    []Event
}

// KeyScheme decides a transfer's Index.
type KeyScheme int

const (
	// KeyTronGrid reproduces the index this repository derives for rows
	// fetched from TronGrid's TRC-20 endpoint, which returns no log index:
	// an FNV-1a hash of from, to, token and value (docs/DECISIONS.md D11).
	// Rows indexed from a node then match rows fetched from TronGrid, so the
	// two sources can meet in one table without counting a transfer twice.
	// Native TRX keeps its position in the transaction, as TronGrid gives it.
	KeyTronGrid KeyScheme = iota
	// KeyPosition uses positions only: the log's position within its
	// transaction for tokens, the contract's position for TRX. It has no
	// collisions, for an index that never meets TronGrid rows.
	KeyPosition
)

// EventFilter selects logs to keep as events: one contract, and optionally
// one topic 0 (the event's signature hash).
type EventFilter struct {
	Contract string // base58check
	Topic0   string // hex without 0x; empty keeps every event of the contract
}

// Config decides what a block yields.
type Config struct {
	// Tokens lists the TRC-20 contracts whose transfers are kept; nil keeps
	// every token's.
	Tokens map[string]bool
	// NativeTRX keeps TRX transfers.
	NativeTRX bool
	Events    []EventFilter
	Keys      KeyScheme
}

// transferTopic is keccak256("Transfer(address,address,uint256)").
const transferTopic = "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// TronGridIndex is the index KeyTronGrid gives a token transfer. The four
// strings are as TronGrid prints them: base58check addresses and contract,
// and the value in decimal.
func TronGridIndex(from, to, token, value string) uint32 {
	h := fnv.New32a()
	for _, part := range []string{from, to, token, value} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return h.Sum32()
}

// RawBlock is a block as /wallet/getblockbynum returns it with visible=true.
type RawBlock struct {
	BlockID     string `json:"blockID"`
	BlockHeader struct {
		RawData struct {
			Number    uint64 `json:"number"`
			Timestamp int64  `json:"timestamp"`
		} `json:"raw_data"`
	} `json:"block_header"`
	Transactions []RawTx `json:"transactions"`
}

// RawTx is one transaction of a RawBlock.
type RawTx struct {
	TxID string `json:"txID"`
	Ret  []struct {
		ContractRet string `json:"contractRet"`
	} `json:"ret"`
	RawData struct {
		Contract []struct {
			Type      string `json:"type"`
			Parameter struct {
				Value json.RawMessage `json:"value"`
			} `json:"parameter"`
		} `json:"contract"`
	} `json:"raw_data"`
}

// RawTxInfo is one transaction's receipt and logs, as
// /wallet/gettransactioninfobyblocknum returns them.
type RawTxInfo struct {
	ID             string `json:"id"`
	BlockNumber    uint64 `json:"blockNumber"`
	BlockTimeStamp int64  `json:"blockTimeStamp"`
	Receipt        struct {
		Result string `json:"result"`
	} `json:"receipt"`
	Log []struct {
		Address string   `json:"address"` // 20 bytes hex, without the 41 prefix
		Topics  []string `json:"topics"`
		Data    string   `json:"data"`
	} `json:"log"`
}

// ParseBlock turns a block and its transactions' receipts into what cfg
// keeps.
func ParseBlock(b RawBlock, infos []RawTxInfo, cfg Config) (BlockData, error) {
	out := BlockData{Number: b.BlockHeader.RawData.Number, Time: time.UnixMilli(b.BlockHeader.RawData.Timestamp).UTC()}
	byID := make(map[string]*RawTxInfo, len(infos))
	for i := range infos {
		byID[infos[i].ID] = &infos[i]
	}
	for _, tx := range b.Transactions {
		ts, evs, err := ParseTx(tx, byID[tx.TxID], out.Number, out.Time, cfg)
		if err != nil {
			return out, fmt.Errorf("block %d: %w", out.Number, err)
		}
		out.Transfers = append(out.Transfers, ts...)
		out.Events = append(out.Events, evs...)
	}
	return out, nil
}

// ParseTx turns one transaction, and its receipt when it has one, into what
// cfg keeps.
func ParseTx(tx RawTx, info *RawTxInfo, block uint64, at time.Time, cfg Config) ([]Transfer, []Event, error) {
	// A reverted transaction moved nothing.
	if len(tx.Ret) > 0 && tx.Ret[0].ContractRet != "" && tx.Ret[0].ContractRet != "SUCCESS" {
		return nil, nil, nil
	}
	var transfers []Transfer
	var events []Event

	if cfg.NativeTRX {
		for i, c := range tx.RawData.Contract {
			if c.Type != "TransferContract" {
				continue
			}
			var v struct {
				Amount       json.Number `json:"amount"`
				OwnerAddress string      `json:"owner_address"`
				ToAddress    string      `json:"to_address"`
			}
			if err := json.Unmarshal(c.Parameter.Value, &v); err != nil {
				return nil, nil, fmt.Errorf("tx %s contract %d: %w", tx.TxID, i, err)
			}
			amount, ok := new(big.Int).SetString(v.Amount.String(), 10)
			if !ok || amount.Sign() <= 0 {
				continue
			}
			from, err := tronaddr.Normalise(v.OwnerAddress)
			if err != nil {
				return nil, nil, fmt.Errorf("tx %s owner: %w", tx.TxID, err)
			}
			to, err := tronaddr.Normalise(v.ToAddress)
			if err != nil {
				return nil, nil, fmt.Errorf("tx %s to: %w", tx.TxID, err)
			}
			if from == "" || to == "" {
				continue
			}
			transfers = append(transfers, Transfer{TxID: tx.TxID, Index: uint32(i), Block: block, Time: at,
				From: from, To: to, Value: amount})
		}
	}

	if info == nil || (info.Receipt.Result != "" && info.Receipt.Result != "SUCCESS") {
		return transfers, events, nil
	}
	for pos, l := range info.Log {
		contract, err := tronaddr.HexToBase58("41" + strings.TrimPrefix(l.Address, "41"))
		if err != nil {
			continue // a malformed log address says nothing we can store
		}
		for _, f := range cfg.Events {
			if f.Contract == contract && (f.Topic0 == "" || (len(l.Topics) > 0 && strings.EqualFold(l.Topics[0], f.Topic0))) {
				events = append(events, Event{TxID: tx.TxID, Block: block, Time: at, Contract: contract,
					Topics: l.Topics, Data: l.Data, Position: pos})
				break
			}
		}
		if len(l.Topics) != 3 || !strings.EqualFold(l.Topics[0], transferTopic) {
			continue
		}
		if cfg.Tokens != nil && !cfg.Tokens[contract] {
			continue
		}
		from, err1 := topicAddress(l.Topics[1])
		to, err2 := topicAddress(l.Topics[2])
		value, ok := new(big.Int).SetString(strings.TrimPrefix(l.Data, "0x"), 16)
		if err1 != nil || err2 != nil || !ok || l.Data == "" {
			continue
		}
		t := Transfer{TxID: tx.TxID, Block: block, Time: at, From: from, To: to, Token: contract, Value: value}
		if cfg.Keys == KeyTronGrid {
			t.Index = TronGridIndex(from, to, contract, value.String())
		} else {
			t.Index = uint32(pos)
		}
		transfers = append(transfers, t)
	}
	return transfers, events, nil
}

// topicAddress reads an address from a 32-byte topic: its last 20 bytes.
func topicAddress(topic string) (string, error) {
	topic = strings.TrimPrefix(topic, "0x")
	if len(topic) < 40 {
		return "", fmt.Errorf("topic %q is too short for an address", topic)
	}
	return tronaddr.HexToBase58("41" + topic[len(topic)-40:])
}
