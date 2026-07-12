package layout

import "testing"

func TestPeekOrientation(t *testing.T) {
	if got := PeekOrientation(PeekHorizontalMinWidth - 1); got != Vertical {
		t.Errorf("just below breakpoint: got %v want Vertical", got)
	}
	if got := PeekOrientation(PeekHorizontalMinWidth); got != Horizontal {
		t.Errorf("at breakpoint: got %v want Horizontal", got)
	}
}

func TestSplitWidthSumsToTotal(t *testing.T) {
	// At and above the minimum splittable width (len(divider)+2 = 5), panes
	// plus the divider sum to exactly total and both are at least 1 column.
	for _, total := range []int{5, 6, 10, 80, 120, 131, 200} {
		left, right := SplitWidth(total)
		if left+right+columnDividerWidth != total {
			t.Errorf("SplitWidth(%d) = %d,%d: panes+divider=%d, want %d",
				total, left, right, left+right+columnDividerWidth, total)
		}
		if left < 1 || right < 1 {
			t.Errorf("SplitWidth(%d) = %d,%d: a pane is non-positive", total, left, right)
		}
	}
}

// TestSplitWidthBelowMinimum documents the degenerate clamp: below the minimum
// splittable width the sum invariant cannot hold, so both panes clamp to 1.
// The peek screen never splits this narrow (PeekHorizontalMinWidth = 120).
func TestSplitWidthBelowMinimum(t *testing.T) {
	for _, total := range []int{0, 2, 3, 4} {
		if left, right := SplitWidth(total); left != 1 || right != 1 {
			t.Errorf("SplitWidth(%d) = %d,%d: want clamp to 1,1", total, left, right)
		}
	}
}

func TestSplitHeightSumsToTotal(t *testing.T) {
	// At and above the minimum splittable height (3), panes plus the divider
	// row sum to exactly total.
	for _, total := range []int{3, 4, 5, 24, 25, 50} {
		top, bottom := SplitHeight(total)
		if top+bottom+1 != total {
			t.Errorf("SplitHeight(%d) = %d,%d: panes+divider=%d, want %d",
				total, top, bottom, top+bottom+1, total)
		}
		if top < 1 || bottom < 1 {
			t.Errorf("SplitHeight(%d) = %d,%d: a pane is non-positive", total, top, bottom)
		}
	}
}

// TestSplitHeightBelowMinimum documents the degenerate clamp below the minimum
// splittable height.
func TestSplitHeightBelowMinimum(t *testing.T) {
	for _, total := range []int{0, 1, 2} {
		if top, bottom := SplitHeight(total); top != 1 || bottom != 1 {
			t.Errorf("SplitHeight(%d) = %d,%d: want clamp to 1,1", total, top, bottom)
		}
	}
}

func TestJoinColumnsPlumbDivider(t *testing.T) {
	left := "ab\nc"    // ragged: widths 2 and 1
	right := "x\nyyyy" // ragged: widths 1 and 4
	got := JoinColumns(left, right)
	want := "ab" + columnDivider + "x   \n" +
		"c " + columnDivider + "yyyy"
	if got != want {
		t.Errorf("JoinColumns mismatch:\n got %q\nwant %q", got, want)
	}
}
