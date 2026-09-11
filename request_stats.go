package main

import "sync/atomic"

type requestStats struct {
	calls, errors, retries, rateLimits atomic.Uint64
}

type requestCounts struct {
	Calls      uint64 `json:"calls"`
	Errors     uint64 `json:"errors"`
	Retries    uint64 `json:"retries"`
	RateLimits uint64 `json:"rate_limits"`
}

func (stats *requestStats) snapshot() requestCounts {
	return requestCounts{stats.calls.Load(), stats.errors.Load(), stats.retries.Load(), stats.rateLimits.Load()}
}

func (counts requestCounts) since(before requestCounts) requestCounts {
	return requestCounts{counts.Calls - before.Calls, counts.Errors - before.Errors, counts.Retries - before.Retries, counts.RateLimits - before.RateLimits}
}
