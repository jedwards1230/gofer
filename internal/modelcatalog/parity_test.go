package modelcatalog_test

// parity_test.go pins the exact catalog a Codex-shaped listing resolves to.
//
// Its job is narrow and specific: [internal/modelcatalog] once implemented the
// Codex listing itself (internal/openaimodels) and now delegates to the SDK's
// provider.ModelLister. That swap must be INVISIBLE — the picker has to render
// byte-identically across it. The golden here is the fully-resolved []Model
// that feeds the picker, captured from the pre-migration implementation and
// frozen; the post-migration implementation has to reproduce it exactly.
//
// Why this artifact and not a rendered picker frame: the picker's row builder
// (internal/tui, catalogDescriptionLine) is a pure function of a
// modelcatalog.Model plus the compiled-in provider registry. Neither is
// reachable from this package, and neither is touched by the migration — so an
// identical []Model is sufficient for an identical row. Pinning the []Model
// here keeps the assertion in the package that owns the behavior.

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/modelcatalog"
)

var update = flag.Bool("update", false, "rewrite the parity golden file")

// codexShapedListing mirrors the real Codex catalogue's schema and its awkward
// cases. Every entry exists to make one decode rule observable in the golden:
//
//   - "visibility" carries only "list" and "hide" in the real response, so both
//     appear here and the "hide" entries must not survive into the catalog.
//   - "context_window" and "max_context_window" disagree on some entries. The
//     smaller "context_window" is authoritative: preferring "max" on the real
//     response would overstate two models' windows by 3.68x (272000 reported as
//     1000000). gpt-5.6-sol carries the disagreement on a VISIBLE model so the
//     winner is legible in the golden — on the real response the divergent
//     entries happen to be hidden ones, where the choice would be invisible.
//   - An entry with neither window must stay 0, meaning UNKNOWN, not backfilled.
//   - An entry with only "max_context_window" uses it: that is a fallback
//     between two vendor-supplied numbers, not a guess.
//   - An entry with no "display_name" must fall back to gofer's compiled-in
//     label rather than rendering a raw slug.
//   - An entry gofer has never heard of must still come through with the
//     vendor's name.
//   - A visibility value from outside the known vocabulary must fail OPEN
//     (kept), so a future vendor rename cannot silently empty the picker.
const codexShapedListing = `{"models":[
	{"slug":"gpt-5.6-sol","display_name":"GPT-5.6 Sol","context_window":272000,"max_context_window":1000000,"visibility":"list"},
	{"slug":"gpt-5.4","display_name":"GPT-5.4","context_window":272000,"max_context_window":1000000,"visibility":"hide"},
	{"slug":"gpt-5.6-terra","display_name":"GPT-5.6 Terra (live)","context_window":272000,"visibility":"list"},
	{"slug":"codex-auto-review","display_name":"Codex Auto Review","context_window":272000,"max_context_window":1000000,"visibility":"hide"},
	{"slug":"gpt-5.6-luna","display_name":"GPT-5.6 Luna","visibility":"list"},
	{"slug":"gpt-5.5","max_context_window":400000,"visibility":"list"},
	{"slug":"gpt-6.0-vega","display_name":"GPT-6.0 Vega","context_window":2000000,"visibility":"list"},
	{"slug":"gpt-5.9-quasar","display_name":"GPT-5.9 Quasar","context_window":128000,"visibility":"experimental"}
]}`

// TestCatalogParityGolden freezes the resolved catalog for the fixture above.
//
// A diff here after the SDK migration means the picker changed — a raw slug
// where a name belonged, an "unknown" where a real window belonged, a hidden
// model surfacing, or an ordering change. Regenerate it ONLY with evidence that
// the new output is the correct one.
func TestCatalogParityGolden(t *testing.T) {
	root := oauthRoot(t)
	srv, httpc := listingServer(t, http.StatusOK, codexShapedListing)

	got, err := modelcatalog.Catalog(context.Background(), root, "openai",
		modelcatalog.WithDiscovery(httpc, srv.URL))
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	dump := dumpCatalog(got)
	path := filepath.Join("testdata", "parity_codex_catalog.golden")

	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(dump), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote golden %s:\n%s", path, dump)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if dump != string(want) {
		t.Errorf("the resolved catalog changed — the picker does NOT render identically.\n\ngot:\n%s\nwant:\n%s", dump, want)
	}
}

// dumpCatalog renders a catalog as stable, diffable text carrying EVERY field
// of every entry. Order is preserved rather than sorted: the vendor's order is
// the picker's order, so a reordering is a real difference and must show up.
func dumpCatalog(models []modelcatalog.Model) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d models\n", len(models))
	for i, m := range models {
		fmt.Fprintf(&b, "%d\tid=%s\tprovider=%s\tlabel=%s\tcontext_window=%d\n",
			i, m.ID, m.Provider, m.Label, m.ContextWindow)
	}
	return b.String()
}
