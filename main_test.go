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
	"strconv"
	"strings"
	"sync"
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

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error: %v", err)
	}

	os.Stdout = w
	defer func() {
		os.Stdout = originalStdout
		_ = r.Close()
	}()

	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("stdout pipe close error: %v", err)
	}

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("stdout pipe read error: %v", err)
	}
	return string(out)
}

func captureStdoutAndStderr(t *testing.T, fn func()) (string, string) {
	t.Helper()

	originalStdout := os.Stdout
	originalStderr := os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout os.Pipe() error: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		t.Fatalf("stderr os.Pipe() error: %v", err)
	}

	os.Stdout = stdoutW
	os.Stderr = stderrW
	defer func() {
		os.Stdout = originalStdout
		os.Stderr = originalStderr
		_ = stdoutR.Close()
		_ = stderrR.Close()
	}()

	fn()
	if err := stdoutW.Close(); err != nil {
		t.Fatalf("stdout pipe close error: %v", err)
	}
	if err := stderrW.Close(); err != nil {
		t.Fatalf("stderr pipe close error: %v", err)
	}

	stdout, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatalf("stdout pipe read error: %v", err)
	}
	stderr, err := io.ReadAll(stderrR)
	if err != nil {
		t.Fatalf("stderr pipe read error: %v", err)
	}
	return string(stdout), string(stderr)
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
			out := captureStdoutWithArgs(t, tt.args, func() {
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
			out := captureStdoutWithArgs(t, tt.args, func() {
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
		// Keep the connection open so the client must use the complete wait
		// duration instead of stopping after the first response frame.
		time.Sleep(180 * time.Millisecond)
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
		time.Sleep(100 * time.Millisecond)
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
			out := captureStdoutWithArgs(t, tt.args, func() {
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
		<-serverDone
		time.Sleep(50 * time.Millisecond)
		if err := stdinW.Close(); err != nil {
			t.Errorf("stdin close error: %v", err)
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	out := captureStdoutWithArgsAndStdin(
		t,
		[]string{"coe", "-c", addr.IP.String(), strconv.Itoa(addr.Port), "LF", "--buffer-size", "3", "--no-color"},
		stdinR,
		runClient,
	)
	<-serverDone
	<-stdinDone

	for _, want := range []string{
		"Connection successful:",
		"Buffer size: 3 bytes",
		"Send] ",
		"hello (Bytes: 6",
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
		handleClient(server, []byte{'\n'}, []byte{'\r', '\n'}, true, 2, 0, 0, 0)
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
		handleClient(server, []byte{'\n'}, []byte{'\n'}, false, 8, 0, 0, 0)
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
		handleClient(conn, []byte{'\n'}, []byte{'\n'}, true, 32, 0, 0, 0)
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
		handleClient(server, []byte{'\r'}, []byte{'\n'}, true, 32, 0, 0, 0)
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
			handleClient(server, []byte{'\n'}, []byte{'\n'}, false, 32, 0, 4, 0)
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
	out := captureStdoutWithArgs(t, []string{"coe", "-c", "127.0.0.1", port, "--no-color"}, func() {
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
	var code int
	out := captureStdoutWithArgs(t, []string{"coe", "-s", "not-a-port", "--no-color"}, func() {
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
