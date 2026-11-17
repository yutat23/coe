package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/term"
)

type uiSink interface {
	printf(format string, args ...any)
	serverFrame(addr, verb, verbColor, message string, raw []byte)
	clientFrame(tag, tagColor, message string, raw []byte)
	clientConnected(addr string)
	clientDisconnected(addr string)
}

// cliSink prints events for the line-oriented UIs. Every write goes through
// lineOutput so that asynchronous events do not land in the middle of the
// prompt the user is typing on.
type cliSink struct{}

func (cliSink) printf(format string, args ...any) {
	logEventf(format, args...)
	lineOutput(func() { fmt.Printf(format, args...) })
}

func (cliSink) serverFrame(addr, verb, verbColor, message string, raw []byte) {
	lineOutput(func() { logServerData(addr, verb, verbColor, message, raw) })
}

func (cliSink) clientFrame(tag, tagColor, message string, raw []byte) {
	lineOutput(func() { logClientData(tag, tagColor, message, raw) })
}

func (cliSink) clientConnected(addr string) {
	logEventf("Client connected: %s", addr)
	lineOutput(func() { fmt.Printf("Client connected: %s\n", addr) })
}

func (cliSink) clientDisconnected(addr string) {
	logEventf("Client disconnected: %s", addr)
	lineOutput(func() { fmt.Printf("Client disconnected: %s\n", addr) })
}

// quietSink discards everything; used after the TUI exits so connection
// goroutines that are still winding down do not print over the shell.
type quietSink struct{}

func (quietSink) printf(string, ...any)                              {}
func (quietSink) serverFrame(string, string, string, string, []byte) {}
func (quietSink) clientFrame(string, string, string, []byte)         {}
func (quietSink) clientConnected(string)                             {}
func (quietSink) clientDisconnected(string)                          {}

// sinkBox wraps the interface so it can be stored in an atomic.Pointer:
// connection goroutines may still be logging while the TUI swaps the sink.
type sinkBox struct{ uiSink }

var activeSinkPtr atomic.Pointer[sinkBox]

func init() {
	setSink(cliSink{})
}

// sink returns the current output destination for server/client events.
func sink() uiSink {
	return activeSinkPtr.Load().uiSink
}

// setSink replaces the output destination and returns the previous one.
func setSink(s uiSink) uiSink {
	if old := activeSinkPtr.Swap(&sinkBox{s}); old != nil {
		return old.uiSink
	}
	return nil
}

type serverConfig struct {
	host                string // empty listens on every interface
	port                string
	sendTerminatorName  string
	recvTerminatorName  string
	sendTerminatorBytes []byte
	recvTerminatorBytes []byte
	echoEnabled         bool
	bufferSize          int
	maxFrameSize        int
	maxClients          int
	writeTimeout        time.Duration
	flushTimeout        time.Duration
	tui                 bool
}

// isTerminal reports whether f is an interactive terminal. A plain
// ModeCharDevice check also accepts /dev/null, which is not a terminal.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(f.Fd())
}

func stdinIsTerminal() bool {
	return isTerminal(os.Stdin)
}

func stdoutIsTerminal() bool {
	return isTerminal(os.Stdout)
}

func tuiAvailable() bool {
	return stdinIsTerminal() && stdoutIsTerminal()
}

// colorChoice records --color / --no-color; the last one given wins.
type colorChoice int

const (
	colorAuto colorChoice = iota
	colorOn
	colorOff
)

// resolveColor decides whether log lines written to out are colored. Without
// a flag, color is used only on a terminal that understands escape sequences
// and when NO_COLOR (https://no-color.org) is unset or empty.
func resolveColor(choice colorChoice, out *os.File) bool {
	switch choice {
	case colorOn:
		enableVirtualTerminal(out)
		return true
	case colorOff:
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isTerminal(out) && enableVirtualTerminal(out)
}

// promptRedrawAvailable reports whether the line UIs may erase and redraw the
// prompt line: input and output must both be a terminal that handles VT
// sequences, otherwise the escape codes would end up in a file or pipe.
func promptRedrawAvailable() bool {
	return stdinIsTerminal() && stdoutIsTerminal() && enableVirtualTerminal(os.Stdout)
}

// lineUI serializes stdout in the line-oriented modes and keeps the input
// prompt on the last line. When the prompt is showing, asynchronous output
// erases it, prints, and draws it again.
var lineUI struct {
	mu      sync.Mutex
	prompt  string
	visible bool
	// redrawn is set when output redrew the prompt. If that happened just
	// after the user pressed Enter, the redrawn prompt sits on the fresh
	// line, and promptConsumed must erase it.
	redrawn bool
}

func clearInputLine() {
	if terminalControlEnabled {
		fmt.Print("\r\033[K")
	}
}

// setPrompt sets the prompt text drawn by showPrompt and lineOutput.
func setPrompt(prompt string) {
	lineUI.mu.Lock()
	lineUI.prompt = prompt
	lineUI.mu.Unlock()
}

// lineOutput runs fn with exclusive access to stdout.
func lineOutput(fn func()) {
	lineUI.mu.Lock()
	defer lineUI.mu.Unlock()
	if lineUI.visible {
		clearInputLine()
	}
	fn()
	if lineUI.visible {
		fmt.Print(lineUI.prompt)
		lineUI.redrawn = true
	}
}

// showPrompt draws the prompt when the program is ready for the next line.
func showPrompt() {
	lineUI.mu.Lock()
	defer lineUI.mu.Unlock()
	if !terminalControlEnabled || lineUI.visible {
		return
	}
	fmt.Print(lineUI.prompt)
	lineUI.visible = true
	lineUI.redrawn = false
}

// hidePrompt erases the prompt before a final message so the shell prompt
// does not follow a dangling "Send> " or "Command> ".
func hidePrompt() {
	lineUI.mu.Lock()
	defer lineUI.mu.Unlock()
	if lineUI.visible {
		clearInputLine()
		lineUI.visible = false
	}
}

// promptConsumed records that the user pressed Enter: the prompt line has
// scrolled up and must not be erased by the next output.
//
// Output may have redrawn the prompt between Enter and this call; that copy
// is on the current line and is erased. If the redraw happened earlier, the
// current line is the empty one after Enter and erasing it is harmless.
func promptConsumed() {
	lineUI.mu.Lock()
	defer lineUI.mu.Unlock()
	if lineUI.redrawn {
		clearInputLine()
	}
	lineUI.visible = false
	lineUI.redrawn = false
}

// fileLog mirrors frames and connection events to the --log file without
// colors. Writes are unbuffered so the file is complete if the process is
// killed.
var fileLog struct {
	mu sync.Mutex
	w  io.Writer
}

func openLogFile(path string) (io.Closer, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	setLogWriter(f)
	return f, nil
}

func setLogWriter(w io.Writer) {
	fileLog.mu.Lock()
	fileLog.w = w
	fileLog.mu.Unlock()
}

func logLine(line string) {
	fileLog.mu.Lock()
	defer fileLog.mu.Unlock()
	if fileLog.w != nil {
		fmt.Fprintln(fileLog.w, line)
	}
}

func logTimestamp() string {
	return time.Now().Format("2006-01-02 15:04:05.000")
}

// logEventf writes a timestamped event line to the --log file, if any.
func logEventf(format string, args ...any) {
	fileLog.mu.Lock()
	enabled := fileLog.w != nil
	fileLog.mu.Unlock()
	if !enabled {
		return
	}
	text := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	if text == "" {
		return
	}
	logLine(logTimestamp() + " | " + displayText(text))
}

// errorf reports a failure on stderr so that it is not mixed into stdout
// output that scripts may be parsing.
func errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}
