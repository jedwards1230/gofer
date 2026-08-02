package uicopy

// usage.go holds the /usage command-panel tab's copy: the two honest empty
// states, the token rows, and the cost breakdown.

import "strconv"

// UsageNoSession is the tab's empty state on the overview.
const UsageNoSession = "No active session — attach to see its usage."

// UsageNone is the tab's empty state for a session that has finished no turn.
const UsageNone = "No usage recorded yet."

// UsageInputTokens is the input-token row.
func UsageInputTokens(n int) string { return "Input tokens: " + strconv.Itoa(n) }

// UsageOutputTokens is the output-token row.
func UsageOutputTokens(n int) string { return "Output tokens: " + strconv.Itoa(n) }

// UsageCacheReadTokens is the cache-read row, omitted when the bucket is zero.
func UsageCacheReadTokens(n int) string { return "Cache read tokens: " + strconv.Itoa(n) }

// UsageCacheWriteTokens is the cache-write row, omitted when the bucket is zero.
func UsageCacheWriteTokens(n int) string { return "Cache write tokens: " + strconv.Itoa(n) }

// UsageCostUnknown is the total-cost row for a session priced at zero, which
// means unpriced rather than free.
const UsageCostUnknown = "Cost: —"

// UsageCost is the total-cost row. usd is already formatted.
func UsageCost(usd string) string { return "Cost: " + usd }

// UsageCostInput is the input bucket of the cost breakdown.
func UsageCostInput(usd string) string { return "  Input: " + usd }

// UsageCostOutput is the output bucket of the cost breakdown.
func UsageCostOutput(usd string) string { return "  Output: " + usd }

// UsageCostCacheRead is the cache-read bucket of the cost breakdown.
func UsageCostCacheRead(usd string) string { return "  Cache read: " + usd }

// UsageCostCacheWrite is the cache-write bucket of the cost breakdown.
func UsageCostCacheWrite(usd string) string { return "  Cache write: " + usd }
