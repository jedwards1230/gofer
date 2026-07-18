package worker

import (
	"sync"

	"github.com/google/uuid"
)

// PinnedIDGen returns a stateful id generator for [runner.Options.IDGen] that
// PINS a session's id: its FIRST call returns id, and every call after returns
// a fresh UUIDv7.
//
// # Why the first call is special
//
// The SDK session store draws the SESSION id from its id generator's first call
// (session/store.go: `id := s.idGen()` in FileStore.Create) and every journal
// ENTRY id from a later call (Journal.Append). So returning id once and fresh
// UUIDv7s thereafter makes the store adopt id as the session id while keeping
// entry ids distinct — a constant generator would collide every entry id.
//
// # Why this exists (a BRIDGE)
//
// The M6 router pre-generates a session's uuid so it can key the worker's
// socket, endpoint file, and lock by it BEFORE the worker starts (design Option
// A); the worker must then make the store adopt that exact uuid as its session
// id. There is no runner.Options.SessionID seam yet — one is being added to the
// SDK in parallel. When it lands, this whole helper collapses to a one-line
// `opts.SessionID = id`. It is deliberately isolated here and guarded by a test
// (pinnedidgen_test.go) that fails LOUDLY if a future SDK bump reorders id
// generation so the first draw is no longer the session id.
//
// The returned closure is safe for concurrent use, though in practice a
// single-session worker's store draws ids serially.
func PinnedIDGen(id string) func() string {
	var (
		mu   sync.Mutex
		used bool
	)
	return func() string {
		mu.Lock()
		first := !used
		used = true
		mu.Unlock()
		if first {
			return id
		}
		return newV7()
	}
}

// newV7 returns a fresh UUIDv7 string, mirroring the SDK store's own default
// generator so pinned entry ids are indistinguishable from unpinned ones. Like
// the SDK, it falls back to a random UUID string if the clock-based v7 draw
// fails.
func newV7() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
