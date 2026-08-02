package uicopy

// peek.go holds the peek card's copy: the waiting line's status verbs, the
// reply box placeholder, and the footer key hint.

// The waiting-line verbs, one per [SessionStatus] the card can show. They are
// the lowercase counterparts of the roster's section labels
// ([SessionStatusWorking] and friends), and the difference is deliberate: a
// section label heads a list, while a verb is read against the duration that
// follows it ("working 5 minutes"). PeekVerbWaiting stands where the roster
// says [SessionStatusNeedsInput], for the same reason — a duration reads as
// how long the wait has lasted.
const (
	PeekVerbWorking  = "working"
	PeekVerbFinished = "finished"
	PeekVerbIdle     = "idle"
	PeekVerbWaiting  = "waiting"
)

// PeekReplyPlaceholder is the muted stand-in inside the card's ❯ reply box
// while nothing has been typed.
const PeekReplyPlaceholder = "reply"

// PeekFooter is the key hint under the peek card.
const PeekFooter = "enter to open · space/esc to close · ctrl+x×2 to delete"
