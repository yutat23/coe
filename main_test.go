package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type recordingConn struct {
	writes bytes.Buffer
	err    error
}

func (c *recordingConn) Read(_ []byte) (int, error) { return 0, io.EOF }
func (c *recordingConn) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	return c.writes.Write(p)
}
func (c *recordingConn) Close() error                       { return nil }
func (c *recordingConn) LocalAddr() net.Addr                { return dummyAddr("local") }
func (c *recordingConn) RemoteAddr() net.Addr               { return dummyAddr("remote") }
func (c *recordingConn) SetDeadline(_ time.Time) error      { return nil }
func (c *recordingConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *recordingConn) SetWriteDeadline(_ time.Time) error { return nil }

type dummyAddr string

func (a dummyAddr) Network() string { return "test" }
func (a dummyAddr) String() string  { return string(a) }

// startCapture redirects *target to a pipe and drains it on a goroutine.
// Reading only after fn returns would deadlock once the output outgrows the
// pipe buffer, which on Windows is a few kilobytes.
func startCapture(t *testing.T, target **os.File) (stop func() string) {
	t.Helper()

	original := *target
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	*target = w
	done := make(chan []byte, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- out
	}()
	return func() string {
		*target = original
		if err := w.Close(); err != nil {
			t.Fatalf("pipe close error: %v", err)
		}
		out := <-done
		_ = r.Close()
		return string(out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	stop := startCapture(t, &os.Stdout)
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	fn()
	out := stop()
	stop = nil
	return out
}

func captureStdoutAndStderr(t *testing.T, fn func()) (string, string) {
	t.Helper()

	stopOut := startCapture(t, &os.Stdout)
	stopErr := startCapture(t, &os.Stderr)
	finished := false
	defer func() {
		if !finished {
			stopErr()
			stopOut()
		}
	}()
	fn()
	finished = true
	stderr := stopErr()
	stdout := stopOut()
	return stdout, stderr
}

func captureStdoutWithArgs(t *testing.T, args []string, fn func()) string {
	t.Helper()

	originalArgs := os.Args
	os.Args = args
	defer func() {
		os.Args = originalArgs
	}()

	return captureStdout(t, fn)
}

// captureOutputWithArgs runs fn with os.Args set and returns stdout and
// stderr joined, for tests that only care that a message was printed.
func captureOutputWithArgs(t *testing.T, args []string, fn func()) string {
	t.Helper()

	originalArgs := os.Args
	os.Args = args
	defer func() {
		os.Args = originalArgs
	}()

	stdout, stderr := captureStdoutAndStderr(t, fn)
	return stdout + stderr
}

func captureStdoutWithArgsAndStdin(t *testing.T, args []string, stdin *os.File, fn func()) string {
	t.Helper()

	originalArgs := os.Args
	originalStdin := os.Stdin
	os.Args = args
	os.Stdin = stdin
	defer func() {
		os.Args = originalArgs
		os.Stdin = originalStdin
	}()

	return captureStdout(t, fn)
}

func freeTCPPort(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()

	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
}

func waitForTCP(t *testing.T, address string) net.Conn {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err == nil {
			return conn
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %v", address, lastErr)
	return nil
}

func readUntilContains(t *testing.T, conn net.Conn, want string) string {
	t.Helper()

	var buf bytes.Buffer
	tmp := make([]byte, 32)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			t.Fatalf("SetReadDeadline() error: %v", err)
		}
		n, err := conn.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if strings.Contains(buf.String(), want) {
				return buf.String()
			}
		}
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			continue
		}
		if err != nil {
			t.Fatalf("read error before receiving %q: %v; got %q", want, err, buf.String())
		}
	}
	t.Fatalf("timed out waiting for %q; got %q", want, buf.String())
	return ""
}

func TestParseTerminator(t *testing.T) {
	tests := []struct {
		name    string
		want    []byte
		wantErr bool
	}{
		{"LF", []byte{0x0A}, false},
		{"lf", []byte{0x0A}, false},
		{" CR ", []byte{0x0D}, false},
		{"CRLF", []byte{0x0D, 0x0A}, false},
		{"", nil, true},
		{"NL", nil, true},
		{"CR LF", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTerminator(tt.name)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTerminator(%q) succeeded, want error", tt.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTerminator(%q) returned error: %v", tt.name, err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("parseTerminator(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestTerminatorToken(t *testing.T) {
	tests := []struct {
		arg  string
		want bool
	}{
		{"LF", true},
		{"lf", true},
		{" CRLF ", true},
		{"CR", true},
		{"", false},
		{"--no-color", false},
		{"NL", false},
	}

	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			if got := terminatorToken(tt.arg); got != tt.want {
				t.Fatalf("terminatorToken(%q) = %v, want %v", tt.arg, got, tt.want)
			}
		})
	}
}

func TestTerminatorHexDescription(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, ""},
		{"lf", []byte{0x0A}, "0x0A"},
		{"crlf", []byte{0x0D, 0x0A}, "0x0D 0x0A"},
		{"arbitrary", []byte{0x00, 0x7F, 0xFF}, "0x00 0x7F 0xFF"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := terminatorHexDescription(tt.in); got != tt.want {
				t.Fatalf("terminatorHexDescription(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestAppendAndFlushBySuffix(t *testing.T) {
	tests := []struct {
		name          string
		chunks        []string
		recvTerm      []byte
		wantMessages  []string
		wantRemainder string
		wantPendingLF bool
	}{
		{
			name:          "waits for LF across chunks",
			chunks:        []string{"hel", "lo\n"},
			recvTerm:      []byte{'\n'},
			wantMessages:  []string{"hello"},
			wantRemainder: "",
		},
		{
			name:          "emits empty LF frame",
			chunks:        []string{"\n"},
			recvTerm:      []byte{'\n'},
			wantMessages:  []string{""},
			wantRemainder: "",
		},
		{
			name:          "emits multiple LF frames and keeps partial tail",
			chunks:        []string{"one\ntwo\nthr"},
			recvTerm:      []byte{'\n'},
			wantMessages:  []string{"one", "two"},
			wantRemainder: "thr",
		},
		{
			name:          "waits for CRLF across chunks",
			chunks:        []string{"hello\r", "\nworld\r\n"},
			recvTerm:      []byte{'\r', '\n'},
			wantMessages:  []string{"hello", "world"},
			wantRemainder: "",
		},
		{
			name:          "drops LF after CR frames",
			chunks:        []string{"hello\r\nworld\r"},
			recvTerm:      []byte{'\r'},
			wantMessages:  []string{"hello", "world"},
			wantRemainder: "",
			wantPendingLF: true,
		},
		{
			name:          "drops LF after CR across chunks",
			chunks:        []string{"hello\r", "\nworld\r"},
			recvTerm:      []byte{'\r'},
			wantMessages:  []string{"hello", "world"},
			wantRemainder: "",
			wantPendingLF: true,
		},
		{
			name:          "does not drop non LF after CR",
			chunks:        []string{"hello\rX\r"},
			recvTerm:      []byte{'\r'},
			wantMessages:  []string{"hello", "X"},
			wantRemainder: "",
			wantPendingLF: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			var pendingSkipLF bool
			var messages []string

			for _, chunk := range tt.chunks {
				if err := appendAndFlushBySuffix(&buf, []byte(chunk), tt.recvTerm, &pendingSkipLF, 0, func(msg string) {
					messages = append(messages, msg)
				}); err != nil {
					t.Fatalf("appendAndFlushBySuffix() error: %v", err)
				}
			}
			if !reflect.DeepEqual(messages, tt.wantMessages) {
				t.Fatalf("messages = %q, want %q", messages, tt.wantMessages)
			}
			if got := buf.String(); got != tt.wantRemainder {
				t.Fatalf("remainder = %q, want %q", got, tt.wantRemainder)
			}
			if pendingSkipLF != tt.wantPendingLF {
				t.Fatalf("pendingSkipLF = %v, want %v", pendingSkipLF, tt.wantPendingLF)
			}
		})
	}
}

func TestDiscardOrphanLFAfterCR(t *testing.T) {
	tests := []struct {
		name      string
		buffer    string
		recvTerm  []byte
		want      bool
		wantAfter string
	}{
		{"orphan LF after CR receiver", "\n", []byte{'\r'}, true, ""},
		{"LF receiver keeps LF", "\n", []byte{'\n'}, false, "\n"},
		{"CRLF receiver keeps LF", "\n", []byte{'\r', '\n'}, false, "\n"},
		{"CR receiver keeps non orphan buffer", "\nextra", []byte{'\r'}, false, "\nextra"},
		{"CR receiver keeps other byte", "x", []byte{'\r'}, false, "x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			buf.WriteString(tt.buffer)
			if got := discardOrphanLFAfterCR(&buf, tt.recvTerm); got != tt.want {
				t.Fatalf("discardOrphanLFAfterCR() = %v, want %v", got, tt.want)
			}
			if got := buf.String(); got != tt.wantAfter {
				t.Fatalf("buffer after discard = %q, want %q", got, tt.wantAfter)
			}
		})
	}
}

func TestParseFlushTimeout(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
	}{
		{"0", 0},
		{"off", 0},
		{"OFF", 0},
		{"none", 0},
		{"100ms", 100 * time.Millisecond},
		{"1s", time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseFlushTimeout(tt.value)
			if err != nil {
				t.Fatalf("parseFlushTimeout(%q) returned error: %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("parseFlushTimeout(%q) = %s, want %s", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseFlushTimeoutRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"abc", "-1s", "1"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseFlushTimeout(value); err == nil {
				t.Fatalf("parseFlushTimeout(%q) succeeded, want error", value)
			}
		})
	}
}

func TestProcessEscapeSequences(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text", "hello", "hello"},
		{"known escapes", `a\r\n\t\\b`, "a\r\n\t\\b"},
		{"hex lowercase", `A\x41Z`, "AAZ"},
		{"hex uppercase", `A\x7F\xFF`, "A\x7f\xff"},
		{"unknown escape kept", `a\qb`, `a\qb`},
		{"invalid hex kept", `a\xG1b`, `a\xG1b`},
		{"invalid second hex digit kept", `a\x1gb`, `a\x1gb`},
		{"short hex kept", `a\x1`, `a\x1`},
		{"trailing backslash kept", `a\`, `a\`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := processEscapeSequences(tt.input); got != tt.want {
				t.Fatalf("processEscapeSequences(%q) = %q (% x), want %q (% x)", tt.input, got, got, tt.want, tt.want)
			}
		})
	}
}

func TestSendToClient(t *testing.T) {
	colorEnabled = false

	var clients sync.Map
	conn := &recordingConn{}
	clients.Store("client-1", conn)

	out := captureStdout(t, func() {
		sendToClient(&clients, "client-1", `hello\r\nworld`, []byte{'\n'}, 0)
	})

	if got, want := conn.writes.String(), "hello\r\nworld\n"; got != want {
		t.Fatalf("written data = %q (% x), want %q (% x)", got, got, want, want)
	}
	if !strings.Contains(out, "[client-1]") || !strings.Contains(out, "Sent: hello\\r\\nworld") || !strings.Contains(out, "Bytes: 13") {
		t.Fatalf("send output missing expected details: %q", out)
	}
}

func TestSendToClientReportsMissingClient(t *testing.T) {
	var clients sync.Map

	out := captureStdout(t, func() {
		sendToClient(&clients, "missing", "hello", []byte{'\n'}, 0)
	})

	if !strings.Contains(out, "Client not found: missing") {
		t.Fatalf("output = %q, want missing client message", out)
	}
}

func TestSendToClientReportsWriteError(t *testing.T) {
	var clients sync.Map
	clients.Store("client-1", &recordingConn{err: errors.New("write failed")})

	out := captureStdout(t, func() {
		sendToClient(&clients, "client-1", "hello", []byte{'\n'}, 0)
	})

	if !strings.Contains(out, "Send error [client-1]: write failed") {
		t.Fatalf("output = %q, want write error", out)
	}
}

func TestBroadcastToAll(t *testing.T) {
	colorEnabled = false

	var clients sync.Map
	conn1 := &recordingConn{}
	conn2 := &recordingConn{}
	clients.Store("client-1", conn1)
	clients.Store("client-2", conn2)

	out := captureStdout(t, func() {
		broadcastToAll(&clients, `ping\tpong`, []byte{'\r', '\n'}, 0)
	})

	for name, conn := range map[string]*recordingConn{"client-1": conn1, "client-2": conn2} {
		if got, want := conn.writes.String(), "ping\tpong\r\n"; got != want {
			t.Fatalf("%s written data = %q (% x), want %q (% x)", name, got, got, want, want)
		}
	}
	if !strings.Contains(out, "Broadcast completed: sent to 2 clients") {
		t.Fatalf("output = %q, want broadcast count", out)
	}
}

func TestBroadcastToAllCountsOnlySuccessfulWrites(t *testing.T) {
	var clients sync.Map
	clients.Store("ok", &recordingConn{})
	clients.Store("bad", &recordingConn{err: errors.New("write failed")})

	out := captureStdout(t, func() {
		broadcastToAll(&clients, "ping", []byte{'\n'}, 0)
	})

	if !strings.Contains(out, "Send error [bad]: write failed") {
		t.Fatalf("output = %q, want write error", out)
	}
	if !strings.Contains(out, "Broadcast completed: sent to 1 clients") {
		t.Fatalf("output = %q, want successful count", out)
	}
}

func TestListClients(t *testing.T) {
	var clients sync.Map
	clients.Store("client-1", &recordingConn{})
	clients.Store("client-2", &recordingConn{})

	out := captureStdout(t, func() {
		listClients(&clients)
	})

	for _, want := range []string{"Connected clients:", "client-1", "client-2", "Total: 2 clients"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want to contain %q", out, want)
		}
	}
}

func TestListClientsWhenEmpty(t *testing.T) {
	var clients sync.Map

	out := captureStdout(t, func() {
		listClients(&clients)
	})

	if !strings.Contains(out, "No clients connected") {
		t.Fatalf("output = %q, want empty client message", out)
	}
}

func TestShowLogoAndUsageOutput(t *testing.T) {
	out := captureStdout(t, func() {
		showLogo()
		shortUsage()
		fullUsage()
		printServerHelp()
	})

	for _, want := range []string{
		"coe - Communicate and echo through sockets.",
		"Version " + version,
		"Server mode:",
		"Client mode:",
		"OPTIONS",
		"ESCAPE SEQUENCES",
		"Server mode commands:",
		"--tui",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %q", want, out)
		}
	}
}

func TestMainArgumentHandling(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		want     []string
		wantCode int
	}{
		{
			name:     "no args shows short usage",
			args:     []string{"coe"},
			want:     []string{"USAGE", "Use 'coe --help'"},
			wantCode: 0,
		},
		{
			name:     "help shows full usage",
			args:     []string{"coe", "--help"},
			want:     []string{"OPTIONS", "EXAMPLES", "ESCAPE SEQUENCES"},
			wantCode: 0,
		},
		{
			name:     "invalid mode reports error",
			args:     []string{"coe", "--bad"},
			want:     []string{"Error: Mode must be", "USAGE"},
			wantCode: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var code int
			out := captureOutputWithArgs(t, tt.args, func() {
				code = runMain()
			})
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d; output=%q", code, tt.wantCode, out)
			}
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Fatalf("output missing %q: %q", want, out)
				}
			}
		})
	}
}

func TestRunClientArgumentValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing address",
			args: []string{"coe", "-c"},
			want: "Usage: -c, --client",
		},
		{
			name: "invalid positional terminator",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "NL"},
			want: "Error: Terminator must be LF, CR, or CRLF",
		},
		{
			name: "missing buffer size",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "--buffer-size"},
			want: "Error: Buffer size must be specified after --buffer-size",
		},
		{
			name: "non numeric buffer size",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "--buffer-size", "abc"},
			want: "Error: Buffer size must be a number",
		},
		{
			name: "zero buffer size",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "--buffer-size", "0"},
			want: "Error: Buffer size must be 1 or greater",
		},
		{
			name: "buffer size with trailing junk",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "--buffer-size", "10x"},
			want: "Error: Buffer size must be a number",
		},
		{
			name: "missing flush timeout",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "--flush-timeout"},
			want: "Error: Flush timeout must be specified after --flush-timeout",
		},
		{
			name: "invalid flush timeout",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "--flush-timeout", "-1s"},
			want: "Error: flush timeout must be 0 or greater",
		},
		{
			name: "missing tx terminator",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "-tt"},
			want: "Error: Value required after -tt / --tx-term",
		},
		{
			name: "invalid tx terminator",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "-tt", "NL"},
			want: "Error (-tt / --tx-term):",
		},
		{
			name: "missing rx terminator",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "-rt"},
			want: "Error: Value required after -rt / --rx-term",
		},
		{
			name: "invalid rx terminator",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "-rt", "NL"},
			want: "Error (-rt / --rx-term):",
		},
		{
			name: "unknown option",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "--bad"},
			want: "Error: Unknown option --bad",
		},
		{
			name: "tui requires a terminal",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "--tui"},
			want: "Error: --tui requires a terminal",
		},
		{
			name: "tui cannot be used with one-shot message",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "-m", "hi", "--tui"},
			want: "Error: --tui cannot be used with -m",
		},
		{
			name: "missing max frame size",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "--max-frame-size"},
			want: "Error: Max frame size must be specified after --max-frame-size",
		},
		{
			name: "missing write timeout",
			args: []string{"coe", "-c", "127.0.0.1", "1234", "--write-timeout"},
			want: "Error: Write timeout must be specified after --write-timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var code int
			out := captureOutputWithArgs(t, tt.args, func() {
				code = runClientWithExitCode()
			})
			if code != exitUsage {
				t.Fatalf("exit code = %d, want %d; output=%q", code, exitUsage, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Fatalf("output missing %q: %q", tt.want, out)
			}
		})
	}
}

func TestParseWaitDuration(t *testing.T) {
	tests := []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{value: "500ms", want: 500 * time.Millisecond},
		{value: "1s", want: time.Second},
		{value: "0", want: 0},
		{value: "-1s", wantErr: true},
		{value: "later", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseWaitDuration(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseWaitDuration(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("parseWaitDuration(%q) = %s, want %s", tt.value, got, tt.want)
			}
		})
	}
}

func TestRunOneShotClientQuietWaitsForMultipleFrames(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept() error: %v", err)
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		got, err := reader.ReadString('\n')
		if err != nil {
			t.Errorf("ReadString() error: %v", err)
			return
		}
		if got != "STATUS\r\n" {
			t.Errorf("request = %q, want escaped CR followed by LF terminator", got)
			return
		}
		if _, err := conn.Write([]byte("one\ntwo\n")); err != nil {
			t.Errorf("response write error: %v", err)
			return
		}
		// Keep the connection open until the client hangs up, so the client
		// must use the complete wait duration instead of stopping after the
		// first response frame.
		_, _ = io.Copy(io.Discard, conn)
	}()

	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	var code int
	started := time.Now()
	stdout, stderr := captureStdoutAndStderr(t, func() {
		originalArgs := os.Args
		os.Args = []string{"coe", "-c", "127.0.0.1", port, "-m", `STATUS\r`, "--wait", "120ms", "--quiet", "--no-color"}
		defer func() { os.Args = originalArgs }()
		code = runClientWithExitCode()
	})
	elapsed := time.Since(started)
	<-serverDone

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "one\ntwo\n" {
		t.Fatalf("quiet stdout = %q, want one line per response frame", stdout)
	}
	if strings.Contains(stdout, "Connection successful") || strings.Contains(stdout, "Recv]") {
		t.Fatalf("quiet stdout contains log output: %q", stdout)
	}
	if !strings.Contains(stderr, "Connection successful:") || !strings.Contains(stderr, "[Send]") {
		t.Fatalf("quiet stderr missing connection/send logs: %q", stderr)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("client returned after %s, want it to honor the wait duration", elapsed)
	}
}

func TestRunOneShotClientWithoutWaitIsFireAndForget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept() error: %v", err)
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		got, err := reader.ReadString('\n')
		if err != nil {
			t.Errorf("ReadString() error: %v", err)
			return
		}
		if got != "PING\n" {
			t.Errorf("request = %q, want PING LF", got)
		}
	}()

	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	var code int
	stdout, _ := captureStdoutAndStderr(t, func() {
		originalArgs := os.Args
		os.Args = []string{"coe", "-c", "127.0.0.1", port, "-m", "PING", "--quiet", "--no-color"}
		defer func() { os.Args = originalArgs }()
		code = runClientWithExitCode()
	})
	<-serverDone

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout != "" {
		t.Fatalf("fire-and-forget stdout = %q, want no response output", stdout)
	}
}

func TestRunOneShotClientWaitReturnsThreeWithoutResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept() error: %v", err)
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadString('\n'); err != nil {
			t.Errorf("ReadString() error: %v", err)
			return
		}
		// Never answer; hold the connection until the client gives up.
		_, _ = io.Copy(io.Discard, reader)
	}()

	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	var code int
	stdout, stderr := captureStdoutAndStderr(t, func() {
		originalArgs := os.Args
		os.Args = []string{"coe", "-c", "127.0.0.1", port, "-m", "PING", "--wait", "30ms", "--quiet", "--no-color"}
		defer func() { os.Args = originalArgs }()
		code = runClientWithExitCode()
	})
	<-serverDone

	if code != 3 {
		t.Fatalf("exit code = %d, want 3; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("no-response stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "No response received") {
		t.Fatalf("stderr = %q, want no-response message", stderr)
	}
}

func TestRunServerArgumentValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing port",
			args: []string{"coe", "-s"},
			want: "Usage: -s, --server",
		},
		{
			name: "missing buffer size",
			args: []string{"coe", "-s", "1234", "--buffer-size"},
			want: "Error: Buffer size must be specified after --buffer-size",
		},
		{
			name: "non numeric buffer size",
			args: []string{"coe", "-s", "1234", "--buffer-size", "abc"},
			want: "Error: Buffer size must be a number",
		},
		{
			name: "zero buffer size",
			args: []string{"coe", "-s", "1234", "--buffer-size", "0"},
			want: "Error: Buffer size must be 1 or greater",
		},
		{
			name: "buffer size with trailing junk",
			args: []string{"coe", "-s", "1234", "--buffer-size", "10x"},
			want: "Error: Buffer size must be a number",
		},
		{
			name: "missing flush timeout",
			args: []string{"coe", "-s", "1234", "--flush-timeout"},
			want: "Error: Flush timeout must be specified after --flush-timeout",
		},
		{
			name: "invalid flush timeout",
			args: []string{"coe", "-s", "1234", "--flush-timeout", "-1s"},
			want: "Error: flush timeout must be 0 or greater",
		},
		{
			name: "missing tx terminator",
			args: []string{"coe", "-s", "1234", "-tt"},
			want: "Error: Value required after -tt / --tx-term",
		},
		{
			name: "invalid tx terminator",
			args: []string{"coe", "-s", "1234", "-tt", "NL"},
			want: "Error (-tt / --tx-term):",
		},
		{
			name: "missing rx terminator",
			args: []string{"coe", "-s", "1234", "-rt"},
			want: "Error: Value required after -rt / --rx-term",
		},
		{
			name: "invalid rx terminator",
			args: []string{"coe", "-s", "1234", "-rt", "NL"},
			want: "Error (-rt / --rx-term):",
		},
		{
			name: "unknown option",
			args: []string{"coe", "-s", "1234", "--bad"},
			want: "Error: Unknown option or terminator --bad",
		},
		{
			name: "tui requires a terminal",
			args: []string{"coe", "-s", "1234", "--tui"},
			want: "Error: --tui requires a terminal",
		},
		{
			name: "missing max frame size",
			args: []string{"coe", "-s", "1234", "--max-frame-size"},
			want: "Error: Max frame size must be specified after --max-frame-size",
		},
		{
			name: "negative max frame size",
			args: []string{"coe", "-s", "1234", "--max-frame-size", "-1"},
			want: "Error: Max frame size must be 0 or greater",
		},
		{
			name: "missing max clients",
			args: []string{"coe", "-s", "1234", "--max-clients"},
			want: "Error: Max clients must be specified after --max-clients",
		},
		{
			name: "missing write timeout",
			args: []string{"coe", "-s", "1234", "--write-timeout"},
			want: "Error: Write timeout must be specified after --write-timeout",
		},
		{
			name: "invalid write timeout",
			args: []string{"coe", "-s", "1234", "--write-timeout", "-1s"},
			want: "Error: write timeout must be 0 or greater",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var code int
			out := captureOutputWithArgs(t, tt.args, func() {
				code = runServer()
			})
			if code != exitUsage {
				t.Fatalf("exit code = %d, want %d; output=%q", code, exitUsage, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Fatalf("output missing %q: %q", tt.want, out)
			}
		})
	}
}

func TestRunServerAcceptsClientCommandsAndQuits(t *testing.T) {
	port := freeTCPPort(t)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	defer stdinR.Close()

	driverDone := make(chan struct{})
	go func() {
		defer close(driverDone)
		defer stdinW.Close()

		conn := waitForTCP(t, "127.0.0.1:"+port)
		defer conn.Close()

		// Wait for the echo reply before issuing #send: handleClient only runs
		// after the accept loop has stored the client, so the echo proves the
		// server has registered this connection.
		if _, err := conn.Write([]byte("ping\n")); err != nil {
			t.Errorf("write ping error: %v", err)
			return
		}
		readUntilContains(t, conn, "ping\n")

		if _, err := fmt.Fprintf(stdinW, "#list\n"); err != nil {
			t.Errorf("write #list error: %v", err)
			return
		}
		if _, err := fmt.Fprintf(stdinW, "#send %s server\\tmessage\n", conn.LocalAddr().String()); err != nil {
			t.Errorf("write #send error: %v", err)
			return
		}
		if got := readUntilContains(t, conn, "server\tmessage\n"); !strings.Contains(got, "server\tmessage\n") {
			t.Errorf("send response = %q, want server message", got)
			return
		}
		if _, err := fmt.Fprintf(stdinW, "#broadcast broadcast\\nmessage\n"); err != nil {
			t.Errorf("write #broadcast error: %v", err)
			return
		}
		if got := readUntilContains(t, conn, "broadcast\nmessage\n"); !strings.Contains(got, "broadcast\nmessage\n") {
			t.Errorf("broadcast response = %q, want broadcast message", got)
			return
		}
		if _, err := fmt.Fprintf(stdinW, "#help\n#help program\n#unknown\n#quit\n"); err != nil {
			t.Errorf("write shutdown commands error: %v", err)
			return
		}
	}()

	out := captureStdoutWithArgsAndStdin(t, []string{"coe", "-s", port, "LF", "--no-color"}, stdinR, func() {
		_ = runServer()
	})
	<-driverDone

	for _, want := range []string{
		"Server started on port: " + port,
		"Client connected:",
		"Connected clients:",
		"Sent: server\\tmessage",
		"Broadcast completed: sent to 1 clients",
		"Server mode commands:",
		"Unknown command: #unknown",
		"Shutting down server...",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("server output missing %q: %q", want, out)
		}
	}
}

func TestRunClientConnectsSendsReceivesAndExitsOnStdinEOF(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)

		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept() error: %v", err)
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		first, err := reader.ReadString('\n')
		if err != nil {
			t.Errorf("ReadString(first) error: %v", err)
			return
		}
		if first != "hello\n" {
			t.Errorf("first frame = %q, want hello LF", first)
			return
		}
		empty, err := reader.ReadString('\n')
		if err != nil {
			t.Errorf("ReadString(empty) error: %v", err)
			return
		}
		if empty != "\n" {
			t.Errorf("empty line was sent as %q, want a bare LF", empty)
			return
		}
		if _, err := conn.Write([]byte("echo-one\n")); err != nil {
			t.Errorf("write echo-one error: %v", err)
			return
		}
		second, err := reader.ReadString('\n')
		if err != nil {
			t.Errorf("ReadString(second) error: %v", err)
			return
		}
		if second != "line\t2\n" {
			t.Errorf("second frame = %q, want escaped tab frame", second)
			return
		}
		if _, err := conn.Write([]byte("echo-two\n")); err != nil {
			t.Errorf("write echo-two error: %v", err)
			return
		}
	}()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		if _, err := stdinW.WriteString("hello\n\nline\\t2\n"); err != nil {
			t.Errorf("stdin write error: %v", err)
			return
		}
		// End the input right away: the client half-closes and still reads
		// echo-two, which the server sends after the last line.
		if err := stdinW.Close(); err != nil {
			t.Errorf("stdin close error: %v", err)
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	out := captureStdoutWithArgsAndStdin(
		t,
		[]string{"coe", "-c", addr.IP.String(), strconv.Itoa(addr.Port), "LF", "--buffer-size", "3", "--no-color"},
		stdinR,
		func() { runClientWithExitCode() },
	)
	<-serverDone
	<-stdinDone

	for _, want := range []string{
		"Connection successful:",
		"Buffer size: 3 bytes",
		"Send] ",
		"hello (Bytes: 6",
		" (Bytes: 1, HEX: 0a)",
		"line\\t2 (Bytes: 7",
		"echo-one",
		"echo-two",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("client output missing %q: %q", want, out)
		}
	}
}

func readExactly(t *testing.T, conn net.Conn, size int) string {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error: %v", err)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("ReadFull() error: %v", err)
	}
	return string(buf)
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleClient did not stop")
	}
}

func TestHandleClientEchoesCompleteFrames(t *testing.T) {
	colorEnabled = false

	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleClient(server, serverConfig{recvTerminatorBytes: []byte{'\n'}, sendTerminatorBytes: []byte{'\r', '\n'}, echoEnabled: true, bufferSize: 2})
	}()

	if _, err := client.Write([]byte("hello\n")); err != nil {
		t.Fatalf("client write error: %v", err)
	}
	if got, want := readExactly(t, client, len("hello\r\n")), "hello\r\n"; got != want {
		t.Fatalf("echo = %q (% x), want %q (% x)", got, got, want, want)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("client close error: %v", err)
	}
	waitDone(t, done)
}

func TestHandleClientDoesNotEchoWhenDisabled(t *testing.T) {
	colorEnabled = false

	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleClient(server, serverConfig{recvTerminatorBytes: []byte{'\n'}, sendTerminatorBytes: []byte{'\n'}, echoEnabled: false, bufferSize: 8})
	}()

	if _, err := client.Write([]byte("hello\n")); err != nil {
		t.Fatalf("client write error: %v", err)
	}
	if err := client.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline() error: %v", err)
	}
	buf := make([]byte, 1)
	_, err := client.Read(buf)
	if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("client read error = %v, want timeout because echo is disabled", err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("client close error: %v", err)
	}
	waitDone(t, done)
}

// dataThenEOFConn returns its data together with io.EOF in a single Read call,
// as real TCP connections may do when the peer sends and immediately closes.
type dataThenEOFConn struct {
	recordingConn
	data []byte
	read bool
}

func (c *dataThenEOFConn) Read(p []byte) (int, error) {
	if c.read {
		return 0, io.EOF
	}
	c.read = true
	return copy(p, c.data), io.EOF
}

func TestHandleClientProcessesDataDeliveredWithEOF(t *testing.T) {
	colorEnabled = false

	conn := &dataThenEOFConn{data: []byte("hello\nworld")}

	out := captureStdout(t, func() {
		handleClient(conn, serverConfig{recvTerminatorBytes: []byte{'\n'}, sendTerminatorBytes: []byte{'\n'}, echoEnabled: true, bufferSize: 32})
	})

	if got, want := conn.writes.String(), "hello\n"; got != want {
		t.Fatalf("echo = %q (% x), want %q (% x)", got, got, want, want)
	}
	for _, want := range []string{"Received: hello", "Received: world"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %q", want, out)
		}
	}
}

func TestHandleClientReceivesCRAndDropsFollowingLF(t *testing.T) {
	colorEnabled = false

	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleClient(server, serverConfig{recvTerminatorBytes: []byte{'\r'}, sendTerminatorBytes: []byte{'\n'}, echoEnabled: true, bufferSize: 32})
	}()

	if _, err := client.Write([]byte("hello\r\nworld\r")); err != nil {
		t.Fatalf("client write error: %v", err)
	}
	if got, want := readExactly(t, client, len("hello\nworld\n")), "hello\nworld\n"; got != want {
		t.Fatalf("echo = %q (% x), want %q (% x)", got, got, want, want)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("client close error: %v", err)
	}
	waitDone(t, done)
}

func TestSplitHead(t *testing.T) {
	tests := []struct {
		in       string
		wantHead string
		wantRest string
	}{
		{"#send addr A  B", "#send", "addr A  B"},
		{"  #broadcast  hello", "#broadcast", " hello"},
		{"#send\t127.0.0.1:1234\thello", "#send", "127.0.0.1:1234\thello"},
		{"#broadcast\thello", "#broadcast", "hello"},
		{"#list", "#list", ""},
		{"   ", "", ""},
		{"#send addr", "#send", "addr"},
	}
	for _, tt := range tests {
		head, rest := splitHead(tt.in)
		if head != tt.wantHead || rest != tt.wantRest {
			t.Fatalf("splitHead(%q) = %q, %q; want %q, %q", tt.in, head, rest, tt.wantHead, tt.wantRest)
		}
	}

	addr, message := splitHead("addr A  B")
	if addr != "addr" || message != "A  B" {
		t.Fatalf("send payload split = %q, %q; want addr, %q", addr, message, "A  B")
	}
	addr, message = splitHead("127.0.0.1:1234\thello")
	if addr != "127.0.0.1:1234" || message != "hello" {
		t.Fatalf("tab send payload split = %q, %q; want addr and hello", addr, message)
	}
}

func TestParseClientConfigIPv6AndIPv4(t *testing.T) {
	original := os.Args
	defer func() { os.Args = original }()

	os.Args = []string{"coe", "-c", "::1", "8080"}
	cfg, err := parseClientConfig()
	if err != nil {
		t.Fatalf("IPv6 parseClientConfig() error: %v", err)
	}
	if cfg.address != "[::1]:8080" {
		t.Fatalf("IPv6 address = %q, want [::1]:8080", cfg.address)
	}

	os.Args = []string{"coe", "-c", "127.0.0.1", "8080"}
	cfg, err = parseClientConfig()
	if err != nil {
		t.Fatalf("IPv4 parseClientConfig() error: %v", err)
	}
	if cfg.address != "127.0.0.1:8080" {
		t.Fatalf("IPv4 address = %q, want 127.0.0.1:8080", cfg.address)
	}
}

func TestParseMaxFrameSizeAndMaxClients(t *testing.T) {
	got, err := parseMaxFrameSize("0")
	if err != nil || got != 0 {
		t.Fatalf("parseMaxFrameSize(0) = %d, %v", got, err)
	}
	if _, err := parseMaxFrameSize("-1"); err == nil {
		t.Fatal("parseMaxFrameSize(-1) succeeded")
	}
	got, err = parseMaxClients("64")
	if err != nil || got != 64 {
		t.Fatalf("parseMaxClients(64) = %d, %v", got, err)
	}
	d, err := parseWriteTimeout("5s")
	if err != nil || d != 5*time.Second {
		t.Fatalf("parseWriteTimeout(5s) = %s, %v", d, err)
	}
	d, err = parseWriteTimeout("off")
	if err != nil || d != 0 {
		t.Fatalf("parseWriteTimeout(off) = %s, %v", d, err)
	}
}

func TestAppendAndFlushBySuffixMaxFrameSize(t *testing.T) {
	var buf bytes.Buffer
	var pendingSkipLF bool
	var messages []string
	err := appendAndFlushBySuffix(&buf, []byte("hello\n"), []byte{'\n'}, &pendingSkipLF, 4, func(msg string) {
		messages = append(messages, msg)
	})
	if !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("error = %v, want errFrameTooLarge", err)
	}
	if len(messages) != 0 {
		t.Fatalf("messages = %q, want none", messages)
	}

	buf.Reset()
	pendingSkipLF = false
	err = appendAndFlushBySuffix(&buf, []byte("ok\n"), []byte{'\n'}, &pendingSkipLF, 4, func(msg string) {
		messages = append(messages, msg)
	})
	if err != nil {
		t.Fatalf("appendAndFlushBySuffix() error: %v", err)
	}
	if len(messages) != 1 || messages[0] != "ok" {
		t.Fatalf("messages = %q, want [ok]", messages)
	}
}

func TestWriteConnTimesOut(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	done := make(chan error, 1)
	go func() {
		done <- writeConn(server, []byte("hello"), 30*time.Millisecond)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writeConn succeeded, want timeout")
		}
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("writeConn error = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("writeConn did not return")
	}
}

func TestHandleClientDropsOversizedFrame(t *testing.T) {
	colorEnabled = false

	server, client := net.Pipe()
	done := make(chan struct{})
	var out string
	go func() {
		defer close(done)
		out = captureStdout(t, func() {
			handleClient(server, serverConfig{recvTerminatorBytes: []byte{'\n'}, sendTerminatorBytes: []byte{'\n'}, echoEnabled: false, bufferSize: 32, maxFrameSize: 4})
		})
	}()

	if _, err := client.Write([]byte("hello\n")); err != nil {
		t.Fatalf("client write error: %v", err)
	}
	waitDone(t, done)
	_ = client.Close()

	if !strings.Contains(out, "frame exceeds max size") {
		t.Fatalf("output = %q, want max-frame error", out)
	}
}

func TestBroadcastWriteTimeoutDropsClient(t *testing.T) {
	colorEnabled = false

	blocked, peer := net.Pipe()
	defer peer.Close()

	okConn := &recordingConn{}
	var clients sync.Map
	clients.Store("blocked", blocked)
	clients.Store("ok", okConn)

	out := captureStdout(t, func() {
		broadcastToAll(&clients, "hello", []byte{'\n'}, 30*time.Millisecond)
	})
	_ = blocked.Close()

	if _, stillThere := clients.Load("blocked"); stillThere {
		t.Fatal("blocked client was not dropped after write timeout")
	}
	if got, want := okConn.writes.String(), "hello\n"; got != want {
		t.Fatalf("successful client write = %q, want %q", got, want)
	}
	if !strings.Contains(out, "Broadcast completed: sent to 1 clients") {
		t.Fatalf("output = %q, want successful count of 1", out)
	}
}

func TestSendToClientWriteTimeoutDropsClient(t *testing.T) {
	colorEnabled = false

	blocked, peer := net.Pipe()
	defer peer.Close()

	var clients sync.Map
	clients.Store("blocked", blocked)

	out := captureStdout(t, func() {
		sendToClient(&clients, "blocked", "hello", []byte{'\n'}, 30*time.Millisecond)
	})
	_ = blocked.Close()

	if _, stillThere := clients.Load("blocked"); stillThere {
		t.Fatal("blocked client was not dropped after write timeout")
	}
	if !strings.Contains(out, "Send error [blocked]:") {
		t.Fatalf("output = %q, want send error", out)
	}
}

func TestInteractiveClientDialFailureExitCode(t *testing.T) {
	port := freeTCPPort(t)
	var code int
	out := captureOutputWithArgs(t, []string{"coe", "-c", "127.0.0.1", port, "--no-color"}, func() {
		code = runClientWithExitCode()
	})
	if code != exitIO {
		t.Fatalf("exit code = %d, want %d; output=%q", code, exitIO, out)
	}
	if !strings.Contains(out, "Connection error:") {
		t.Fatalf("output = %q, want connection error", out)
	}
}

func TestRunServerBindFailureExitCode(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer busy.Close()

	var code int
	out := captureOutputWithArgs(t, []string{"coe", "-s", busy.Addr().String(), "--no-color"}, func() {
		code = runServer()
	})
	if code != exitIO {
		t.Fatalf("exit code = %d, want %d; output=%q", code, exitIO, out)
	}
	if !strings.Contains(out, "Server startup error:") {
		t.Fatalf("output = %q, want bind error", out)
	}
}

func TestRunServerRejectsExtraClients(t *testing.T) {
	port := freeTCPPort(t)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	defer stdinR.Close()

	driverDone := make(chan struct{})
	go func() {
		defer close(driverDone)
		defer stdinW.Close()

		first := waitForTCP(t, "127.0.0.1:"+port)
		defer first.Close()

		second, err := net.Dial("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Errorf("second Dial() error: %v", err)
			return
		}
		defer second.Close()
		if err := second.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Errorf("SetReadDeadline() error: %v", err)
			return
		}
		buf := make([]byte, 1)
		_, err = second.Read(buf)
		if err == nil {
			t.Error("second client stayed connected, want rejection")
			return
		}

		if _, err := fmt.Fprintf(stdinW, "#quit\n"); err != nil {
			t.Errorf("write #quit error: %v", err)
		}
	}()

	out := captureStdoutWithArgsAndStdin(t, []string{"coe", "-s", port, "--no-color", "--max-clients", "1"}, stdinR, func() {
		_ = runServer()
	})
	<-driverDone

	if !strings.Contains(out, "max clients (1) reached") {
		t.Fatalf("server output missing rejection: %q", out)
	}
}

func TestRunServerSendPreservesRepeatedSpaces(t *testing.T) {
	port := freeTCPPort(t)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	defer stdinR.Close()

	driverDone := make(chan struct{})
	go func() {
		defer close(driverDone)
		defer stdinW.Close()

		conn := waitForTCP(t, "127.0.0.1:"+port)
		defer conn.Close()

		if _, err := conn.Write([]byte("ping\n")); err != nil {
			t.Errorf("write ping error: %v", err)
			return
		}
		readUntilContains(t, conn, "ping\n")

		if _, err := fmt.Fprintf(stdinW, "#send %s A  B\n", conn.LocalAddr().String()); err != nil {
			t.Errorf("write #send error: %v", err)
			return
		}
		if got := readUntilContains(t, conn, "A  B\n"); !strings.Contains(got, "A  B\n") {
			t.Errorf("send response = %q, want preserved spaces", got)
			return
		}
		if _, err := fmt.Fprintf(stdinW, "#broadcast C  D\n"); err != nil {
			t.Errorf("write #broadcast error: %v", err)
			return
		}
		if got := readUntilContains(t, conn, "C  D\n"); !strings.Contains(got, "C  D\n") {
			t.Errorf("broadcast response = %q, want preserved spaces", got)
			return
		}
		if _, err := fmt.Fprintf(stdinW, "#quit\n"); err != nil {
			t.Errorf("write #quit error: %v", err)
		}
	}()

	out := captureStdoutWithArgsAndStdin(t, []string{"coe", "-s", port, "LF", "--no-color"}, stdinR, func() {
		_ = runServer()
	})
	<-driverDone

	if !strings.Contains(out, "Sent: A  B") {
		t.Fatalf("server output missing spaced send: %q", out)
	}
}

func TestInteractiveClientExitsOnPeerDisconnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept() error: %v", err)
			return
		}
		_ = conn.Close()
	}()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()

	addr := listener.Addr().(*net.TCPAddr)
	done := make(chan struct{})
	var code int
	go func() {
		defer close(done)
		captureStdoutWithArgsAndStdin(
			t,
			[]string{"coe", "-c", addr.IP.String(), strconv.Itoa(addr.Port), "--no-color"},
			stdinR,
			func() { code = runClientWithExitCode() },
		)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("interactive client did not exit after peer disconnect")
	}
	<-serverDone
	if code != exitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestInteractiveClientOversizedFrameExitCode(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept() error: %v", err)
			return
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("hello\n")); err != nil {
			t.Errorf("write oversized frame error: %v", err)
			return
		}
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
	}()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	defer stdinR.Close()
	defer stdinW.Close()

	addr := listener.Addr().(*net.TCPAddr)
	done := make(chan struct{})
	var code int
	go func() {
		defer close(done)
		captureStdoutWithArgsAndStdin(
			t,
			[]string{"coe", "-c", addr.IP.String(), strconv.Itoa(addr.Port), "--no-color", "--max-frame-size", "4"},
			stdinR,
			func() { code = runClientWithExitCode() },
		)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("interactive client did not exit after oversized frame")
	}
	<-serverDone
	if code != exitIO {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestRunOneShotClientReceiveResetReturnsOne(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept() error: %v", err)
			return
		}
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadString('\n'); err != nil {
			t.Errorf("ReadString() error: %v", err)
			_ = conn.Close()
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}()

	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	var code int
	stdout, stderr := captureStdoutAndStderr(t, func() {
		originalArgs := os.Args
		os.Args = []string{"coe", "-c", "127.0.0.1", port, "-m", "PING", "--wait", "1s", "--quiet", "--no-color"}
		defer func() { os.Args = originalArgs }()
		code = runClientWithExitCode()
	})
	<-serverDone

	if code != exitIO {
		t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stderr, "No response received") {
		t.Fatalf("stderr = %q, want I/O error rather than wait timeout", stderr)
	}
}

func TestRunOneShotClientResetAfterFrameReturnsOne(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept() error: %v", err)
			return
		}
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadString('\n'); err != nil {
			t.Errorf("ReadString() error: %v", err)
			_ = conn.Close()
			return
		}
		if _, err := conn.Write([]byte("one\n")); err != nil {
			t.Errorf("response write error: %v", err)
			_ = conn.Close()
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}()

	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	var code int
	stdout, stderr := captureStdoutAndStderr(t, func() {
		originalArgs := os.Args
		os.Args = []string{"coe", "-c", "127.0.0.1", port, "-m", "PING", "--wait", "1s", "--quiet", "--no-color"}
		defer func() { os.Args = originalArgs }()
		code = runClientWithExitCode()
	})
	<-serverDone

	if code != exitIO {
		t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "one\n" {
		t.Fatalf("quiet stdout = %q, want the frame received before reset", stdout)
	}
	if strings.Contains(stderr, "No response received") {
		t.Fatalf("stderr = %q, want I/O error rather than wait timeout", stderr)
	}
}

func TestRunOneShotClientEOFAfterFrameReturnsZero(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("Accept() error: %v", err)
			return
		}
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadString('\n'); err != nil {
			t.Errorf("ReadString() error: %v", err)
			_ = conn.Close()
			return
		}
		if _, err := conn.Write([]byte("one\n")); err != nil {
			t.Errorf("response write error: %v", err)
		}
		_ = conn.Close()
	}()

	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	var code int
	stdout, stderr := captureStdoutAndStderr(t, func() {
		originalArgs := os.Args
		os.Args = []string{"coe", "-c", "127.0.0.1", port, "-m", "PING", "--wait", "1s", "--quiet", "--no-color"}
		defer func() { os.Args = originalArgs }()
		code = runClientWithExitCode()
	})
	<-serverDone

	if code != exitOK {
		t.Fatalf("exit code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "one\n" {
		t.Fatalf("quiet stdout = %q, want the frame received before EOF", stdout)
	}
}

func TestRunServerSendAcceptsTabSeparators(t *testing.T) {
	port := freeTCPPort(t)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	defer stdinR.Close()

	driverDone := make(chan struct{})
	go func() {
		defer close(driverDone)
		defer stdinW.Close()

		conn := waitForTCP(t, "127.0.0.1:"+port)
		defer conn.Close()

		if _, err := conn.Write([]byte("ping\n")); err != nil {
			t.Errorf("write ping error: %v", err)
			return
		}
		readUntilContains(t, conn, "ping\n")

		if _, err := fmt.Fprintf(stdinW, "#send\t%s\thello\n", conn.LocalAddr().String()); err != nil {
			t.Errorf("write tab #send error: %v", err)
			return
		}
		if got := readUntilContains(t, conn, "hello\n"); !strings.Contains(got, "hello\n") {
			t.Errorf("send response = %q, want hello", got)
			return
		}
		if _, err := fmt.Fprintf(stdinW, "#broadcast\tworld\n"); err != nil {
			t.Errorf("write tab #broadcast error: %v", err)
			return
		}
		if got := readUntilContains(t, conn, "world\n"); !strings.Contains(got, "world\n") {
			t.Errorf("broadcast response = %q, want world", got)
			return
		}
		if _, err := fmt.Fprintf(stdinW, "#quit\n"); err != nil {
			t.Errorf("write #quit error: %v", err)
		}
	}()

	out := captureStdoutWithArgsAndStdin(t, []string{"coe", "-s", port, "LF", "--no-color"}, stdinR, func() {
		_ = runServer()
	})
	<-driverDone

	if !strings.Contains(out, "Sent: hello") {
		t.Fatalf("server output missing tab send: %q", out)
	}
}

func TestDisplayTextEscapesControls(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"A\r\nB\x1bC", `A\r\nB\x1BC`},
		{"tab\there", `tab\there`},
		{"\x1b]0;title\x07", `\x1B]0;title\x07`},
		{"c1\u009bx", `c1\xC2\x9Bx`},
		{"bad\xffutf8", `bad\xFFutf8`},
		{"日本語", "日本語"},
		{`back\slash`, `back\slash`},
	}
	for _, tt := range tests {
		if got := displayText(tt.in); got != tt.want {
			t.Errorf("displayText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLogLinesDoNotCarryRawControlBytes(t *testing.T) {
	colorEnabled = false
	out := captureStdout(t, func() {
		logClientData("Recv", colorGreen, "hello\r", []byte("hello\r\n"))
		logServerData("1.2.3.4:5", "Received", colorGreen, "\x1b[2Jx", []byte("\x1b[2Jx\n"))
	})
	if strings.ContainsAny(out, "\r\x1b") {
		t.Fatalf("log output contains raw control bytes: %q", out)
	}
	if !strings.Contains(out, `hello\r (Bytes: 7`) || !strings.Contains(out, `\x1B[2Jx (Bytes: 6`) {
		t.Fatalf("escaped payload missing: %q", out)
	}
}

func TestParseClientTarget(t *testing.T) {
	tests := []struct {
		args     []string
		want     string
		wantUsed int
		wantErr  bool
	}{
		{args: []string{"127.0.0.1", "8080"}, want: "127.0.0.1:8080", wantUsed: 2},
		{args: []string{"127.0.0.1:8080"}, want: "127.0.0.1:8080", wantUsed: 1},
		{args: []string{"127.0.0.1:8080", "LF"}, want: "127.0.0.1:8080", wantUsed: 1},
		{args: []string{"localhost:8080", "--tui"}, want: "localhost:8080", wantUsed: 1},
		{args: []string{"::1", "8080"}, want: "[::1]:8080", wantUsed: 2},
		{args: []string{"[::1]:8080"}, want: "[::1]:8080", wantUsed: 1},
		{args: []string{"host", "abc"}, wantErr: true},
		{args: []string{"host", "0"}, wantErr: true},
		{args: []string{"host", "65536"}, wantErr: true},
		{args: []string{"host:99999"}, wantErr: true},
		{args: []string{"host"}, wantErr: true},
		{args: nil, wantErr: true},
	}
	for _, tt := range tests {
		got, used, err := parseClientTarget(tt.args)
		if (err != nil) != tt.wantErr {
			t.Fatalf("parseClientTarget(%q) error = %v, wantErr %v", tt.args, err, tt.wantErr)
		}
		if !tt.wantErr && (got != tt.want || used != tt.wantUsed) {
			t.Fatalf("parseClientTarget(%q) = %q, %d; want %q, %d", tt.args, got, used, tt.want, tt.wantUsed)
		}
	}
}

func TestParseServerAddress(t *testing.T) {
	tests := []struct {
		arg, host, port string
		wantErr         bool
	}{
		{arg: "8080", port: "8080"},
		{arg: "0", port: "0"},
		{arg: "127.0.0.1:9000", host: "127.0.0.1", port: "9000"},
		{arg: "[::1]:9000", host: "::1", port: "9000"},
		{arg: ":9000", port: "9000"},
		{arg: "abc", wantErr: true},
		{arg: "70000", wantErr: true},
		{arg: "--tui", wantErr: true},
		{arg: "-1", wantErr: true},
		{arg: "::1", wantErr: true},
	}
	for _, tt := range tests {
		host, port, err := parseServerAddress(tt.arg)
		if (err != nil) != tt.wantErr {
			t.Fatalf("parseServerAddress(%q) error = %v, wantErr %v", tt.arg, err, tt.wantErr)
		}
		if !tt.wantErr && (host != tt.host || port != tt.port) {
			t.Fatalf("parseServerAddress(%q) = %q, %q; want %q, %q", tt.arg, host, port, tt.host, tt.port)
		}
	}
}

func TestUsageErrorsForBadPortsAndOptionCombinations(t *testing.T) {
	tests := []struct {
		name string
		args []string
		run  func() int
		want string
	}{
		{"server port is not a number", []string{"coe", "-s", "abc"}, runServer, "port must be a number"},
		{"server port is an option", []string{"coe", "-s", "--tui"}, runServer, "port must be a number"},
		{"server port out of range", []string{"coe", "-s", "70000"}, runServer, "port must be a number"},
		{"bind with host:port", []string{"coe", "-s", "127.0.0.1:0", "--bind", "::1"}, runServer, "--bind cannot be combined"},
		{"client port is not a number", []string{"coe", "-c", "127.0.0.1", "abc"}, runClientWithExitCode, "port must be a number"},
		{"wait with tui", []string{"coe", "-c", "127.0.0.1", "1234", "--wait", "1s", "--tui"}, runClientWithExitCode, "--wait is only used with -m"},
		{"quiet without message", []string{"coe", "-c", "127.0.0.1", "1234", "--quiet"}, runClientWithExitCode, "--quiet is only used with -m"},
		{"flush timeout below 1ms", []string{"coe", "-c", "127.0.0.1", "1234", "--flush-timeout", "1ns"}, runClientWithExitCode, "at least 1ms"},
		{"server flush timeout below 1ms", []string{"coe", "-s", "0", "--flush-timeout", "10us"}, runServer, "at least 1ms"},
		{"negative connect timeout", []string{"coe", "-c", "127.0.0.1", "1234", "--connect-timeout", "-1s"}, runClientWithExitCode, "connect timeout must be 0 or greater"},
		{"missing log file", []string{"coe", "-c", "127.0.0.1", "1234", "--log"}, runClientWithExitCode, "Log file must be specified after --log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var code int
			stdout, stderr := captureStdoutAndStderr(t, func() {
				originalArgs := os.Args
				os.Args = tt.args
				defer func() { os.Args = originalArgs }()
				code = tt.run()
			})
			if code != exitUsage {
				t.Fatalf("exit code = %d, want %d; stdout=%q stderr=%q", code, exitUsage, stdout, stderr)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr, tt.want)
			}
			if stdout != "" {
				t.Fatalf("usage error leaked to stdout: %q", stdout)
			}
		})
	}
}

func TestVersionFlag(t *testing.T) {
	for _, arg := range []string{"--version", "-v", "version"} {
		var code int
		out := captureStdoutWithArgs(t, []string{"coe", arg}, func() { code = runMain() })
		if code != exitOK || out != "coe "+version+"\n" {
			t.Fatalf("%s: code=%d output=%q", arg, code, out)
		}
	}
}

func TestResolveColor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	defer r.Close()
	defer w.Close()

	t.Setenv("NO_COLOR", "")
	if resolveColor(colorAuto, w) {
		t.Fatalf("auto color is on for a pipe")
	}
	if !resolveColor(colorOn, w) {
		t.Fatalf("--color did not force color")
	}
	t.Setenv("NO_COLOR", "1")
	if resolveColor(colorAuto, os.Stdout) {
		t.Fatalf("NO_COLOR was ignored")
	}
	if !resolveColor(colorOn, w) {
		t.Fatalf("--color should win over NO_COLOR")
	}
	if resolveColor(colorOff, w) {
		t.Fatalf("--no-color left color on")
	}
}

func TestLineOutputRedrawsVisiblePrompt(t *testing.T) {
	saved := terminalControlEnabled
	terminalControlEnabled = true
	defer func() {
		terminalControlEnabled = saved
		promptConsumed()
	}()

	out := captureStdout(t, func() {
		setPrompt("Send> ")
		showPrompt()
		showPrompt() // already showing: not drawn twice
		lineOutput(func() { fmt.Println("event") })
		promptConsumed() // Enter pressed
		lineOutput(func() { fmt.Println("reply") })
		showPrompt()
		hidePrompt()
	})
	// After Enter, the prompt redrawn by "event" is erased before "reply".
	want := "Send> " + "\r\033[K" + "event\n" + "Send> " + "\r\033[K" + "reply\n" + "Send> " + "\r\033[K"
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

func TestRunServerKeepsRunningWithoutUsableStdin(t *testing.T) {
	interrupt, err := os.FindProcess(os.Getpid())
	if err != nil || interrupt.Signal(syscall.Signal(0)) != nil {
		t.Skip("cannot signal this process")
	}
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt cannot be sent to a process on Windows")
	}

	closedPipe := func(t *testing.T) *os.File {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe() error: %v", err)
		}
		w.Close() // like </dev/null or a container without -i
		return r
	}
	writeOnly := func(t *testing.T) *os.File {
		// GNU nohup reopens a terminal stdin like this, so reads fail.
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open %s: %v", os.DevNull, err)
		}
		return f
	}
	for _, tt := range []struct {
		name  string
		stdin func(*testing.T) *os.File
		want  string
	}{
		{"stdin at EOF", closedPipe, "Standard input closed"},
		{"stdin unreadable", writeOnly, "Standard input unreadable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stdin := tt.stdin(t)
			defer stdin.Close()
			port := freeTCPPort(t)

			done := make(chan struct{})
			var code int
			var out string
			go func() {
				defer close(done)
				out = captureStdoutWithArgsAndStdin(t, []string{"coe", "-s", port, "--no-color"}, stdin, func() {
					code = runServer()
				})
			}()

			conn := waitForTCP(t, "127.0.0.1:"+port)
			defer conn.Close()
			for _, msg := range []string{"one\n", "two\n"} {
				if _, err := conn.Write([]byte(msg)); err != nil {
					t.Fatalf("write error: %v", err)
				}
				readUntilContains(t, conn, msg)
				time.Sleep(100 * time.Millisecond) // give the end of stdin time to be handled
			}
			select {
			case <-done:
				t.Fatalf("server exited without usable stdin: %q", out)
			default:
			}

			if err := interrupt.Signal(os.Interrupt); err != nil {
				t.Fatalf("Signal() error: %v", err)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("server did not stop on SIGINT")
			}
			if code != exitOK {
				t.Fatalf("exit code = %d, want 0", code)
			}
			for _, want := range []string{tt.want, "Shutting down server...", "Client disconnected"} {
				if !strings.Contains(out, want) {
					t.Fatalf("output missing %q: %q", want, out)
				}
			}
		})
	}
}

func TestInteractiveClientWaitBoundsPipedReplies(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()
	release := make(chan struct{})
	defer close(release)
	// Answers once, then keeps the connection open after the client's FIN.
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := bufio.NewReader(conn).ReadString('\n'); err == nil {
			conn.Write([]byte("pong\n"))
		}
		<-release
	}()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	defer stdinR.Close()
	stdinW.WriteString("ping\n")
	stdinW.Close()

	started := time.Now()
	var code int
	out := captureStdoutWithArgsAndStdin(t,
		[]string{"coe", "-c", listener.Addr().String(), "--wait", "300ms", "--no-color"},
		stdinR,
		func() { code = runClientWithExitCode() })
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("client waited %s despite --wait 300ms", elapsed)
	}
	if code != exitOK || !strings.Contains(out, "| pong (Bytes") {
		t.Fatalf("code=%d output=%q", code, out)
	}
}

func TestInteractiveClientWithPipedInputKeepsReadingReplies(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()

	// The server answers only after the client has finished sending, as a
	// batch processor would; the client must still be reading by then.
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		request, _ := io.ReadAll(conn)
		for _, line := range strings.Split(strings.TrimSuffix(string(request), "\n"), "\n") {
			fmt.Fprintf(conn, "re:%s\n", line)
		}
	}()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}
	defer stdinR.Close()
	if _, err := stdinW.WriteString("a\nb\n"); err != nil {
		t.Fatalf("stdin write error: %v", err)
	}
	stdinW.Close()

	addr := listener.Addr().(*net.TCPAddr)
	var code int
	out := captureStdoutWithArgsAndStdin(t,
		[]string{"coe", "-c", addr.String(), "--no-color"},
		stdinR,
		func() { code = runClientWithExitCode() })

	if code != exitOK {
		t.Fatalf("exit code = %d, want 0; output=%q", code, out)
	}
	for _, want := range []string{"| re:a (Bytes", "| re:b (Bytes", "Connection closed by peer"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "Receive error") {
		t.Fatalf("a normal close was reported as an error: %q", out)
	}
}

func TestHandleClientDoesNotReportNormalCloseAsError(t *testing.T) {
	colorEnabled = false
	server, client := net.Pipe()
	out := captureStdout(t, func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			handleClient(server, serverConfig{recvTerminatorBytes: []byte{'\n'}, sendTerminatorBytes: []byte{'\n'}, bufferSize: 16})
		}()
		client.Close()
		waitDone(t, done)
	})
	if strings.Contains(out, "Receive error") || !strings.Contains(out, "Client disconnected") {
		t.Fatalf("output = %q", out)
	}
}

type failingListener struct {
	mu       sync.Mutex
	failures int
	calls    int
}

func (l *failingListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.calls <= l.failures {
		return nil, errors.New("accept: too many open files")
	}
	return nil, net.ErrClosed
}
func (l *failingListener) Close() error   { return nil }
func (l *failingListener) Addr() net.Addr { return dummyAddr("listener") }

func TestAcceptClientsBacksOffOnRepeatedErrors(t *testing.T) {
	l := &failingListener{failures: 4}
	var clients sync.Map
	var count atomic.Int64
	var shuttingDown atomic.Bool
	started := time.Now()
	out := captureStdout(t, func() {
		acceptClients(l, serverConfig{}, &clients, &count, &shuttingDown, nil)
	})
	// 5ms + 10ms + 20ms + 40ms between the four failures.
	if elapsed := time.Since(started); elapsed < 70*time.Millisecond {
		t.Fatalf("accept loop retried without backing off (%s)", elapsed)
	}
	if got := strings.Count(out, "retrying in"); got != 4 {
		t.Fatalf("logged %d retries, want 4: %q", got, out)
	}
}

func TestOneShotClientWritesLogFile(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := bufio.NewReader(conn).ReadString('\n'); err == nil {
			conn.Write([]byte("pong\x1b\n"))
		}
	}()

	logPath := t.TempDir() + "/session.log"
	var code int
	stdout, stderr := captureStdoutAndStderr(t, func() {
		originalArgs := os.Args
		os.Args = []string{"coe", "-c", listener.Addr().String(), "-m", "ping", "--wait", "200ms", "--quiet", "--log", logPath}
		defer func() { os.Args = originalArgs }()
		code = runClientWithExitCode()
	})
	if code != exitOK {
		t.Fatalf("exit code = %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	log := string(data)
	for _, want := range []string{"client connecting to " + listener.Addr().String(), "[Send] ", "| ping (Bytes: 5", "[Recv] ", `| pong\x1B (Bytes: 6`} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "\x1b") {
		t.Fatalf("log contains raw escape bytes:\n%q", log)
	}
}

func TestConnectTimeoutBoundsDial(t *testing.T) {
	// 192.0.2.0/24 is reserved for documentation and is never routed, so the
	// dial either times out or fails at once; it must not hang.
	started := time.Now()
	var code int
	captureStdoutAndStderr(t, func() {
		originalArgs := os.Args
		os.Args = []string{"coe", "-c", "192.0.2.1", "9", "-m", "x", "--connect-timeout", "200ms"}
		defer func() { os.Args = originalArgs }()
		code = runClientWithExitCode()
	})
	if code != exitIO {
		t.Fatalf("exit code = %d, want %d", code, exitIO)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("dial took %s with a 200ms connect timeout", elapsed)
	}
}
