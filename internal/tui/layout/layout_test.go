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
	for _, total := range []int{10, 80, 120, 131, 200} {
		left, right := SplitWidth(total)
		if left+right+len(columnDivider) != total {
			t.Errorf("SplitWidth(%d) = %d,%d: panes+divider=%d, want %d",
				total, left, right, left+right+len(columnDivider), total)
		}
		if left < 1 || right < 1 {
			t.Errorf("SplitWidth(%d) = %d,%d: a pane is non-positive", total, left, right)
		}
	}
}

func TestSplitHeightSumsToTotal(t *testing.T) {
	for _, total := range []int{5, 24, 25, 50} {
		top, bottom := SplitHeight(total)
		if top+bottom+1 != total {
			t.Errorf("SplitHeight(%d) = %d,%d: panes+divider=%d, want %d",
				total, top, bottom, top+bottom+1, total)
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
