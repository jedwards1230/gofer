package runner_test

import (
	"strings"

	"github.com/jedwards1230/agent-sdk-go/provider"
)

// msgText concatenates a message's text blocks.
func msgText(m provider.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Type == provider.BlockText {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// msgReasoning concatenates a message's reasoning blocks.
func msgReasoning(m provider.Message) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Type == provider.BlockReasoning {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// blocksOfType returns the message's content blocks of a given type.
func blocksOfType(m provider.Message, t provider.BlockType) []provider.ContentBlock {
	var out []provider.ContentBlock
	for _, blk := range m.Content {
		if blk.Type == t {
			out = append(out, blk)
		}
	}
	return out
}
