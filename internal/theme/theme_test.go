package theme

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestPaletteNonNil(t *testing.T) {
	for name, c := range map[string]lipgloss.TerminalColor{
		"Grey":   Grey,
		"Blue":   Blue,
		"Orange": Orange,
		"Green":  Green,
		"Red":    Red,
	} {
		if c == nil {
			t.Errorf("%s palette color is nil", name)
		}
	}
}

func TestStylesRender(t *testing.T) {
	for name, s := range map[string]lipgloss.Style{
		"Header": Header,
		"Faint":  Faint,
		"Box":    Box,
	} {
		if got := s.Render("sample"); got == "" {
			t.Errorf("%s rendered an empty string", name)
		}
	}
}
