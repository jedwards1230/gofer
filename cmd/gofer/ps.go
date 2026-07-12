package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/jedwards1230/gofer/internal/daemon"
)

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

	c, err := dialDaemon(ctx, df)
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

// writePSTable renders rows as an aligned table: ID (short), STATUS, MODEL,
// COST, QUEUED, PROJECT, plus LIVE when showLive is set (the --all view, where
// an archived entry's Live=false is the only thing distinguishing it from a
// live one).
func writePSTable(w io.Writer, rows []psRow, showLive bool) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	header := "ID\tSTATUS\tMODEL\tCOST\tQUEUED\tPROJECT"
	if showLive {
		header += "\tLIVE"
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, r := range rows {
		line := fmt.Sprintf("%s\t%s\t%s\t$%.4f\t%d\t%s",
			shortID(r.ID), r.Status, r.Model, r.Cost.USD, r.Queued, r.Project)
		if showLive {
			line += fmt.Sprintf("\t%v", r.Live)
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
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
