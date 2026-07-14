// Package layout holds the geometry constants the TUI screens share. Everything
// here is pure int math so screens stay golden-testable without a terminal.
package layout

// TopPadding is the number of blank rows prepended to every live TUI frame
// (overview, peek, attach) before it is rendered. Some terminal emulators —
// observed on a macOS beta running fullscreen — clip the top row of the
// alt-screen frame, swallowing half the header; this compensates by pushing
// the whole frame down one row. Safe to revert to 0, or make configurable,
// once the underlying terminal bug is fixed or better understood.
const TopPadding = 1
