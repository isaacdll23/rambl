// Package theme centralizes the rambl TUI palette and shared base styles.
// Colors are adaptive (light/dark) so the dashboard and picker read on any
// terminal background. Typed as lipgloss.TerminalColor so they drop directly
// into Foreground(...) calls and into map[...]lipgloss.TerminalColor lookups.
package theme

import "github.com/charmbracelet/lipgloss"

// Palette — adaptive status/brand colors. The Dark values match the codes the
// TUI used before this package existed (grey 245, blue 39, orange 214,
// green 42, red 203); the Light values are darker variants that stay legible
// on a light background.
var (
	Grey   lipgloss.TerminalColor = lipgloss.AdaptiveColor{Light: "240", Dark: "245"}
	Blue   lipgloss.TerminalColor = lipgloss.AdaptiveColor{Light: "27", Dark: "39"}
	Orange lipgloss.TerminalColor = lipgloss.AdaptiveColor{Light: "166", Dark: "214"}
	Green  lipgloss.TerminalColor = lipgloss.AdaptiveColor{Light: "28", Dark: "42"}
	Red    lipgloss.TerminalColor = lipgloss.AdaptiveColor{Light: "160", Dark: "203"}
)

// Base styles shared across surfaces.
var (
	// Header is a bold heading style.
	Header = lipgloss.NewStyle().Bold(true)
	// Faint is a dimmed style for secondary text.
	Faint = lipgloss.NewStyle().Faint(true)
	// Box is a rounded border with light horizontal padding, used to frame
	// dashboard sections. Callers set .Width(...) as needed.
	Box = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)
