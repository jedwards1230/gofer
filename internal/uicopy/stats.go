package uicopy

// stats.go holds the /stats command-panel tab's copy: the current session's
// lifecycle rows and the roster-wide rollup beneath them.

import "strconv"

// StatsSession is the session-name row.
func StatsSession(title string) string { return "Session: " + title }

// StatsAge is the Created→now row. age is already formatted.
func StatsAge(age string) string { return "Age: " + age }

// StatsLastActive is the Updated→now row. since is already formatted.
func StatsLastActive(since string) string { return "Last active: " + since }

// StatsStatus is the session-status row.
func StatsStatus(status string) string { return "Status: " + status }

// StatsModel is the session-model row.
func StatsModel(model string) string { return "Model: " + model }

// StatsSessions is the rollup's session count.
func StatsSessions(n int) string { return "Sessions: " + strconv.Itoa(n) }

// StatsTotalTokens is the rollup's summed token count across the roster.
func StatsTotalTokens(n int) string { return "Total tokens: " + strconv.Itoa(n) }

// StatsTotalCostUnknown is the rollup's cost row when the sum is zero.
const StatsTotalCostUnknown = "Total cost: —"

// StatsTotalCost is the rollup's summed cost. usd is already formatted.
func StatsTotalCost(usd string) string { return "Total cost: " + usd }
