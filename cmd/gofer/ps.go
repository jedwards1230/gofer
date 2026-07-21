package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// jsonRPCMethodNotFound is the standard JSON-RPC 2.0 "method not found" code. A
// daemon predating gofer/fleet answers with it, which `gofer ps` treats as "no
// fleet total to show" rather than an error (see fetchFleet).
const jsonRPCMethodNotFound = -32601

// isMethodNotFound reports whether err is a daemon reply that the called method
// is unknown — the signal of a daemon older than the method being called.
func isMethodNotFound(err error) bool {
	var ce *daemon.CallError
	return errors.As(err, &ce) && ce.Code == jsonRPCMethodNotFound
}

// shortIDLen is how many leading characters of a session id `gofer ps` shows
// — long enough to disambiguate in practice (session ids are UUIDv7, and
// gofer's own workflow rarely runs more than a handful of sessions at once),
// short enough to keep the table legible. kill/archive accept any unambiguous
// prefix (see [resolveSessionID]), so a short id copy-pasted from `gofer ps`
// is always enough on its own.
const shortIDLen = 8

// shortID truncates id to [shortIDLen] characters for display. ids shorter
// than that (e.g. in a test fixture) are returned unchanged.
func shortID(id string) string {
	if len(id) <= shortIDLen {
		return id
	}
	return id[:shortIDLen]
}

// psRow mirrors the subset of the daemon's gofer/roster and gofer/ps wire
// shape (internal/daemon/wire.go's sessionInfoDTO) that `gofer ps` renders —
// decoded independently rather than importing an internal/daemon type, since
// this IS the daemon's public wire contract: any ACP client (an editor, a
// phone app) decodes the same JSON the same way. Fields the table does not
// show (created/updated timestamps) are deliberately omitted rather than
// decoded and dropped.
type psRow struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Model   string `json:"model"`
	Cost    psCost `json:"cost"`
	Queued  int    `json:"queued"`
	Project string `json:"project"`
	Live    bool   `json:"live"`
	// BinaryVersion is the gofer build running the session's process. Under M6
	// process isolation each session runs in its own worker, so a daemon upgrade
	// leaves old workers finishing their turns on the OLD binary while new
	// sessions start on the new one — and this column is how an operator sees
	// that drain happening. Additive and live-only: an older daemon never sends
	// it, and an offline row has no process, so both render as "-".
	BinaryVersion string `json:"binaryVersion,omitempty"`
}

// psCost decodes only the total USD field of provider.Cost — the one column
// `gofer ps` renders.
type psCost struct {
	USD float64 `json:"usd"`
}

// runPS implements `gofer ps [--all]`: the live roster by default, or every
// on-disk session (including archived ones) with --all.
func runPS(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "include archived sessions, not just the live roster")
	df := addDaemonFlags(fs)
	if help, err := parseFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}
	if len(fs.Args()) > 0 {
		return &usageError{msg: "usage: gofer ps [--all]"}
	}

	c, err := dialDaemon(ctx, df, "", stderr)
	if err != nil {
		return daemonDialErr(df.addr, err)
	}
	defer func() { _ = c.Close() }()

	method := "gofer/roster"
	if *all {
		method = "gofer/ps"
	}
	rows, err := fetchRows(ctx, c, method)
	if err != nil {
		return err
	}

	writePSTable(stdout, rows, *all)

	// Fleet-wide total: with M6 process isolation each session's cost lives in its
	// own worker, so the daemon aggregates it (gofer/fleet) rather than the client
	// re-summing the roster. An in-process or older daemon does not report one, in
	// which case the footer is silently omitted (see fetchFleet).
	fleet, err := fetchFleet(ctx, c)
	if err != nil {
		return err
	}
	writeFleetFooter(stdout, fleet, rows)
	return nil
}

// fetchRows calls a gofer/roster- or gofer/ps-shaped method and decodes its
// result as a []psRow.
func fetchRows(ctx context.Context, c *daemon.Client, method string) ([]psRow, error) {
	result, err := c.Call(ctx, method, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	var rows []psRow
	if err := json.Unmarshal(result, &rows); err != nil {
		return nil, fmt.Errorf("%s: decode response: %w", method, err)
	}
	return rows, nil
}

// psFleet decodes the gofer/fleet reply: the fleet-wide total the daemon
// aggregates across live sessions. Supported is false for an in-process daemon
// (no per-worker fan-out to aggregate), which — like an older daemon that never
// implements the method — makes `gofer ps` omit the fleet footer.
type psFleet struct {
	Supported bool    `json:"supported"`
	Cost      psCost  `json:"cost"`
	Usage     psUsage `json:"usage"`
}

// psUsage decodes the token counters of the fleet total's provider.Usage that
// the footer renders.
type psUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// fetchFleet calls gofer/fleet and decodes the fleet total. A method-not-found
// error from an older daemon that never implemented it is NOT an error to the
// caller: it degrades to an unsupported (footer-less) result, exactly as a
// daemon that answers {supported:false} does. Any other error propagates.
func fetchFleet(ctx context.Context, c *daemon.Client) (psFleet, error) {
	result, err := c.Call(ctx, "gofer/fleet", nil)
	if err != nil {
		if isMethodNotFound(err) {
			return psFleet{Supported: false}, nil
		}
		return psFleet{}, fmt.Errorf("gofer/fleet: %w", err)
	}
	var fleet psFleet
	if err := json.Unmarshal(result, &fleet); err != nil {
		return psFleet{}, fmt.Errorf("gofer/fleet: decode response: %w", err)
	}
	return fleet, nil
}

// writeFleetFooter prints the fleet-wide cost/usage line beneath the table when
// the daemon reported a total (a worker-mode daemon). It counts the LIVE rows so
// the footer says how many sessions the total spans, matching the router's
// live-only aggregation. Nothing is printed when the total is unsupported, so an
// in-process or older daemon's `gofer ps` looks exactly as it did before.
func writeFleetFooter(w io.Writer, fleet psFleet, rows []psRow) {
	if !fleet.Supported {
		return
	}
	live := 0
	for _, r := range rows {
		if r.Live {
			live++
		}
	}
	_, _ = fmt.Fprintf(w, "\nFleet: $%.4f, %d tokens (%d live)\n",
		fleet.Cost.USD, fleet.Usage.InputTokens+fleet.Usage.OutputTokens, live)
}

// writePSTable renders rows as an aligned table: ID (short), STATUS, MODEL,
// COST, QUEUED, BINARY, PROJECT, plus LIVE when showLive is set (the --all view,
// where an archived entry's Live=false is the only thing distinguishing it from
// a live one).
//
// BINARY sits next to PROJECT rather than at the end so it stays on screen in a
// narrow terminal: under M6 it is the column that shows a daemon upgrade
// draining — old workers finishing on the old build alongside new sessions on
// the new one — which is exactly what an operator wants to see mid-upgrade.
func writePSTable(w io.Writer, rows []psRow, showLive bool) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	header := "ID\tSTATUS\tMODEL\tCOST\tQUEUED\tBINARY\tPROJECT"
	if showLive {
		header += "\tLIVE"
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, r := range rows {
		line := fmt.Sprintf("%s\t%s\t%s\t$%.4f\t%d\t%s\t%s",
			shortID(r.ID), r.Status, r.Model, r.Cost.USD, r.Queued, psBinaryVersion(r.BinaryVersion), r.Project)
		if showLive {
			line += fmt.Sprintf("\t%v", r.Live)
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
}

// psBinaryVersion renders a row's BINARY cell, substituting "-" for the empty
// value an offline row (no process to have a build) or a pre-M6 daemon (never
// sends the field) produces — so an absent version reads as absent rather than
// as a blank column that looks like a rendering bug.
func psBinaryVersion(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

// resolveSessionID resolves idOrPrefix — typically a [shortID] copy-pasted
// from `gofer ps` — against the live roster, the same set gofer/kill and
// gofer/archive both operate on. An exact id match wins outright (so a full
// id is never ambiguous even if it happens to prefix another one); otherwise
// idOrPrefix must be an unambiguous prefix of exactly one live session's id.
// The supervisor itself does no prefix matching (session ids are looked up
// exactly), so this is what makes a `gofer ps`-printed short id usable on the
// command line at all.
func resolveSessionID(ctx context.Context, c *daemon.Client, idOrPrefix string) (string, error) {
	rows, err := fetchRows(ctx, c, "gofer/roster")
	if err != nil {
		return "", err
	}

	var matches []string
	for _, r := range rows {
		if r.ID == idOrPrefix {
			return r.ID, nil
		}
		if strings.HasPrefix(r.ID, idOrPrefix) {
			matches = append(matches, r.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no live session matches id %q", idOrPrefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("id %q matches %d live sessions, be more specific", idOrPrefix, len(matches))
	}
}
