package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestFormatHexDump(t *testing.T) {
	got := formatHexDump([]byte("Hello\n"))
	if !strings.Contains(got, "0000  ") {
		t.Fatalf("missing offset: %q", got)
	}
	if !strings.Contains(got, "48 65 6c 6c 6f 0a") {
		t.Fatalf("missing hex bytes: %q", got)
	}
	if !strings.Contains(got, "|Hello.|") {
		t.Fatalf("missing ascii gutter: %q", got)
	}

	if formatHexDump(nil) != "" {
		t.Fatalf("empty dump should be empty")
	}
}

func TestHexDumpRowsFitWidth(t *testing.T) {
	raw := []byte(strings.Repeat("0123456789abcdef", 3))
	for _, width := range []int{30, 45, 51, 74, 120} {
		dump := formatHexDumpN(raw, hexBytesPerRow(width))
		for _, line := range strings.Split(dump, "\n") {
			if w := ansi.StringWidth(line); w > width {
				t.Fatalf("width %d: hex row is %d columns: %q", width, w, line)
			}
		}
	}
}

func TestCycleIndex(t *testing.T) {
	tests := []struct {
		selected, n, delta, want int
	}{
		{selected: 0, n: 3, delta: 1, want: 1},
		{selected: 2, n: 3, delta: 1, want: 0},
		{selected: 0, n: 3, delta: -1, want: 2},
		{selected: 0, n: 0, delta: 1, want: 0},
	}
	for _, tt := range tests {
		if got := cycleIndex(tt.selected, tt.n, tt.delta); got != tt.want {
			t.Fatalf("cycleIndex(%d,%d,%d) = %d, want %d", tt.selected, tt.n, tt.delta, got, tt.want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("abcdef", 4); got != "abc…" {
		t.Fatalf("truncateRunes() = %q", got)
	}
	if got := truncateRunes("ab", 4); got != "ab" {
		t.Fatalf("truncateRunes() = %q", got)
	}
}

func TestRenderTimelineItemIncludesDirection(t *testing.T) {
	colorEnabled = false
	item := timelineItem{
		kind:     itemFrame,
		addr:     "127.0.0.1:9",
		incoming: true,
		message:  "STATUS\r",
		raw:      []byte("STATUS\r\n"),
	}
	got := renderTimelineItem(item, false, 0)
	if !strings.Contains(got, "←") || !strings.Contains(got, `STATUS\r`) || !strings.Contains(got, "8B") {
		t.Fatalf("frame line = %q", got)
	}
	gotHex := renderTimelineItem(item, true, 0)
	if !strings.Contains(gotHex, "53 54 41 54 55 53 0d 0a") {
		t.Fatalf("hex dump missing: %q", gotHex)
	}
}

func TestRenderTimelineItemWrapsToWidth(t *testing.T) {
	colorEnabled = false
	msg := strings.Repeat("A", 100) + "END"
	item := timelineItem{kind: itemFrame, addr: "127.0.0.1:50000", incoming: true, message: msg, raw: []byte(msg + "\n")}
	got := renderTimelineItem(item, true, 51)
	for _, line := range strings.Split(got, "\n") {
		if w := ansi.StringWidth(line); w > 51 {
			t.Fatalf("line is %d columns: %q", w, line)
		}
	}
	if !strings.Contains(strings.ReplaceAll(got, "\n", ""), "END  (104B)") {
		t.Fatalf("tail of message or byte count lost: %q", got)
	}
}

func newTestServerTUI() serverTUI {
	var clients sync.Map
	var count atomic.Int64
	return serverTUI{
		tuiChrome: newTUIChrome(),
		cfg: serverConfig{port: "0", sendTerminatorName: "LF", recvTerminatorName: "LF",
			sendTerminatorBytes: []byte{'\n'}, recvTerminatorBytes: []byte{'\n'}, writeTimeout: time.Second},
		clients:     &clients,
		clientCount: &count,
		selected:    broadcastSelection,
	}
}

func updateServer(t *testing.T, m serverTUI, msgs ...tea.Msg) serverTUI {
	t.Helper()
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		m = next.(serverTUI)
	}
	return m
}

func TestServerTUIFrameAfterDisconnectDoesNotResurrectClient(t *testing.T) {
	m := updateServer(t, newTestServerTUI(),
		clientConnectedMsg("a:1"),
		frameMsg{kind: itemFrame, addr: "a:1", incoming: true, message: "x"},
		clientDisconnectedMsg("a:1"),
		// A "Sent" frame logged by #broadcast right after the disconnect.
		frameMsg{kind: itemFrame, addr: "a:1", message: "late"},
	)
	if len(m.entries) != 0 {
		t.Fatalf("entries = %+v, want none", m.entries)
	}
}

func TestServerTUISelectionNeverMovesOnItsOwn(t *testing.T) {
	m := updateServer(t, newTestServerTUI(), clientConnectedMsg("a:1"))
	if _, broadcast, _ := m.selectedAddr(); !broadcast {
		t.Fatalf("first connection changed the target")
	}

	m = updateServer(t, m, clientConnectedMsg("b:2"), clientConnectedMsg("c:3"))
	m.selected = 2 // b:2
	m = updateServer(t, m, clientDisconnectedMsg("a:1"))
	if addr, _, ok := m.selectedAddr(); !ok || addr != "b:2" {
		t.Fatalf("after removing an earlier entry target = %q ok=%v, want b:2", addr, ok)
	}

	m = updateServer(t, m, clientDisconnectedMsg("b:2"))
	if _, _, ok := m.selectedAddr(); ok {
		t.Fatalf("target should be (none) after the selected client left, got selected=%d", m.selected)
	}

	m.input.SetValue("keep me")
	m = updateServer(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "keep me" {
		t.Fatalf("input was cleared although nothing was sent: %q", m.input.Value())
	}

	m = updateServer(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if _, broadcast, ok := m.selectedAddr(); !ok || !broadcast {
		t.Fatalf("Tab from (none) should pick broadcast")
	}
	m.selected = noSelection
	m = updateServer(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if addr, _, _ := m.selectedAddr(); addr != "c:3" {
		t.Fatalf("Shift+Tab from (none) should pick the last client, got %q", addr)
	}
}

func TestServerTUIClientPaneKeepsSelectionVisible(t *testing.T) {
	colorEnabled = false
	m := updateServer(t, newTestServerTUI(), tea.WindowSizeMsg{Width: 80, Height: 16})
	for i := 0; i < 20; i++ {
		m.addClient(fmt.Sprintf("10.0.0.%d:5000", i))
	}
	for _, sel := range []int{0, 5, 15, 20} {
		m.selected = sel
		pane := m.clientPane()
		label := "* broadcast"
		if sel > 0 {
			label = m.entries[sel-1].addr
		}
		if !strings.Contains(pane, "> "+label) {
			t.Fatalf("selected %q not visible:\n%s", label, pane)
		}
		if lines := strings.Count(pane, "\n") + 1; lines != m.listHeight {
			t.Fatalf("pane has %d lines, want %d", lines, m.listHeight)
		}
	}
	if !strings.Contains(m.clientPane(), "↑") {
		t.Fatalf("scrolled pane should show the hidden count")
	}
}

func TestServerTUIQuestionMarkIsTypeable(t *testing.T) {
	m := updateServer(t, newTestServerTUI(), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	if m.showHelp || m.input.Value() != "?" {
		t.Fatalf("showHelp=%v input=%q, want the ? typed into the input", m.showHelp, m.input.Value())
	}
}

func TestServerTUIViewFitsTerminal(t *testing.T) {
	colorEnabled = true
	m := updateServer(t, newTestServerTUI(), tea.WindowSizeMsg{Width: 80, Height: 20}, clientConnectedMsg("127.0.0.1:50000"))
	for _, hex := range []bool{false, true} {
		if m.showHex != hex {
			m = updateServer(t, m, tea.KeyMsg{Type: tea.KeyF2})
		}
		for i := 0; i < 30; i++ {
			msg := fmt.Sprintf("%02d-%s", i, strings.Repeat("A", 70))
			m = updateServer(t, m, frameMsg{kind: itemFrame, time: time.Now(), addr: "127.0.0.1:50000", incoming: true, message: msg, raw: []byte(msg + "\n")})
		}
		view := m.View()
		lines := strings.Split(view, "\n")
		if len(lines) != 20 {
			t.Fatalf("hex=%v: view has %d lines, want 20", hex, len(lines))
		}
		for _, l := range lines {
			if w := ansi.StringWidth(l); w > 80 {
				t.Fatalf("hex=%v: line is %d columns", hex, w)
			}
		}
		if !strings.Contains(ansi.Strip(view), "(74B)") {
			t.Fatalf("hex=%v: newest frame tail not visible:\n%s", hex, ansi.Strip(view))
		}
	}
}

type frameRecorder struct {
	mu  sync.Mutex
	got []string
}

func (r *frameRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

type recordModel struct{ rec *frameRecorder }

func (m recordModel) Init() tea.Cmd { return nil }
func (m recordModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if f, ok := msg.(frameMsg); ok {
		m.rec.mu.Lock()
		m.rec.got = append(m.rec.got, f.message)
		m.rec.mu.Unlock()
	}
	return m, nil
}
func (m recordModel) View() string { return "" }

func TestTUISinkPreservesOrder(t *testing.T) {
	rec := &frameRecorder{}
	p := tea.NewProgram(recordModel{rec: rec}, tea.WithInput(nil), tea.WithoutRenderer())
	s := newTUISink()
	s.start(p)
	done := make(chan struct{})
	go func() { _, _ = p.Run(); close(done) }()

	const n = 3000 // more than the queue holds, to exercise backpressure
	for i := 0; i < n; i++ {
		s.serverFrame("a:1", "Received", "", strconv.Itoa(i), nil)
	}
	var got []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got = rec.snapshot(); len(got) == n || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.Quit()
	<-done
	s.close()

	if len(got) != n {
		t.Fatalf("delivered %d of %d frames", len(got), n)
	}
	for i, v := range got {
		if v != strconv.Itoa(i) {
			t.Fatalf("frame %d is %q: delivered out of order", i, v)
		}
	}
	// Sends after close must not block.
	s.printf("after close")
}

func TestClientTUIStaysOpenAfterPeerClose(t *testing.T) {
	m := clientTUI{tuiChrome: newTUIChrome(), cfg: clientConfig{address: "127.0.0.1:1"}}
	next, cmd := m.Update(peerClosedMsg{})
	m = next.(clientTUI)
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Fatalf("peer close quit the TUI")
		}
	}
	if !m.disconnected || m.exitCode != exitOK || !strings.Contains(m.exitNote, "peer closed") {
		t.Fatalf("disconnected=%v exitCode=%d note=%q", m.disconnected, m.exitCode, m.exitNote)
	}

	next, _ = m.Update(peerClosedMsg{err: errors.New("boom")})
	if got := next.(clientTUI).exitCode; got != exitOK {
		t.Fatalf("second close event changed the exit code to %d", got)
	}

	m2 := clientTUI{tuiChrome: newTUIChrome(), cfg: clientConfig{address: "127.0.0.1:1"}}
	next, _ = m2.Update(sendFailedMsg{err: errors.New("broken pipe")})
	if got := next.(clientTUI); !got.disconnected || got.exitCode != exitIO {
		t.Fatalf("send failure: disconnected=%v exitCode=%d", got.disconnected, got.exitCode)
	}

	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatalf("Ctrl+C should quit")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Fatalf("Ctrl+C should quit")
	}
}
