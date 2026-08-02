// Package uicopy is the single home for gofer's operator-facing TUI copy: every
// phrase a human reads on screen, in one place, so it can be changed once
// instead of hunted for across 46 files.
//
// # The audience split, which is the whole point
//
// gofer's strings serve two consumers, and only one of them may live here:
//
//   - Operator-facing — panel labels, status words, key descriptions, transcript
//     chrome, human error prose. These belong in this package.
//   - Model-facing — tool descriptions, composed system prompts, ask_user
//     question text, tool result content. These MUST NOT live here. They are not
//     copy; they are behaviour. Rewording a tool description changes what the
//     agent does, so the day a second locale exists, translating one of these
//     would silently change how gofer acts.
//
// The split is enforced by this package boundary rather than by a naming
// convention: a string is operator-facing if and only if it is in here. Some
// strings genuinely have two masters and are deliberately left where they are,
// annotated in place — [gofer/internal/tui] keeps the shell fold header and the
// no-output marker beside the prompt composer that emits them, and keeps the
// shell run's note, which is written into the model's context AND rendered to
// the operator from the same value.
//
// # Why constants and functions rather than a keyed map
//
// A map lookup fails quietly: a mistyped key returns the zero string and the TUI
// renders a blank line, which no test and no reader will notice. Constants make
// the same mistake a compile error.
//
// Parameterised copy is a function rather than an exported format string for the
// same reason. Half the multi-argument messages here interpolate two or more
// values, and an exported format string lets a caller transpose them silently;
// a function's parameters are named and its arity is checked. It also keeps
// go vet's printf analysis pointed at the format string, next to its arguments.
//
// Plurals are per-message functions rather than a shared helper taking a noun.
// English's "add an s" rule is an implementation detail of each entry here, not
// a rule call sites should encode — which is what a future move to CLDR plural
// forms needs, and what a shared helper would have to unpick.
//
// # Scope
//
// This package covers [gofer/internal/tui]. Operator copy also lives in
// cmd/gofer, internal/permrationale, and internal/supervisor's sentinel errors,
// which are rendered verbatim on the TUI's status line; those are not migrated
// yet. New TUI copy belongs here, and TestNoInlineOperatorCopy in
// [gofer/internal/tui] fails if it is added inline instead.
//
// # Naming
//
// Entries are named <Domain><Thing>, where the domain is the TUI surface the
// copy belongs to, and the files here mirror the internal/tui file that owns
// that surface — so "where does this string live" is answerable without a grep.
package uicopy
