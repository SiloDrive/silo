package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/SiloDrive/silo/client"
)

func TestScrollTo(t *testing.T) {
	tests := []struct {
		name                          string
		offset, cursor, n, rows, want int
	}{
		{"list fits, never scrolls", 0, 4, 5, 10, 0},
		{"cursor still on screen", 0, 3, 100, 10, 0},
		{"cursor off the bottom", 0, 10, 100, 10, 1},
		{"cursor off the top", 20, 5, 100, 10, 5},
		{"last item pins the window", 0, 99, 100, 10, 90},
		{"offset past the end is pulled back", 95, 99, 100, 10, 90},
		{"shrunk terminal", 0, 0, 100, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scrollTo(tt.offset, tt.cursor, tt.n, tt.rows); got != tt.want {
				t.Errorf("scrollTo(%d, %d, %d, %d) = %d, want %d",
					tt.offset, tt.cursor, tt.n, tt.rows, got, tt.want)
			}
		})
	}
}

func TestMoveCursor(t *testing.T) {
	tests := []struct {
		key               string
		cursor, offset, n int
		want, wantOffset  int
		wantHandled       bool
	}{
		{"j", 0, 0, 10, 1, 0, true},
		{"down", 9, 0, 10, 9, 0, true},
		{"k", 0, 0, 10, 0, 0, true},
		{"G", 0, 0, 10, 9, 5, true},
		{"g", 9, 5, 10, 0, 0, true},
		{"pgdown", 0, 0, 100, 5, 5, true},
		{"pgup", 7, 5, 100, 2, 0, true},
		{"j", 0, 0, 0, 0, 0, true},
		{"G", 0, 0, 0, 0, 0, true},
		{"enter", 3, 1, 10, 3, 1, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s at %d of %d", tt.key, tt.cursor, tt.n), func(t *testing.T) {
			got, offset, handled := moveCursor(tt.key, tt.cursor, tt.offset, tt.n, 5)
			if got != tt.want || offset != tt.wantOffset || handled != tt.wantHandled {
				t.Errorf("moveCursor(%q, %d, %d, %d, 5) = (%d, %d, %v), want (%d, %d, %v)",
					tt.key, tt.cursor, tt.offset, tt.n, got, offset, handled,
					tt.want, tt.wantOffset, tt.wantHandled)
			}
		})
	}
}

// A page key moves the window by a page, not by the one row a minimal scroll
// would need to keep the cursor on screen.
func TestPageKeysMoveAWholeScreen(t *testing.T) {
	m := browseModel(500, 80, 24)
	rows := m.browseRows()

	m = press(t, m, "pgdown")
	if m.browseCursor != rows || m.browseOffset != rows {
		t.Errorf("after pgdown: cursor %d, offset %d, want both %d", m.browseCursor, m.browseOffset, rows)
	}

	m = press(t, m, "pgup")
	if m.browseCursor != 0 || m.browseOffset != 0 {
		t.Errorf("after pgup: cursor %d, offset %d, want both 0", m.browseCursor, m.browseOffset)
	}
}

// browseModel is a browse view over n generated entries, sized to a terminal.
func browseModel(n, width, height int) model {
	entries := make([]client.DirEntry, n)
	for i := range entries {
		size := int64(1024)
		entries[i] = client.DirEntry{Name: fmt.Sprintf("file%03d", i), Type: "file", Size: &size}
	}
	return model{
		view:              viewBrowse,
		browseLibraryID:   "library",
		browseLibraryName: "Library",
		browsePath:        "/",
		dirEntries:        entries,
		serverURL:         "http://localhost:8082",
		width:             width,
		height:            height,
	}
}

// key spells a keystroke the way Update's own switch does, so a test presses
// "shift+tab" rather than assembling a tea.KeyMsg. The named keys need a Type;
// everything else is the runes it looks like.
func key(s string) tea.KeyMsg {
	switch s {
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func press(t *testing.T, m model, keys ...string) model {
	t.Helper()
	for _, k := range keys {
		next, _ := m.Update(key(k))
		updated, ok := next.(model)
		if !ok {
			t.Fatalf("Update returned %T, want tui.model", next)
		}
		m = updated
	}
	return m
}

// The whole point of the frame: a list longer than the terminal still renders
// exactly one screenful, with the status bar on the last row.
func TestViewFillsExactlyTheTerminal(t *testing.T) {
	for _, size := range []struct{ w, h int }{{80, 24}, {40, 12}, {120, 60}, {30, 6}} {
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			m := browseModel(500, size.w, size.h)
			lines := strings.Split(m.View(), "\n")

			if len(lines) != size.h {
				t.Errorf("rendered %d lines, want %d", len(lines), size.h)
			}
			if !strings.Contains(lines[len(lines)-1], "http://localhost:8082") {
				t.Errorf("last line is %q, want the status bar", lines[len(lines)-1])
			}
			for i, line := range lines {
				if w := lipgloss.Width(line); w > size.w {
					t.Errorf("line %d is %d cells wide, want at most %d", i, w, size.w)
				}
			}
		})
	}
}

// Short screens still get their footer, even when there is no room for a body.
func TestViewSurvivesATinyTerminal(t *testing.T) {
	m := browseModel(500, 20, 3)
	if got := m.View(); got == "" {
		t.Fatal("View() is empty")
	}
}

func TestBrowseScrollsToFollowTheCursor(t *testing.T) {
	m := browseModel(500, 80, 24)
	if !strings.Contains(m.View(), "file000") {
		t.Fatal("first entry is not on the opening screen")
	}

	// Bottom of the list: the window has to have moved with the cursor.
	m = press(t, m, "G")
	view := m.View()
	if !strings.Contains(view, "> file499") {
		t.Errorf("after G, the cursor is not on the last entry:\n%s", view)
	}
	if strings.Contains(view, "file000") {
		t.Error("after G, the first entry is still on screen")
	}
	if !strings.Contains(view, "of 500") {
		t.Error("the title is missing its position counter")
	}

	// ...and back to the top.
	m = press(t, m, "g")
	if view := m.View(); !strings.Contains(view, "> file000") {
		t.Errorf("after g, the cursor is not on the first entry:\n%s", view)
	}
}

func TestBrowseScrollsOnlyWhenTheCursorLeavesTheWindow(t *testing.T) {
	m := browseModel(500, 80, 24)
	rows := m.browseRows()

	// One short of the bottom row: nothing has scrolled yet.
	m = press(t, m, strings.Split(strings.Repeat("j", rows-1), "")...)
	if m.browseOffset != 0 {
		t.Errorf("offset is %d after %d moves inside the window, want 0", m.browseOffset, rows-1)
	}

	// One more, and the window slides by exactly one row.
	m = press(t, m, "j")
	if m.browseOffset != 1 {
		t.Errorf("offset is %d after stepping off the bottom, want 1", m.browseOffset)
	}
}

// Growing the terminal shows more of the list; shrinking it keeps the cursor
// on screen rather than leaving the window behind.
func TestResizeRelaidsTheList(t *testing.T) {
	m := browseModel(500, 80, 24)
	m = press(t, m, "G")

	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 60})
	m = next.(model)
	if m.browseOffset != len(m.dirEntries)-m.browseRows() {
		t.Errorf("after growing, offset is %d, want %d", m.browseOffset, len(m.dirEntries)-m.browseRows())
	}
	if lines := strings.Split(m.View(), "\n"); len(lines) != 60 {
		t.Errorf("after growing, rendered %d lines, want 60", len(lines))
	}

	next, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	m = next.(model)
	if !strings.Contains(m.View(), "> file499") {
		t.Error("after shrinking, the cursor scrolled off screen")
	}
}

// The library list draws two rows per library, so its window is measured in
// libraries rather than rows.
func TestLibrariesListWindowsByLibrary(t *testing.T) {
	libraries := make([]client.Library, 40)
	for i := range libraries {
		libraries[i] = client.Library{ID: fmt.Sprintf("id-%03d", i), Name: fmt.Sprintf("lib%03d", i)}
	}
	m := model{view: viewLibraries, libraries: libraries, serverURL: "http://localhost:8082", width: 80, height: 24}

	m = press(t, m, "G")
	view := m.View()
	if !strings.Contains(view, "> lib039") || !strings.Contains(view, "id-039") {
		t.Errorf("the last library and its id are not both on screen:\n%s", view)
	}
	if lines := strings.Split(view, "\n"); len(lines) != 24 {
		t.Errorf("rendered %d lines, want 24", len(lines))
	}
}

func TestEmptyListsRenderAndDoNotScroll(t *testing.T) {
	m := browseModel(0, 80, 24)
	m = press(t, m, "j", "G", "pgdown")
	if m.browseCursor != 0 || m.browseOffset != 0 {
		t.Errorf("cursor/offset moved in an empty directory: %d/%d", m.browseCursor, m.browseOffset)
	}
	if !strings.Contains(m.View(), "(empty directory)") {
		t.Error("the empty-directory notice is missing")
	}
}
