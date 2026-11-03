package tui

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/neftaliacosta/paola/internal/orchestrator"
)

type lineMsg string
type progressMsg orchestrator.Progress
type doneMsg struct{}
type clockMsg time.Time

var logTokenRE = regexp.MustCompile(`//.*|#.*|"(?:\\.|[^"])*"|'(?:\\.|[^'])*'|\b(?:class|function|public|private|protected|namespace|use|return|if|else|elseif|foreach|for|new|extends|implements|const|var|let|interface|true|false|null)\b|\b\d+(?:\.\d+)?\b`)

var (
	logAddStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("#A6E22E"))
	logRemoveStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#F92672"))
	logHeaderStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#66D9EF")).Bold(true)
	logWarningStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#E6DB74"))
	logErrorStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#F92672")).Bold(true)
	logKeywordStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#F92672"))
	logStringStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#E6DB74"))
	logCommentStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#75715E"))
	logNumberStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#AE81FF"))
	logTypeStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#66D9EF"))
	roadmapPendingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#C5C8C6"))
	roadmapActiveStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#A6E22E")).Bold(true)
	// Avoid Strikethrough here: some macOS terminal renderers extend its ANSI
	// decoration across the complete row and draw distracting horizontal lines.
	roadmapDoneStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#75715E")).Faint(true)
)

type model struct {
	viewport     viewport.Model
	roadmap      viewport.Model
	roadmapFocus bool
	lines        []string
	title        string
	progress     orchestrator.Progress
	control      *orchestrator.Control
	width        int
	height       int
}

func New(title string, control *orchestrator.Control) *tea.Program {
	m := model{
		title:    title,
		control:  control,
		width:    100,
		height:   30,
		viewport: viewport.New(70, 20),
		roadmap:  viewport.New(28, 20),
	}
	return tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
}

func Log(p *tea.Program, s string)                            { p.Send(lineMsg(s)) }
func UpdateState(p *tea.Program, state orchestrator.Progress) { p.Send(progressMsg(state)) }
func Finish(p *tea.Program)                                   { p.Send(doneMsg{}) }
func (m model) Init() tea.Cmd                                 { return clockTick() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case lineMsg:
		m.lines = append(m.lines, string(v))
		m.viewport.SetContent(renderLogLines(m.lines))
		m.viewport.GotoBottom()
	case progressMsg:
		m.progress = orchestrator.Progress(v)
		m.refreshRoadmap()
	case doneMsg:
		return m, tea.Quit
	case clockMsg:
		return m, clockTick()
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
		leftWidth := min(34, max(22, v.Width/3))
		m.roadmap.Width = max(10, leftWidth-4)
		m.roadmap.Height = max(6, v.Height-8)
		m.viewport.Width = max(20, v.Width-leftWidth-8)
		m.viewport.Height = max(6, v.Height-8)
		m.refreshRoadmap()
	case tea.KeyMsg:
		switch v.String() {
		case "q", "ctrl+c":
			if m.control != nil {
				m.control.Quit()
			}
			return m, tea.Quit
		case "p":
			if m.control != nil {
				m.progress.Paused = m.control.TogglePause()
			}
		case "s":
			if m.control != nil {
				m.control.Skip()
			}
		case "tab", "shift+tab":
			m.roadmapFocus = !m.roadmapFocus
			return m, nil
		}
		// Navigation keys are sent to the panel selected with Tab.
		var cmd tea.Cmd
		if m.roadmapFocus {
			m.roadmap, cmd = m.roadmap.Update(msg)
		} else {
			m.viewport, cmd = m.viewport.Update(msg)
		}
		return m, cmd
	default:
		var cmd tea.Cmd
		if m.roadmapFocus {
			m.roadmap, cmd = m.roadmap.Update(msg)
		} else {
			m.viewport, cmd = m.viewport.Update(msg)
		}
		return m, cmd
	}
	return m, nil
}

func clockTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return clockMsg(t) })
}

func (m model) View() string {
	project := m.progress.Project
	if project == "" {
		project = m.title
	}
	role := m.progress.Role
	if role == "" {
		role = "idle"
	}
	header := fmt.Sprintf("PAOLA · %s   Task %d/%d   Attempt %d/%d   Role: %s (%s)", project, m.progress.CurrentTask, m.progress.TotalTasks, m.progress.Attempt, m.progress.MaxRetries, role, m.progress.CLI)
	badges := fmt.Sprintf("Coder: %s  |  Reviewer: %s  |  Judge: %s", m.progress.RoleAgents["coder"], m.progress.RoleAgents["reviewer"], m.progress.RoleAgents["judge"])
	headerView := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render(header + "\n" + badges)
	leftWidth := max(18, min(32, m.width/3))
	if m.roadmap.Width == 0 {
		m.roadmap.Width = max(10, leftWidth-4)
		m.roadmap.Height = max(6, m.height-8)
		m.refreshRoadmap()
	}
	panelBorder := lipgloss.Color("#3A3D41")
	left := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(panelBorder).Padding(1).Width(leftWidth).Render(m.roadmap.View())
	right := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(panelBorder).Padding(1).Width(max(20, m.width-leftWidth-8)).Render(m.viewport.View())
	status := "RUNNING"
	if m.progress.Paused {
		status = "PAUSED"
	}
	if m.progress.Verdict != "" {
		status += " · " + m.progress.Verdict
	}
	focus := "logs"
	if m.roadmapFocus {
		focus = "roadmap"
	}
	footer := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render(status + " · [p] Pause/Resume  [s] Skip Task  [q] Safe Quit · [tab] focus: " + focus + " · " + time.Now().Format("15:04:05"))
	return headerView + "\n\n" + lipgloss.JoinHorizontal(lipgloss.Top, left, right) + "\n\n" + footer
}

func (m *model) refreshRoadmap() {
	if m.roadmap.Width <= 0 {
		return
	}
	m.roadmap.SetContent(renderRoadmap(m.progress.Tasks, m.roadmap.Width))
}

func renderRoadmap(tasks []orchestrator.Task, width int) string {
	var roadmap strings.Builder
	roadmap.WriteString("ROADMAP\n\n")
	for _, task := range tasks {
		prefix := "[ ] "
		style := roadmapPendingStyle
		switch task.Status {
		case "done":
			prefix = "✓ "
			style = roadmapDoneStyle
		case "current":
			prefix = "[▶] "
			style = roadmapActiveStyle
		}
		roadmap.WriteString(renderRoadmapTask(prefix, task.Description, width, style))
	}
	return roadmap.String()
}

func renderRoadmapTask(prefix, description string, width int, style lipgloss.Style) string {
	words := strings.Fields(description)
	if len(words) == 0 {
		return style.Render(prefix) + "\n"
	}

	firstWidth := max(1, width-lipgloss.Width(prefix))
	continuationWidth := max(1, width-3)
	lines := wrapWords(words, firstWidth, continuationWidth)
	for i, line := range lines {
		if i == 0 {
			lines[i] = prefix + line
		} else {
			lines[i] = "   " + line
		}
		lines[i] = style.Render(lines[i])
	}
	return strings.Join(lines, "\n") + "\n"
}

func wrapWords(words []string, firstWidth, continuationWidth int) []string {
	lines := []string{}
	line := ""
	limit := firstWidth
	for _, word := range words {
		separator := ""
		if line != "" {
			separator = " "
		}
		if line != "" && len(line)+len(separator)+len(word) > limit {
			lines = append(lines, line)
			line = word
			limit = continuationWidth
			continue
		}
		line += separator + word
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// renderLogLines applies a compact Monokai-inspired palette while preserving
// the raw log text in m.lines for resizing and future redraws.
func renderLogLines(lines []string) string {
	rendered := make([]string, 0, len(lines))
	for _, line := range lines {
		rendered = append(rendered, highlightLogLine(line))
	}
	return strings.Join(rendered, "\n")
}

func highlightLogLine(line string) string {
	switch {
	case strings.HasPrefix(line, "diff --git"), strings.HasPrefix(line, "@@"),
		strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"):
		return logHeaderStyle.Render(line)
	case strings.HasPrefix(line, "+"):
		return logAddStyle.Render(line)
	case strings.HasPrefix(line, "-"):
		return logRemoveStyle.Render(line)
	case containsAnyFold(line, "error", "fatal", "failed", "panic"):
		return logErrorStyle.Render(line)
	case containsAnyFold(line, "warning", "deprecated"):
		return logWarningStyle.Render(line)
	case looksLikeCode(line):
		return highlightCodeLine(line)
	default:
		return line
	}
}

func looksLikeCode(line string) bool {
	return strings.ContainsAny(line, "{};$") || strings.Contains(line, "->") ||
		strings.Contains(line, "<?php") || strings.ContainsAny(line, "=>")
}

func highlightCodeLine(line string) string {
	return logTokenRE.ReplaceAllStringFunc(line, func(token string) string {
		if strings.HasPrefix(token, "//") || strings.HasPrefix(token, "#") {
			return logCommentStyle.Render(token)
		}
		if strings.HasPrefix(token, "\"") || strings.HasPrefix(token, "'") {
			return logStringStyle.Render(token)
		}
		if isLogKeyword(token) {
			return logKeywordStyle.Render(token)
		}
		if token[0] >= '0' && token[0] <= '9' {
			return logNumberStyle.Render(token)
		}
		return logTypeStyle.Render(token)
	})
}

func isLogKeyword(token string) bool {
	switch token {
	case "class", "function", "public", "private", "protected", "namespace", "use", "return", "if", "else", "elseif", "foreach", "for", "new", "extends", "implements", "const", "var", "let", "interface", "true", "false", "null":
		return true
	default:
		return false
	}
}

func containsAnyFold(line string, terms ...string) bool {
	line = strings.ToLower(line)
	for _, term := range terms {
		if strings.Contains(line, term) {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
