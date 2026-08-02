package uicopy

// dialog.go holds the status-line copy the approval dialog's key handlers emit:
// why Tab could not open the amend editor, and the two failure prefixes an
// amend commit and an explain round trip report through.

// DialogAmendUnavailable explains why Tab is a no-op on a call whose spec
// carries no editable command key — an empty editor whose commit would blank
// the call is strictly worse.
const DialogAmendUnavailable = "This call has no editable command to amend."

// DialogAmendErrorPrefix labels an amend commit that could not be sent, the
// underlying error following it.
const DialogAmendErrorPrefix = "amend: "

// DialogExplainErrorPrefix labels a failed explain, the underlying error
// following it. The prompt stays open and answerable.
const DialogExplainErrorPrefix = "explain: "
