package tron

import "sync/atomic"

// requests counts HTTP requests sent to TronGrid by this process, retries
// included: each one counts against the key's daily quota
// (docs/DECISIONS.md D35).
var requests atomic.Int64

// TakeRequests returns the requests sent since the last call and resets the
// count.
func TakeRequests() int64 { return requests.Swap(0) }
