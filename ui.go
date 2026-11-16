package main

import (
	"fmt"
	"os"
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

type cliSink struct{}

func (cliSink) printf(format string, args ...any) {
	fmt.Printf(format, args...)
}

func (cliSink) serverFrame(addr, verb, verbColor, message string, raw []byte) {
	logServerData(addr, verb, verbColor, message, raw)
}

func (cliSink) clientFrame(tag, tagColor, message string, raw []byte) {
	logClientData(tag, tagColor, message, raw)
}

func (cliSink) clientConnected(addr string) {
	fmt.Printf("Client connected: %s\n", addr)
}

func (cliSink) clientDisconnected(addr string) {
	fmt.Printf("Client disconnected: %s\n", addr)
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
