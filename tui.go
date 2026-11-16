package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	timelineCap        = 2000
	tuiSinkBuffer      = 1024
	noSelection        = -1
	broadcastSelection = 0
)

type timelineKind int

const (
	itemSystem timelineKind = iota
	itemFrame
)

type timelineItem struct {
	kind     timelineKind
	time     time.Time
	addr     string
	incoming bool
	message  string
	raw      []byte
	text     string
}

type frameMsg timelineItem
type systemMsg string
type clientConnectedMsg string
type clientDisconnectedMsg string
type peerClosedMsg struct{ err error }
type sendFailedMsg struct{ err error }
type idleTickMsg time.Time

func runIO(fn func()) tea.Cmd {
	return func() tea.Msg {
		fn()
		return nil
	}
}

// tuiSink forwards network events to the Bubble Tea program in the order they
// were produced. A single goroutine drains a bounded queue, so a slow UI
// applies backpressure to the connection goroutines instead of piling up
// goroutines or reordering frames.
//
// send blocks while the queue is full, so it must never be called from
// Update or View; call it from connection goroutines or tea.Cmd functions.
type tuiSink struct {
	ch       chan tea.Msg
	stop     chan struct{}
	stopOnce sync.Once
}

func newTUISink() *tuiSink {
	return &tuiSink{
		ch:   make(chan tea.Msg, tuiSinkBuffer),
		stop: make(chan struct{}),
	}
}

func (s *tuiSink) start(p *tea.Program) {
	go func() {
		for {
			select {
			case msg := <-s.ch:
				p.Send(msg) // returns immediately once the program has exited
			case <-s.stop:
				return
			}
		}
	}()
}

// close turns later sends into no-ops.
func (s *tuiSink) close() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *tuiSink) send(msg tea.Msg) {
	select {
	case s.ch <- msg:
	case <-s.stop:
	}
}

func (s *tuiSink) printf(format string, args ...any) {
	msg := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	if msg == "" {
		return
	}
	s.send(systemMsg(msg))
}

func (s *tuiSink) serverFrame(addr, verb, _ string, message string, raw []byte) {
	s.send(frameMsg{
		kind:     itemFrame,
		time:     time.Now(),
		addr:     addr,
		incoming: verb == "Received",
		message:  message,
		raw:      append([]byte(nil), raw...),
	})
}

func (s *tuiSink) clientFrame(tag, _ string, message string, raw []byte) {
	s.send(frameMsg{
		kind:     itemFrame,
		time:     time.Now(),
		addr:     tag,
		incoming: tag == "Recv",
		message:  message,
		raw:      append([]byte(nil), raw...),
	})
}

func (s *tuiSink) clientConnected(addr string) {
	s.send(clientConnectedMsg(addr))
}

func (s *tuiSink) clientDisconnected(addr string) {
	s.send(clientDisconnectedMsg(addr))
}

// hexBytesPerRow picks the widest hex dump row that fits in width columns.
// A row of n bytes takes 9+4n columns, plus one for the middle gap at 16.
func hexBytesPerRow(width int) int {
	switch {
	case width <= 0 || width >= 74:
		return 16
	case width >= 41:
		return 8
	default:
		return 4
	}
}

func formatHexDump(raw []byte) string {
	return formatHexDumpN(raw, 16)
}

func formatHexDumpN(raw []byte, perRow int) string {
	if len(raw) == 0 {
		return ""
	}
	if perRow <= 0 {
		perRow = 16
	}
	var b strings.Builder
	for i := 0; i < len(raw); i += perRow {
		end := min(i+perRow, len(raw))
		chunk := raw[i:end]
		fmt.Fprintf(&b, "%04x  ", i)
		for j := 0; j < perRow; j++ {
			if perRow > 8 && j == perRow/2 {
				b.WriteByte(' ')
			}
			if j < len(chunk) {
				fmt.Fprintf(&b, "%02x ", chunk[j])
			} else {
				b.WriteString("   ")
			}
		}
		b.WriteString(" |")
		for _, c := range chunk {
			if c < 32 || c > 126 {
				b.WriteByte('.')
			} else {
				b.WriteByte(c)
			}
		}
		b.WriteByte('|')
		if end < len(raw) {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return string(r[0])
	}
	return string(r[:max-1]) + "…"
}

func cycleIndex(selected, n, delta int) int {
	if n <= 0 {
		return 0
	}
	selected = (selected + delta) % n
	if selected < 0 {
		selected += n
	}
	return selected
}

func formatIdle(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	if d < time.Second {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func colorStyle(c lipgloss.Color) lipgloss.Style {
	s := lipgloss.NewStyle()
	if colorEnabled {
		s = s.Foreground(c)
	}
	return s
}

func borderColor(s lipgloss.Style, c lipgloss.Color) lipgloss.Style {
	if colorEnabled {
		s = s.BorderForeground(c)
	}
	return s
}

func newTUIInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Placeholder = "message or #command"
	ti.Focus()
	return ti
}

func tickIdle() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return idleTickMsg(t)
	})
}

// wrapTo hard-wraps s to width columns (ANSI- and wide-character aware) so
// long frames are not cut off by the viewport. width <= 0 disables wrapping.
func wrapTo(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Hardwrap(s, width, true)
}

func renderTimelineItem(item timelineItem, showHex bool, width int) string {
	ts := colorStyle(lipgloss.Color("11")).Render(item.time.Format("15:04:05.000"))
	if item.kind == itemSystem {
		return wrapTo(ts+"  "+displayText(item.text), width)
	}
	arrow := "→"
	arrowStyle := colorStyle(lipgloss.Color("9"))
	if item.incoming {
		arrow = "←"
		arrowStyle = colorStyle(lipgloss.Color("10"))
	}
	addr := colorStyle(lipgloss.Color("12")).Render(item.addr)
	msg := displayText(item.message)
	count := colorStyle(lipgloss.Color("14")).Render(fmt.Sprintf("%dB", len(item.raw)))
	line := wrapTo(fmt.Sprintf("%s  %s %s  %s  (%s)", ts, arrowStyle.Render(arrow), addr, msg, count), width)
	if showHex && len(item.raw) > 0 {
		dump := formatHexDumpN(item.raw, hexBytesPerRow(width))
		line += "\n" + wrapTo(colorStyle(lipgloss.Color("13")).Render(dump), width)
	}
	return line
}

type tuiChrome struct {
	width        int
	height       int
	input        textinput.Model
	viewport     viewport.Model
	ready        bool
	showHex      bool
	showHelp     bool
	follow       bool
	timeline     []timelineItem
	rendered     []string // rendered[i] caches timeline[i] at contentWidth/showHex
	contentWidth int
	history      []string
	historyIdx   int
}

func newTUIChrome() tuiChrome {
	return tuiChrome{
		input:    newTUIInput(),
		viewport: viewport.New(1, 1),
		follow:   true,
	}
}

func (c *tuiChrome) appendItem(item timelineItem) {
	c.timeline = append(c.timeline, item)
	c.rendered = append(c.rendered, renderTimelineItem(item, c.showHex, c.contentWidth))
	if over := len(c.timeline) - timelineCap; over > 0 {
		c.timeline = c.timeline[over:]
		c.rendered = c.rendered[over:]
	}
	c.syncViewport()
}

func (c *tuiChrome) appendSystem(text string) {
	c.appendItem(timelineItem{
		kind: itemSystem,
		time: time.Now(),
		text: text,
	})
}

// rerender rebuilds every cached line; needed only when the width or the hex
// setting changes. Appends render just the new item.
func (c *tuiChrome) rerender() {
	c.rendered = c.rendered[:0]
	for _, item := range c.timeline {
		c.rendered = append(c.rendered, renderTimelineItem(item, c.showHex, c.contentWidth))
	}
	c.syncViewport()
}

func (c *tuiChrome) syncViewport() {
	if !c.ready {
		return
	}
	stayBottom := c.follow || c.viewport.AtBottom()
	if len(c.rendered) == 0 {
		c.viewport.SetContent(colorStyle(lipgloss.Color("8")).Render("waiting for frames…"))
	} else {
		c.viewport.SetContent(strings.Join(c.rendered, "\n"))
	}
	if stayBottom {
		c.viewport.GotoBottom()
		c.follow = true
	}
}

func (c *tuiChrome) recallHistory(delta int) {
	if len(c.history) == 0 {
		return
	}
	c.historyIdx += delta
	if c.historyIdx < 0 {
		c.historyIdx = 0
	}
	if c.historyIdx >= len(c.history) {
		c.historyIdx = len(c.history)
		c.input.SetValue("")
		c.input.CursorEnd()
		return
	}
	c.input.SetValue(c.history[c.historyIdx])
	c.input.CursorEnd()
}

func (c *tuiChrome) remember(line string) {
	if line == "" {
		return
	}
	if n := len(c.history); n > 0 && c.history[n-1] == line {
		c.historyIdx = n
		return
	}
	c.history = append(c.history, line)
	c.historyIdx = len(c.history)
}

func (c *tuiChrome) takeInput() string {
	line := c.input.Value()
	c.input.SetValue("")
	c.input.CursorEnd()
	c.remember(line)
	return line
}

// handleKey processes keys shared by the server and client TUIs. It reports
// whether the key was consumed and whether the program should quit. Help is
// on F1 only: "?" must stay typeable as the first character of a message.
func (c *tuiChrome) handleKey(msg tea.KeyMsg) (handled, quit bool) {
	key := msg.String()
	if key == "ctrl+c" {
		return true, true
	}
	if c.showHelp {
		switch key {
		case "f1", "esc", "q", "?":
			c.showHelp = false
		}
		return true, false
	}
	switch key {
	case "f1":
		c.showHelp = true
	case "f2":
		c.showHex = !c.showHex
		c.rerender()
	case "pgup":
		c.follow = false
		c.viewport.PageUp()
	case "pgdown":
		c.viewport.PageDown()
		if c.viewport.AtBottom() {
			c.follow = true
		}
	case "up":
		c.recallHistory(-1)
	case "down":
		c.recallHistory(1)
	default:
		return false, false
	}
	return true, false
}

func (c *tuiChrome) footer(extra string) string {
	hex := "ascii"
	if c.showHex {
		hex = "hex"
	}
	return colorStyle(lipgloss.Color("8")).Render(
		fmt.Sprintf("F1 help  F2 %s  %sEnter send  Ctrl+C quit", hex, extra))
}

type clientEntry struct {
	addr     string
	frames   int
	lastSeen time.Time
}

type serverTUI struct {
	tuiChrome
	cfg         serverConfig
	clients     *sync.Map
	clientCount *atomic.Int64
	entries     []clientEntry
	selected    int // noSelection, broadcastSelection, or 1+index into entries
	listWidth   int
	listHeight  int
}

func runServerTUI(cfg serverConfig, listener net.Listener) int {
	var clients sync.Map
	var clientCount atomic.Int64
	var shuttingDown atomic.Bool

	m := serverTUI{
		tuiChrome:   newTUIChrome(),
		cfg:         cfg,
		clients:     &clients,
		clientCount: &clientCount,
		selected:    broadcastSelection,
	}
	s := newTUISink()
	p := tea.NewProgram(m, tea.WithAltScreen())
	s.start(p)
	setSink(s)

	go acceptClients(listener, cfg, &clients, &clientCount, &shuttingDown)

	_, err := p.Run()
	// Connection goroutines may outlive the program; keep them off the
	// restored terminal. The process exits right after this returns.
	setSink(quietSink{})
	s.close()
	shuttingDown.Store(true)
	listener.Close()
	clients.Range(func(_, value interface{}) bool {
		value.(net.Conn).Close()
		return true
	})
	if err != nil {
		fmt.Println("TUI error:", err)
		return exitIO
	}
	return exitOK
}

func (m serverTUI) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, tickIdle())
}

func (m serverTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.applySize(msg.Width, msg.Height)
		return m, nil
	case idleTickMsg:
		return m, tickIdle()
	case frameMsg:
		item := timelineItem(msg)
		m.appendItem(item)
		m.touchClient(item.addr)
		return m, nil
	case systemMsg:
		m.appendSystem(string(msg))
		return m, nil
	case clientConnectedMsg:
		m.addClient(string(msg))
		m.appendSystem("connected " + string(msg))
		return m, nil
	case clientDisconnectedMsg:
		addr := string(msg)
		wasTarget := m.isSelected(addr)
		m.removeClient(addr)
		m.appendSystem("disconnected " + addr)
		if wasTarget {
			m.appendSystem("target cleared; Tab to pick another client")
		}
		return m, nil
	case tea.KeyMsg:
		if handled, quit := m.handleKey(msg); handled {
			if quit {
				return m, tea.Quit
			}
			return m, nil
		}
		switch msg.String() {
		case "tab":
			m.cycle(1)
			return m, nil
		case "shift+tab":
			m.cycle(-1)
			return m, nil
		case "enter":
			return m.submit()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *serverTUI) applySize(w, h int) {
	m.width = w
	m.height = h
	m.ready = true
	bodyH := max(4, h-4) // header, target, input, footer
	leftW := 28
	if w < 90 {
		leftW = max(18, w/4)
	}
	rightW := w - leftW - 1
	if rightW < 24 {
		rightW = max(10, w-19)
		leftW = max(10, w-rightW-1)
	}
	m.listWidth = leftW
	m.listHeight = bodyH
	m.viewport.Width = max(1, rightW)
	m.viewport.Height = max(1, bodyH)
	m.input.Width = max(1, w-3)
	m.contentWidth = m.viewport.Width
	m.rerender()
}

func (m *serverTUI) cycle(delta int) {
	n := len(m.entries) + 1
	if m.selected == noSelection {
		if delta > 0 {
			m.selected = broadcastSelection
		} else {
			m.selected = n - 1
		}
		return
	}
	m.selected = cycleIndex(m.selected, n, delta)
}

// addClient never moves the selection: the send target only changes when the
// user presses Tab.
func (m *serverTUI) addClient(addr string) {
	for _, e := range m.entries {
		if e.addr == addr {
			return
		}
	}
	m.entries = append(m.entries, clientEntry{addr: addr, lastSeen: time.Now()})
}

func (m *serverTUI) isSelected(addr string) bool {
	sel, broadcast, ok := m.selectedAddr()
	return ok && !broadcast && sel == addr
}

// removeClient drops addr from the list. If it was the send target, the
// selection becomes empty rather than silently moving to another client.
func (m *serverTUI) removeClient(addr string) {
	removedIndex := -1
	for i, e := range m.entries {
		if e.addr == addr {
			removedIndex = i
			break
		}
	}
	if removedIndex < 0 {
		return
	}
	m.entries = append(m.entries[:removedIndex], m.entries[removedIndex+1:]...)
	switch sel := m.selected - 1; {
	case m.selected <= broadcastSelection:
	case sel == removedIndex:
		m.selected = noSelection
	case sel > removedIndex:
		m.selected--
	}
}

// touchClient updates counters for a listed client. It never adds one: a frame
// logged just after the disconnect event must not resurrect the entry.
func (m *serverTUI) touchClient(addr string) {
	for i := range m.entries {
		if m.entries[i].addr == addr {
			m.entries[i].frames++
			m.entries[i].lastSeen = time.Now()
			return
		}
	}
}

func (m *serverTUI) selectedAddr() (addr string, broadcast bool, ok bool) {
	if m.selected == broadcastSelection {
		return "", true, true
	}
	idx := m.selected - 1
	if idx < 0 || idx >= len(m.entries) {
		return "", false, false
	}
	return m.entries[idx].addr, false, true
}

func (m serverTUI) submit() (tea.Model, tea.Cmd) {
	line := m.input.Value()
	if line == "" {
		return m, nil
	}
	cmd, rest := splitHead(line)
	if strings.HasPrefix(cmd, "#") {
		m.takeInput()
		switch cmd {
		case "#send":
			clientAddr, message := splitHead(rest)
			if clientAddr == "" {
				m.appendSystem("Usage: #send <clientAddr> <message>")
				return m, nil
			}
			return m, runIO(func() {
				sendToClient(m.clients, clientAddr, message, m.cfg.sendTerminatorBytes, m.cfg.writeTimeout)
			})
		case "#broadcast":
			return m, runIO(func() {
				broadcastToAll(m.clients, rest, m.cfg.sendTerminatorBytes, m.cfg.writeTimeout)
			})
		case "#list":
			return m, runIO(func() {
				listClients(m.clients)
			})
		case "#help":
			sub, _ := splitHead(rest)
			if sub == "program" {
				m.appendSystem("program help: run coe --help")
			} else {
				m.showHelp = true
			}
		case "#quit", "#exit":
			return m, tea.Quit
		default:
			m.appendSystem("Unknown command: " + cmd)
			m.appendSystem("Available commands: #send, #broadcast, #list, #help, #quit")
		}
		return m, nil
	}

	addr, broadcast, ok := m.selectedAddr()
	if !ok {
		// Keep the typed line so it can be sent after picking a target.
		m.appendSystem("No client selected. Tab to pick a client, or #broadcast <message>")
		return m, nil
	}
	m.takeInput()
	if broadcast {
		return m, runIO(func() {
			broadcastToAll(m.clients, line, m.cfg.sendTerminatorBytes, m.cfg.writeTimeout)
		})
	}
	return m, runIO(func() {
		sendToClient(m.clients, addr, line, m.cfg.sendTerminatorBytes, m.cfg.writeTimeout)
	})
}

func (m serverTUI) View() string {
	if !m.ready {
		return "starting tui…"
	}
	if m.showHelp {
		return renderHelp(m.width, m.height, serverTUIHelp)
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.clientPane(), m.viewport.View())
	return lipgloss.JoinVertical(lipgloss.Left,
		m.headerView(), body, m.targetView(), m.input.View(), m.footer("Tab client  "))
}

func (m serverTUI) headerView() string {
	echo := "echo OFF"
	if m.cfg.echoEnabled {
		echo = "echo ON"
	}
	var clients string
	if m.cfg.maxClients > 0 {
		clients = fmt.Sprintf("%d/%d", m.clientCount.Load(), m.cfg.maxClients)
	} else {
		clients = fmt.Sprintf("%d", m.clientCount.Load())
	}
	write := "write off"
	if m.cfg.writeTimeout > 0 {
		write = "write " + m.cfg.writeTimeout.String()
	}
	title := colorStyle(lipgloss.Color("14")).Bold(true).Render("coe server")
	return truncateLine(fmt.Sprintf("%s  :%s   TX:%s  RX:%s  %s  clients %s  %s",
		title, m.cfg.port,
		strings.ToUpper(m.cfg.sendTerminatorName),
		strings.ToUpper(m.cfg.recvTerminatorName),
		echo, clients, write), m.width)
}

func (m serverTUI) targetView() string {
	addr, broadcast, ok := m.selectedAddr()
	if !ok {
		return colorStyle(lipgloss.Color("8")).Render("to (none) — Tab to pick a client")
	}
	if broadcast {
		return "to " + colorStyle(lipgloss.Color("11")).Render("* broadcast")
	}
	return "to " + colorStyle(lipgloss.Color("12")).Render(addr)
}

// clientPane lists "* broadcast" and the clients. When the list is taller
// than the pane it scrolls so the selected entry stays visible, and the title
// shows how many entries are hidden above and below.
func (m serverTUI) clientPane() string {
	width := max(10, m.listWidth)
	dim := colorStyle(lipgloss.Color("8"))

	type block []string
	blocks := make([]block, 0, len(m.entries)+1)
	labels := append([]string{"* broadcast"}, make([]string, len(m.entries))...)
	for i, e := range m.entries {
		labels[i+1] = e.addr
	}
	for i, label := range labels {
		marker := "  "
		if i == m.selected {
			marker = "> "
		}
		line := marker + truncateRunes(label, max(1, width-2))
		if i == m.selected && colorEnabled {
			line = lipgloss.NewStyle().Reverse(true).Render(line)
		}
		b := block{line}
		if i > 0 {
			e := m.entries[i-1]
			meta := fmt.Sprintf("    %d f  idle %s", e.frames, formatIdle(e.lastSeen))
			b = append(b, dim.Render(truncateRunes(meta, width)))
		}
		blocks = append(blocks, b)
	}

	avail := max(1, m.listHeight-1) // first line is the title
	focus := max(0, m.selected)
	linesIn := func(from, to int) int {
		n := 0
		for i := from; i <= to; i++ {
			n += len(blocks[i])
		}
		return n
	}
	start := 0
	for start < focus && linesIn(start, focus) > avail {
		start++
	}
	end, used := start, 0
	for end < len(blocks) && used+len(blocks[end]) <= avail {
		used += len(blocks[end])
		end++
	}
	if end == start {
		end = start + 1 // a pane shorter than one entry still shows the focus
	}

	title := "clients"
	if start > 0 || end < len(blocks) {
		title += dim.Render(fmt.Sprintf("  ↑%d ↓%d", start, len(blocks)-end))
	}
	lines := []string{truncateLine(title, width)}
	for _, b := range blocks[start:end] {
		lines = append(lines, b...)
	}
	if len(m.entries) == 0 {
		lines = append(lines, dim.Render("  no clients"))
	}

	style := lipgloss.NewStyle().
		Width(width).
		Height(m.listHeight).
		MaxHeight(m.listHeight).
		Border(lipgloss.NormalBorder(), false, true, false, false)
	return borderColor(style, lipgloss.Color("8")).Render(strings.Join(lines, "\n"))
}

func truncateLine(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

const serverTUIHelp = `coe server TUI

Keys
  Tab / Shift+Tab   select target (* = broadcast)
  Enter             send to the selected target
  F2                toggle hex dump under frames
  F1                toggle this help (Esc / q close it)
  PgUp / PgDn       scroll the frame log
  Up / Down         send history
  Ctrl+C            stop the server

The target never changes by itself. If the selected
client disconnects, the target becomes (none).

Commands (same as the line UI)
  #send <clientAddr> <message>
  #broadcast <message>
  #list
  #help
  #quit / #exit

Escape sequences in messages: \r \n \t \\ \xHH
Control bytes in frames are shown the same way.`

type clientTUI struct {
	tuiChrome
	cfg          clientConfig
	conn         net.Conn
	out          *tuiSink
	disconnected bool
	exitCode     int
	exitNote     string // printed after the alt screen is gone
}

func runClientTUI(cfg clientConfig) int {
	conn, err := net.Dial("tcp", cfg.address)
	if err != nil {
		fmt.Println("Connection error:", err)
		return exitIO
	}
	defer conn.Close()

	var closing atomic.Bool
	s := newTUISink()
	m := clientTUI{
		tuiChrome: newTUIChrome(),
		cfg:       cfg,
		conn:      conn,
		out:       s,
	}
	m.input.Placeholder = "message"
	p := tea.NewProgram(m, tea.WithAltScreen())
	s.start(p)

	go clientTUIReadLoop(conn, cfg, &closing, s)

	final, err := p.Run()
	closing.Store(true)
	conn.Close()
	s.close()
	if err != nil {
		fmt.Println("TUI error:", err)
		return exitIO
	}
	cm, ok := final.(clientTUI)
	if !ok {
		return exitOK
	}
	if cm.exitNote != "" {
		fmt.Println(cm.exitNote)
	}
	return cm.exitCode
}

// clientTUIReadLoop sends frames and the final close event through the same
// ordered sink, so the last frames always appear before "peer closed".
func clientTUIReadLoop(conn net.Conn, cfg clientConfig, closing *atomic.Bool, s *tuiSink) {
	buffer := make([]byte, cfg.bufferSize)
	var messageBuffer bytes.Buffer
	var pendingSkipLF bool

	flushIncomplete := func() {
		if discardOrphanLFAfterCR(&messageBuffer, cfg.recvTerminatorBytes) {
			return
		}
		if messageBuffer.Len() == 0 {
			return
		}
		s.clientFrame("Recv", colorGreen, messageBuffer.String(), messageBuffer.Bytes())
		messageBuffer.Reset()
	}

	for {
		if cfg.flushTimeout > 0 {
			conn.SetReadDeadline(time.Now().Add(cfg.flushTimeout))
		}
		n, err := conn.Read(buffer)
		if n > 0 {
			if frameErr := appendAndFlushBySuffix(&messageBuffer, buffer[:n], cfg.recvTerminatorBytes, &pendingSkipLF, cfg.maxFrameSize, func(message string) {
				fullFrame := append([]byte(message), cfg.recvTerminatorBytes...)
				s.clientFrame("Recv", colorGreen, message, fullFrame)
			}); frameErr != nil {
				if !closing.Load() {
					s.send(peerClosedMsg{err: frameErr})
				}
				return
			}
		}
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			flushIncomplete()
			continue
		}
		if err != nil {
			if closing.Load() {
				return
			}
			flushIncomplete()
			if errors.Is(err, io.EOF) {
				s.send(peerClosedMsg{})
			} else {
				s.send(peerClosedMsg{err: err})
			}
			return
		}
	}
}

func (m clientTUI) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, tickIdle())
}

// markDisconnected keeps the session on screen so the last frames and the
// reason stay readable; the user leaves with Ctrl+C.
func (m *clientTUI) markDisconnected(reason string, code int) {
	if m.disconnected {
		return
	}
	m.disconnected = true
	m.exitCode = code
	m.exitNote = fmt.Sprintf("Disconnected from %s: %s", m.cfg.address, reason)
	m.appendSystem(reason + " — Ctrl+C to exit")
	m.input.Blur()
	m.input.Placeholder = "disconnected — Ctrl+C to exit"
}

func (m clientTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.applySize(msg.Width, msg.Height)
		return m, nil
	case idleTickMsg:
		return m, tickIdle()
	case frameMsg:
		m.appendItem(timelineItem(msg))
		return m, nil
	case systemMsg:
		m.appendSystem(string(msg))
		return m, nil
	case peerClosedMsg:
		if msg.err != nil {
			m.markDisconnected("receive error: "+msg.err.Error(), exitIO)
		} else {
			m.markDisconnected("peer closed the connection", exitOK)
		}
		return m, nil
	case sendFailedMsg:
		m.markDisconnected("send error: "+msg.err.Error(), exitIO)
		return m, nil
	case tea.KeyMsg:
		if handled, quit := m.handleKey(msg); handled {
			if quit {
				return m, tea.Quit
			}
			return m, nil
		}
		if msg.String() == "enter" {
			return m.submit()
		}
		if m.disconnected {
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *clientTUI) applySize(w, h int) {
	m.width = w
	m.height = h
	m.ready = true
	m.viewport.Width = max(1, w)
	m.viewport.Height = max(1, max(3, h-3)) // header, input, footer
	m.input.Width = max(1, w-3)
	m.contentWidth = m.viewport.Width
	m.rerender()
}

func (m clientTUI) submit() (tea.Model, tea.Cmd) {
	if m.disconnected {
		return m, nil
	}
	line := m.takeInput()
	if line == "" {
		return m, nil
	}
	processed := processEscapeSequences(line)
	frame := append([]byte(processed), m.cfg.sendTerminatorBytes...)
	conn, timeout, out := m.conn, m.cfg.writeTimeout, m.out
	// Report through the sink, not as the Cmd result, so the sent frame is
	// queued with the received frames instead of racing them.
	return m, runIO(func() {
		if err := writeConn(conn, frame, timeout); err != nil {
			out.send(sendFailedMsg{err: err})
			return
		}
		out.clientFrame("Send", colorCyan, line, frame)
	})
}

func (m clientTUI) View() string {
	if !m.ready {
		return "starting tui…"
	}
	if m.showHelp {
		return renderHelp(m.width, m.height, clientTUIHelp)
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.headerView(), m.viewport.View(), m.input.View(), m.footer(""))
}

func (m clientTUI) headerView() string {
	title := colorStyle(lipgloss.Color("14")).Bold(true).Render("coe client")
	state := colorStyle(lipgloss.Color("10")).Render("connected")
	if m.disconnected {
		state = colorStyle(lipgloss.Color("9")).Render("disconnected")
	}
	return truncateLine(fmt.Sprintf("%s  %s   TX:%s  RX:%s  %s",
		title, m.cfg.address,
		strings.ToUpper(m.cfg.sendTerminatorName),
		strings.ToUpper(m.cfg.recvTerminatorName),
		state), m.width)
}

const clientTUIHelp = `coe client TUI

Keys
  Enter             send the current line
  F2                toggle hex dump under frames
  F1                toggle this help (Esc / q close it)
  PgUp / PgDn       scroll the frame log
  Up / Down         send history
  Ctrl+C            disconnect and exit

When the peer closes the connection the log stays on
screen; press Ctrl+C to exit.

Escape sequences in messages: \r \n \t \\ \xHH
Control bytes in frames are shown the same way.`

func renderHelp(width, height int, body string) string {
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(min(72, max(40, width-4)))
	box := borderColor(style, lipgloss.Color("14")).Render(body)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}
