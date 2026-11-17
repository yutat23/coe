package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
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
	m = updateServer(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.input.Value() != "keep me" {
		t.Fatalf("input was cleared although nothing was sent: %q", m.input.Value())
	}

	m = updateServer(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if _, broadcast, ok := m.selectedAddr(); !ok || !broadcast {
		t.Fatalf("Tab from (none) should pick broadcast")
	}
	m.selected = noSelection
	m = updateServer(t, m, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
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
	m := updateServer(t, newTestServerTUI(), tea.KeyPressMsg{Code: '?', Text: "?"})
	if m.showHelp || m.input.Value() != "?" {
		t.Fatalf("showHelp=%v input=%q, want the ? typed into the input", m.showHelp, m.input.Value())
	}
}

func TestServerTUIViewFitsTerminal(t *testing.T) {
	colorEnabled = true
	m := updateServer(t, newTestServerTUI(), tea.WindowSizeMsg{Width: 80, Height: 20}, clientConnectedMsg("127.0.0.1:50000"))
	for _, hex := range []bool{false, true} {
		if m.showHex != hex {
			m = updateServer(t, m, tea.KeyPressMsg{Code: tea.KeyF2})
		}
		for i := 0; i < 30; i++ {
			msg := fmt.Sprintf("%02d-%s", i, strings.Repeat("A", 70))
			m = updateServer(t, m, frameMsg{kind: itemFrame, time: time.Now(), addr: "127.0.0.1:50000", incoming: true, message: msg, raw: []byte(msg + "\n")})
		}
		view := m.View().Content
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
func (m recordModel) View() tea.View { return tea.NewView("") }

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

	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatalf("Ctrl+C should quit")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Fatalf("Ctrl+C should quit")
	}
}

func TestIOQueueRunsJobsInSubmitOrder(t *testing.T) {
	q := newIOQueue()
	defer q.close()
	const n = 500
	var mu sync.Mutex
	var got []int
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		q.push(func() {
			mu.Lock()
			got = append(got, i)
			mu.Unlock()
			if i == n-1 {
				close(done)
			}
		})
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queue did not drain")
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("job %d ran as #%d", v, i)
		}
	}
}

func TestClientTUISendsLinesInTypedOrder(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := listener.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer conn.Close()
	peer := <-accepted
	defer peer.Close()

	s := newTUISink()
	defer s.close()
	m := clientTUI{tuiChrome: newTUIChrome(), cfg: clientConfig{address: "test", sendTerminatorBytes: []byte{'\n'}, writeTimeout: time.Second}, conn: conn, out: s}
	defer m.io.close()

	const n = 300 // fewer than the sink buffer, so nothing needs to drain it
	for i := 0; i < n; i++ {
		m.input.SetValue(strconv.Itoa(i))
		next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = next.(clientTUI)
	}
	m.input.SetValue("")
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // empty line: bare terminator
	m = next.(clientTUI)

	reader := bufio.NewReader(peer)
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i <= n; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		want := strconv.Itoa(i) + "\n"
		if i == n {
			want = "\n"
		}
		if line != want {
			t.Fatalf("line %d is %q, want %q: sends were reordered", i, line, want)
		}
	}
	for i := 0; i <= n; i++ {
		select {
		case msg := <-s.ch:
			f, ok := msg.(frameMsg)
			want := strconv.Itoa(i)
			if i == n {
				want = ""
			}
			if !ok || f.incoming || f.message != want {
				t.Fatalf("sink event %d = %#v, want sent frame %q", i, msg, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("sink event %d missing", i)
		}
	}
}

func TestTUIFramesAreClippedForDisplay(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 1<<20)
	s := newTUISink()
	defer s.close()
	s.serverFrame("a:1", "Received", "", string(big), big)
	f := (<-s.ch).(frameMsg)
	if len(f.raw) != tuiFrameKeepBytes || cap(f.raw) != tuiFrameKeepBytes || len(f.message) != tuiFrameKeepBytes {
		t.Fatalf("kept raw len=%d cap=%d message=%d, want %d", len(f.raw), cap(f.raw), len(f.message), tuiFrameKeepBytes)
	}
	if f.size != len(big) || !f.clipped {
		t.Fatalf("size=%d clipped=%v", f.size, f.clipped)
	}
	colorEnabled = false
	line := renderTimelineItem(timelineItem(f), false, 0)
	if !strings.Contains(line, "(1048576B)") || !strings.Contains(line, "first 2048 bytes shown") {
		t.Fatalf("clipped frame line does not say so: %q", line[len(line)-80:])
	}

	// A multi-byte character is not split at the cut.
	msg, _, _, _ := clipFrame(strings.Repeat("あ", tuiFrameKeepBytes), nil)
	if !utf8.ValidString(msg) {
		t.Fatalf("clipped message is not valid UTF-8")
	}
}

func TestTimelineStaysWithinByteBudget(t *testing.T) {
	colorEnabled = false
	c := newTUIChrome()
	defer c.io.close()
	c.ready = true
	c.contentWidth = 80
	c.contentHeight = 20
	c.showHex = true
	raw := bytes.Repeat([]byte("y"), tuiFrameKeepBytes)
	for i := 0; i < timelineCap; i++ {
		c.appendItem(timelineItem{kind: itemFrame, addr: "a:1", message: string(raw), raw: raw, size: 1 << 20, clipped: true})
	}
	if c.storedBytes > timelineByteBudget {
		t.Fatalf("stored %d bytes, budget %d", c.storedBytes, timelineByteBudget)
	}
	stored, lines := 0, 0
	for _, item := range c.timeline {
		stored += item.storedBytes()
	}
	for _, n := range c.itemLines {
		lines += n
	}
	if stored != c.storedBytes || lines != len(c.lines) || len(c.itemLines) != len(c.timeline) {
		t.Fatalf("bookkeeping is off: counted %d bytes, tracked %d; %d of %d lines; %d counts for %d items",
			stored, c.storedBytes, lines, len(c.lines), len(c.itemLines), len(c.timeline))
	}
	if len(c.timeline) >= timelineCap {
		t.Fatalf("large frames were not evicted: %d items", len(c.timeline))
	}

	// Toggling the hex dump or resizing changes only the rendering; no
	// entry may be dropped by it.
	kept := len(c.timeline)
	started := time.Now()
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyF2})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("toggling hex took %s", elapsed)
	}
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyF2})
	c.contentWidth = 41
	c.rerender()
	if len(c.timeline) != kept {
		t.Fatalf("rerendering dropped history: %d of %d entries left", len(c.timeline), kept)
	}
}

func TestPastedLinesAreSentOneByOne(t *testing.T) {
	lines, rest := splitPaste("one\ntwo\r\nthree\rfour")
	if !reflect.DeepEqual(lines, []string{"one", "two", "three"}) || rest != "four" {
		t.Fatalf("splitPaste = %q, %q", lines, rest)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := listener.Accept(); err == nil {
			accepted <- c
		}
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer conn.Close()
	peer := <-accepted
	defer peer.Close()

	s := newTUISink()
	defer s.close()
	m := clientTUI{tuiChrome: newTUIChrome(), cfg: clientConfig{address: "test", sendTerminatorBytes: []byte{'\n'}, writeTimeout: time.Second}, conn: conn, out: s}
	defer m.io.close()
	m.input.SetValue("pre-")
	next, _ := m.Update(tea.PasteMsg{Content: "one\ntwo\r\nthr"})
	m = next.(clientTUI)
	if m.input.Value() != "thr" {
		t.Fatalf("input after paste = %q, want the unfinished line", m.input.Value())
	}
	reader := bufio.NewReader(peer)
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	for _, want := range []string{"pre-one\n", "two\n"} {
		got, err := reader.ReadString('\n')
		if err != nil || got != want {
			t.Fatalf("got %q (%v), want %q", got, err, want)
		}
	}
}

func TestServerTUIDoesNotBroadcastAnEmptyLine(t *testing.T) {
	m := newTestServerTUI()
	defer m.io.close()
	m = updateServer(t, m, tea.WindowSizeMsg{Width: 80, Height: 20}, tea.KeyPressMsg{Code: tea.KeyEnter})
	last := m.timeline[len(m.timeline)-1]
	if last.kind != itemSystem || !strings.Contains(last.text, "Empty line not broadcast") {
		t.Fatalf("last entry = %+v, want the empty-broadcast hint", last)
	}
}

func TestTUIProgramOptionsFollowColorFlags(t *testing.T) {
	savedEnabled, savedMode := colorEnabled, colorMode
	defer func() { colorEnabled, colorMode = savedEnabled, savedMode }()

	colorEnabled, colorMode = false, colorOff
	if len(tuiProgramOptions()) != 1 {
		t.Fatalf("--no-color must pin a color profile")
	}
	colorEnabled, colorMode = true, colorOn
	if len(tuiProgramOptions()) != 1 {
		t.Fatalf("--color must pin a color profile over NO_COLOR")
	}
	colorEnabled, colorMode = true, colorAuto
	if len(tuiProgramOptions()) != 0 {
		t.Fatalf("auto color should let Bubble Tea detect the terminal")
	}
}

func TestWrapKeepsStylesOnEveryLine(t *testing.T) {
	styled := "\x1b[90m" + strings.Repeat("z", 30) + "\x1b[m"
	for _, line := range strings.Split(wrapTo(styled, 10), "\n") {
		if !strings.HasPrefix(line, "\x1b[") || !strings.HasSuffix(line, "m") {
			t.Fatalf("wrapped line lost or leaked its style: %q", line)
		}
		if w := ansi.StringWidth(line); w > 10 {
			t.Fatalf("wrapped line is %d columns", w)
		}
	}
}

func TestClientTUIReadLoopOrdersFramesBeforeClose(t *testing.T) {
	server, client := net.Pipe()
	go func() {
		server.Write([]byte("one\ntwo\npartial"))
		server.Close()
	}()
	s := newTUISink()
	defer s.close()
	var closing atomic.Bool
	cfg := clientConfig{recvTerminatorBytes: []byte{'\n'}, bufferSize: 4}
	done := make(chan struct{})
	go func() {
		clientTUIReadLoop(client, cfg, &closing, s)
		close(done)
	}()
	waitDone(t, done)

	var got []string
	for len(s.ch) > 0 {
		switch msg := (<-s.ch).(type) {
		case frameMsg:
			got = append(got, msg.message)
		case peerClosedMsg:
			if msg.err != nil {
				t.Fatalf("normal close reported as %v", msg.err)
			}
			got = append(got, "<closed>")
		}
	}
	if want := []string{"one", "two", "partial", "<closed>"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestTUIExitErrorTreatsSignalsAsNormalExit(t *testing.T) {
	if tuiExitError(tea.ErrInterrupted) != nil {
		t.Fatalf("SIGINT should exit normally")
	}
	if tuiExitError(nil) != nil {
		t.Fatalf("nil error changed")
	}
	if tuiExitError(tea.ErrProgramKilled) == nil {
		t.Fatalf("a killed program must still report an error")
	}
}

func TestLogViewScrollingKeepsPlaceWhileFramesArrive(t *testing.T) {
	colorEnabled = false
	c := newTUIChrome()
	defer c.io.close()
	c.ready, c.contentWidth, c.contentHeight = true, 40, 5
	add := func(i int) {
		c.appendItem(timelineItem{kind: itemFrame, addr: "a", message: fmt.Sprintf("m%03d", i), raw: []byte("x")})
	}
	for i := 0; i < 50; i++ {
		add(i)
	}
	if !strings.Contains(c.logView(), "m049") {
		t.Fatalf("not following the newest frame:\n%s", c.logView())
	}
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	before := c.logView()
	if strings.Contains(before, "m049") || !strings.Contains(before, "m045") {
		t.Fatalf("PgUp did not scroll back one page:\n%s", before)
	}
	add(50)
	if after := c.logView(); after != before {
		t.Fatalf("a new frame moved the scrolled view:\n%s\nwant\n%s", after, before)
	}
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if c.scroll != 0 || !strings.Contains(c.logView(), "m050") {
		t.Fatalf("PgDown did not return to the newest frame (scroll=%d):\n%s", c.scroll, c.logView())
	}
	for i := 0; i < 100; i++ {
		c.handleKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	if !strings.Contains(c.logView(), "m000") {
		t.Fatalf("PgUp cannot reach the oldest frame:\n%s", c.logView())
	}
	if lines := strings.Split(c.logView(), "\n"); len(lines) != 5 {
		t.Fatalf("log view has %d lines, want 5", len(lines))
	}
}
