package skillset

import (
	"strings"

	sdkskill "github.com/jedwards1230/agent-sdk-go/skill"
)

// duplicateNameMarker is the distinguishing fragment of the SDK loader's
// duplicate-name diagnostic ("skill: duplicate name %q; the earlier
// directory's definition wins" — see [sdkskill.Load]). The SDK mints that
// diagnostic with fmt.Errorf and exports no sentinel to compare against, so a
// substring test is the only available signal.
const duplicateNameMarker = "duplicate name"

// IsShadowed reports whether d is the loader's DUPLICATE-name diagnostic —
// i.e. d.Path is a SKILL.md that lost its name to an earlier discovery
// directory (project beats global; see [config.Skills.Directories]).
//
// This is the only half of the precedence story that is recoverable. The
// LOSING file is named by the diagnostic; the WINNING file's path is recorded
// nowhere ([sdkskill.Meta] carries none), which is why nothing here — and
// nothing in internal/capability — reports a source path for a loaded skill.
//
// It degrades gracefully rather than lying: the classification is a substring
// test against an SDK message that is not part of any contract, so a reworded
// message makes this return false and the entry simply stays an ordinary
// diagnostic. The path and the loader's full reason are rendered verbatim
// either way, so a miss costs a label, never information.
func IsShadowed(d sdkskill.Diagnostic) bool {
	return d.Err != nil && strings.Contains(d.Err.Error(), duplicateNameMarker)
}
