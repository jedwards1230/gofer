package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/tool"
)

// ToolName is the name the model calls the structured-decision tool by. It is
// gofer's FIRST tool of its own: every other tool a session sees is an SDK
// builtin registered verbatim (see internal/sandbox.WrapRegistry).
const ToolName = "ask_user"

// noClientContent is the model-facing result when nothing is attached to answer
// (see [ErrNoClient]). It names the alternative rather than just reporting
// failure, because the model's useful next move is to keep going in prose.
const noClientContent = "no client is attached to answer questions — continue in conversation instead"

// AskUser is the `ask_user` [tool.Tool]: the model asks the human one or more
// titled, multiple-choice questions and the turn blocks until a client answers.
// It is policy-free like every SDK builtin — it owns the schema and the
// model-facing formatting, while the blocking, the id space, and the
// answer validation belong to the [Gate] it holds.
type AskUser struct {
	gate *Gate
}

// NewAskUser returns the tool bound to gate, the session's decision registry.
func NewAskUser(gate *Gate) *AskUser { return &AskUser{gate: gate} }

// AskUser is a tool the SDK's registry accepts. Failing this assertion means
// the tool contract drifted from what gofer implements.
var _ tool.Tool = (*AskUser)(nil)

// Name returns "ask_user".
func (*AskUser) Name() string { return ToolName }

// Description returns the model-facing description.
func (*AskUser) Description() string {
	return "Ask the user a structured question with concrete options and get a typed " +
		"answer back. Use it when you need a decision only the user can make — a " +
		"tradeoff between real alternatives, a missing requirement, a go/no-go — " +
		"rather than guessing or asking in prose. Give each option a short label " +
		"and a rationale naming its benefit and its risk, and mark at most one " +
		"recommended. The user can always answer in free text or ask to discuss " +
		"instead; set allow_free_text or allow_chat to false only when such an " +
		"answer would be meaningless. Do not use it for questions you can answer " +
		"by reading the code, and do not use it to ask permission to run a tool — " +
		"tool calls have their own approval path."
}

// Spec returns the JSON Schema for the tool's input: an object with a required
// "questions" array of question objects, each carrying its own "options" array.
// Field names are snake_case, matching the SDK builtins' convention rather than
// the camelCase of the acp wire types these decode into.
func (*AskUser) Spec() tool.Schema {
	option := tool.Property{
		Type: "object",
		Properties: map[string]tool.Property{
			"label": {
				Type:        "string",
				Description: "Short label for the choice itself, e.g. \"Shadow table + backfill\".",
			},
			"rationale": {
				Type:        "string",
				Description: "One line of reasoning shown under the label: the benefit and the risk.",
			},
			"reference": {
				Type:        "string",
				Description: "Optional locator for supporting material, e.g. a file:line or URL.",
			},
			"recommended": {
				Type:        "boolean",
				Description: "Marks the option you recommend. Mark at most one.",
			},
		},
	}
	question := tool.Property{
		Type: "object",
		Properties: map[string]tool.Property{
			"title": {
				Type:        "string",
				Description: "Short chip label for the decision, e.g. \"Migration strategy\".",
			},
			"question": {
				Type:        "string",
				Description: "The question text. Required.",
			},
			"context": {
				Type:        "string",
				Description: "Optional supporting context the client can show alongside the question.",
			},
			"options": {
				Type:        "array",
				Description: "The choices offered, in the order to show them.",
				Items:       &option,
			},
			"allow_free_text": {
				Type:        "boolean",
				Default:     DefaultAllowFreeText,
				Description: "Offer a free-text answer. Defaults to true; set false only when a typed answer would be meaningless.",
			},
			"allow_chat": {
				Type:        "boolean",
				Default:     DefaultAllowChat,
				Description: "Offer \"let's discuss this instead\". Defaults to true; set false only when discussion cannot help.",
			},
		},
	}
	return tool.ObjectSchema([]string{"questions"}, map[string]tool.Property{
		"questions": {
			Type:        "array",
			Description: "The questions to ask, in order. Ask everything you need in one call.",
			Items:       &question,
		},
	})
}

// askUserInput is the decoded shape of the tool's Run argument.
type askUserInput struct {
	Questions []questionInput `json:"questions"`
}

// questionInput is one decoded question. AllowFreeText/AllowChat are *bool so
// an omitted field is distinguishable from an explicit false: they default to
// TRUE (see the package doc), so decoding them into a plain bool would silently
// turn every question the model did not annotate into a forced choice.
type questionInput struct {
	Title         string        `json:"title"`
	Question      string        `json:"question"`
	Context       string        `json:"context"`
	Options       []optionInput `json:"options"`
	AllowFreeText *bool         `json:"allow_free_text"`
	AllowChat     *bool         `json:"allow_chat"`
}

// optionInput is one decoded choice.
type optionInput struct {
	Label       string `json:"label"`
	Rationale   string `json:"rationale"`
	Reference   string `json:"reference"`
	Recommended bool   `json:"recommended"`
}

// Run asks the questions and blocks until they are answered.
//
// The (Result, error) split follows [tool.Tool]'s contract exactly. Only three
// things are Go errors: input that cannot be decoded at all, ctx cancellation
// (an interrupted turn — the loop aborts the turn on it rather than feeding it
// back to the model), and the session's gate closing under it ([ErrClosed]).
// Everything the model could correct or
// react to — a malformed question set, no client attached to answer, a
// cancelled or chat outcome — comes back as a [tool.Result], and only the first
// two of those set IsError: a user who chose to discuss instead of answering is
// a legitimate outcome, not a failure.
func (t *AskUser) Run(ctx context.Context, input json.RawMessage) (tool.Result, error) {
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	var in askUserInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tool.Result{}, fmt.Errorf("decision: %s: decode input: %w", ToolName, err)
	}
	if problem := validate(in); problem != "" {
		return tool.Result{IsError: true, Content: problem}, nil
	}

	// Stamp ids on our own copy too: Gate.Request re-stamps identically (see
	// AssignIDs), and formatting the result needs the same ids the answers
	// come back keyed by, plus the labels to echo alongside them.
	questions := AssignIDs(toACP(in.Questions))
	answers, err := t.gate.Request(ctx, questions)
	switch {
	case errors.Is(err, ErrNoClient):
		return tool.Result{IsError: true, Content: noClientContent}, nil
	case err != nil:
		// Request returns only ErrNoClient (handled above), ErrClosed, or the
		// ctx error. The last two both mean the turn has nowhere to land — the
		// session is being torn down, or was interrupted — so both abort it as
		// Go errors, per the contract above.
		return tool.Result{}, err
	}

	extra, err := marshalAnswers(answers)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{
		Content:  formatAnswers(questions, answers),
		Metadata: tool.Metadata{Extra: map[string]any{"answers": extra}},
	}, nil
}

// validate reports the first thing wrong with a decoded call as a
// model-facing sentence naming the fix, or "" when the call is well formed.
// A question with no options is fine as long as free text is allowed — that is
// the "just ask me" shape; with both switched off there would be no way to
// answer at all.
func validate(in askUserInput) string {
	if len(in.Questions) == 0 {
		return fmt.Sprintf("%s: questions must not be empty — pass at least one question", ToolName)
	}
	for i, q := range in.Questions {
		if strings.TrimSpace(q.Question) == "" {
			return fmt.Sprintf("%s: question %d has no question text — set its \"question\" field", ToolName, i+1)
		}
		if len(q.Options) == 0 && !boolOr(q.AllowFreeText, DefaultAllowFreeText) {
			return fmt.Sprintf("%s: question %d offers no options and no free text — add options or leave allow_free_text unset", ToolName, i+1)
		}
	}
	return ""
}

// toACP converts decoded input to the acp wire types, resolving the two
// escape-hatch defaults. Ids are left empty — [AssignIDs] owns them.
func toACP(questions []questionInput) []acp.DecisionQuestion {
	out := make([]acp.DecisionQuestion, len(questions))
	for i, q := range questions {
		var options []acp.DecisionOption
		if len(q.Options) > 0 {
			options = make([]acp.DecisionOption, len(q.Options))
			for j, o := range q.Options {
				options[j] = acp.DecisionOption{
					Label:       o.Label,
					Rationale:   o.Rationale,
					Reference:   o.Reference,
					Recommended: o.Recommended,
				}
			}
		}
		out[i] = acp.DecisionQuestion{
			Title:         q.Title,
			Question:      q.Question,
			Context:       q.Context,
			Options:       options,
			AllowFreeText: boolOr(q.AllowFreeText, DefaultAllowFreeText),
			AllowChat:     boolOr(q.AllowChat, DefaultAllowChat),
		}
	}
	return out
}

// boolOr resolves an optional bool: the model's explicit value, or def when the
// field was omitted.
func boolOr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// marshalAnswers encodes the answers as the ACP session/request_decision
// response payload — {"answers":[…]} — for [tool.Metadata].Extra. That shape,
// rather than a bare array, is deliberate: it is exactly what the daemon relay
// puts on the wire in the follow-up PR, and it round-trips through
// [acp.RequestDecisionResponse]'s unmarshaller, which is what resolves each
// outcome back to its concrete variant. Metadata never enters the model's
// context; it is there for a client that wants to render the structured form.
func marshalAnswers(answers []acp.DecisionAnswer) (json.RawMessage, error) {
	b, err := json.Marshal(acp.RequestDecisionResponse{Answers: answers})
	if err != nil {
		return nil, fmt.Errorf("decision: %s: encode answers: %w", ToolName, err)
	}
	return b, nil
}

// formatAnswers renders the model-facing result: one line per question, in
// question order, with an indented "notes:" line under any answer the user
// annotated. It is deterministic — ids are position-derived and the iteration
// is over questions, not over a map — so the same decision always reads the
// same way in a journal, a golden, and the model's context.
//
// answers is [Gate.Answer]-normalized: exactly one entry per question, in the
// same order, never with a nil outcome.
func formatAnswers(questions []acp.DecisionQuestion, answers []acp.DecisionAnswer) string {
	var b strings.Builder
	for i, q := range questions {
		if i >= len(answers) {
			break
		}
		a := answers[i]
		fmt.Fprintf(&b, "%s %q → %s\n", q.QuestionID, q.Question, describeOutcome(q, a.Outcome))
		if a.Notes != "" {
			fmt.Fprintf(&b, "    notes: %s\n", a.Notes)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// describeOutcome renders one outcome's right-hand side. A selected option
// echoes both its id and its label so the model can reason about what it got
// without holding the id table in its head.
func describeOutcome(q acp.DecisionQuestion, outcome acp.DecisionOutcome) string {
	switch o := outcome.(type) {
	case acp.DecisionOutcomeSelected:
		return fmt.Sprintf("selected %s %q", o.OptionID, optionLabel(q, o.OptionID))
	case acp.DecisionOutcomeText:
		return fmt.Sprintf("text: %s", o.Text)
	case acp.DecisionOutcomeChat:
		return "chat (the user wants to discuss this instead of choosing)"
	case acp.DecisionOutcomeCancelled:
		return "cancelled (unanswered)"
	default:
		// Unreachable while acp's outcome union is what it is; kept so a new
		// variant degrades to something readable instead of an empty line.
		return outcome.Outcome()
	}
}

// optionLabel returns the label of q's optionID. The gate rejects a selected
// outcome naming an option that does not exist, so the fallback only guards a
// future caller that skips that validation.
func optionLabel(q acp.DecisionQuestion, optionID string) string {
	for _, o := range q.Options {
		if o.OptionID == optionID {
			return o.Label
		}
	}
	return ""
}
