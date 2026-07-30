// Package lspdiag is the consuming layer over the SDK's lsp/ package (see
// agent-sdk-go's docs/DESIGN.md "LSP" section): it starts real language
// servers on demand, drives a diagnostics round-trip after an edit/write
// tool call, and hands the result to gofer's tool-result pipeline so it
// reaches both the transcript/event stream AND the model's own context.
//
// # Why the tool-registry seam
//
// The SDK's loop package documents tool.Result.Metadata.Diagnostics as "a
// slot the loop fills with LSP diagnostics gathered around the tool call
// (M3) — tools never populate it themselves" (agent-sdk-go tool/tool.go), but
// nothing in the SDK actually fills it yet: loop/toolreg.go's
// registryAdapter.Run explicitly does not surface it
// ("tool.Result.Metadata.Diagnostics is an M3 slot the builtins never
// populate, so it is still not surfaced here"). Until the SDK ships that,
// this package reaches diagnostics into a session the same way
// internal/sandbox reaches containment in: decorating the consumer-side
// loop.ToolRegistry/loop.Tool interfaces gofer already builds a session's
// tool surface from (see internal/sandbox.WrapRegistry), never an SDK
// internal. loop.ToolResult.Diagnostics — the field this package sets — is
// itself part of that public contract and already flows end to end to every
// client (internal/wirestream, internal/daemonbridge, the JSONL journal),
// unmodified by this package.
//
// # Reaching the model, not just the transcript
// A tool result's Diagnostics field is client-facing metadata; the loop only
// feeds a tool's Content back to the model on the next turn. So a diagnostic
// that only sets Diagnostics would be visible to a UI but invisible to the
// agent that needs to act on it. This package therefore also appends a
// bounded, capped summary onto the tool's own Content — see [Wrap].
//
// # Lifecycle
//
// One [Manager] is shared by every session a Supervisor hosts: language
// servers are workspace-scoped (one gopls per cwd+language), matching
// real-editor behavior, not spawned per session. A server starts lazily on
// the first diagnosable tool call in its workspace and is torn down only
// once, by [Manager.Close] — never per session.
//
// # Failure mode
//
// LSP is advisory everywhere in this package: no server on PATH, an
// unsupported language, a crashed server, or a diagnostics wait that times
// out all degrade silently to the tool's original, unmodified result. A tool
// call never fails, and never even slows down beyond the configured timeout,
// because a language server is unavailable.
package lspdiag
