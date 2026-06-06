package ui

// scrollWindow returns the [start,end) slice bounds of a scrolling window of size
// limit over n rows, kept around the selected cursor so it stays visible. It is a
// pure function of (cursor, n, limit) — the window FOLLOWS the cursor (no stored
// offset to drift), so the selected row stays in view when paging past the top or
// bottom edge. Shared by the slash palette (renderPalette), the @-mention menu
// (renderMention), and the /models picker (renderModelsPanel); lifted from
// palette.go (was paletteWindow) so the call sites can't diverge.
func scrollWindow(cursor, n, limit int) (start, end int) {
	if n <= limit {
		return 0, n
	}
	start = cursor - limit/2
	if start < 0 {
		start = 0
	}
	end = start + limit
	if end > n {
		end = n
		start = end - limit
	}
	return start, end
}
