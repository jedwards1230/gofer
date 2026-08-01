// Package lspdiag is the consuming layer over the SDK's lsp/ package (see
// agent-sdk-go's docs/DESIGN.md "LSP" section): it starts real language
// servers on demand, drives a diagnostics round-trip after an edit/write
// tool call, and hands the result to gofer's tool-result pipeline so it
// reaches both the transcript/event stream AND the model's own context.
//
// # Why the tool-registry seam
//
// This package reaches diagnostics into a session the same way
// internal/sandbox reaches containment in: decorating the consumer-side
// loop.ToolRegistry/loop.Tool interfaces gofer already builds a session's
// tool surface from (see internal/sandbox.WrapRegistry), never an SDK
// internal. loop.ToolResult.Diagnostics — the field this package sets — is
// itself part of that public contract and already flows end to end to every
// client (internal/wirestream, internal/daemonbridge, the JSONL journal),
// unmodified by this package.
//
// That decoration is the permanent answer, not an interim one. Earlier
// versions of this doc described it as a stand-in until the SDK filled
// tool.Result.Metadata.Diagnostics — "a slot the loop fills with LSP
// diagnostics gathered around the tool call". That slot never was filled, and
// agent-sdk-go removed both it and the tool.Diagnostic type in v0.23.0
// (agent-sdk-go#112) precisely because it was dead end to end: nothing in the
// SDK ever wrote it and loop/toolreg.go deliberately dropped it. So there is
// no pending SDK feature to migrate onto.
//
// loop.ToolResult.Diagnostics ([]string) is the live path, and an embedder
// reaches it through either seam that yields a loop.ToolResult: decorating a
// loop.ToolRegistry, as this package does, or a loop.Hooks AfterTool hook.
// tool.Result carries no diagnostics field at all, so there is no tool-side
// slot for the SDK to fill.
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
