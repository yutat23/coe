package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

// version is the single source of the release number. build.ps1 reads it
// from here; a build can still override it with -ldflags "-X main.version=...".
var version = "0.1.10"

const (
	defaultBufferSize     = 1024
	defaultMaxFrameSize   = 1024 * 1024
	defaultMaxClients     = 64
	defaultWriteTimeout   = 5 * time.Second
	defaultConnectTimeout = 10 * time.Second
	// A shorter flush timeout makes the read loop spin on deadlines.
	minFlushTimeout = time.Millisecond

	exitOK         = 0
	exitIO         = 1
	exitUsage      = 2
	exitNoResponse = 3
)

var errFrameTooLarge = errors.New("frame exceeds max size")

// Color codes
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorWhite  = "\033[37m"
)

var colorEnabled bool

// colorMode is the --color / --no-color choice; the TUI passes it on to
// Bubble Tea, which would otherwise decide on its own (and honor NO_COLOR
// even over --color).
var colorMode colorChoice
var terminalControlEnabled bool

func parseTerminator(name string) ([]byte, error) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "LF":
		return []byte{0x0A}, nil
	case "CR":
		return []byte{0x0D}, nil
	case "CRLF":
		return []byte{0x0D, 0x0A}, nil
	default:
		return nil, fmt.Errorf("terminator must be LF, CR, or CRLF")
	}
}

func terminatorToken(arg string) bool {
	switch strings.ToUpper(strings.TrimSpace(arg)) {
	case "LF", "CR", "CRLF":
		return true
	default:
		return false
	}
}

func terminatorHexDescription(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("0x%02X", x)
	}
	return strings.Join(parts, " ")
}

func parseFlushTimeout(value string) (time.Duration, error) {
	if strings.EqualFold(value, "off") || strings.EqualFold(value, "none") || value == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("flush timeout must be a duration like 100ms, 1s, or 0/off")
	}
	if d < 0 {
		return 0, fmt.Errorf("flush timeout must be 0 or greater")
	}
	if d > 0 && d < minFlushTimeout {
		return 0, fmt.Errorf("flush timeout must be at least %s, or 0/off to disable it", minFlushTimeout)
	}
	return d, nil
}

func parseWaitDuration(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("wait duration must be a duration like 500ms or 1s")
	}
	if d < 0 {
		return 0, fmt.Errorf("wait duration must be 0 or greater")
	}
	return d, nil
}

func parseBufferSize(value string) (int, error) {
	size, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("Buffer size must be a number")
	}
	if size <= 0 {
		return 0, fmt.Errorf("Buffer size must be 1 or greater")
	}
	return size, nil
}

func parseMaxFrameSize(value string) (int, error) {
	size, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("Max frame size must be a number")
	}
	if size < 0 {
		return 0, fmt.Errorf("Max frame size must be 0 or greater")
	}
	return size, nil
}

func parseMaxClients(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("Max clients must be a number")
	}
	if n < 0 {
		return 0, fmt.Errorf("Max clients must be 0 or greater")
	}
	return n, nil
}

// parseTimeout parses a timeout where 0, off, and none disable it.
func parseTimeout(name, value string) (time.Duration, error) {
	if strings.EqualFold(value, "off") || strings.EqualFold(value, "none") || value == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration like 5s, 100ms, or 0/off", name)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must be 0 or greater", name)
	}
	return d, nil
}

func parseWriteTimeout(value string) (time.Duration, error) {
	return parseTimeout("write timeout", value)
}

func parseConnectTimeout(value string) (time.Duration, error) {
	return parseTimeout("connect timeout", value)
}

// parsePort accepts a decimal TCP port. Port 0 (pick a free port) is only
// meaningful for a listener.
func parsePort(value string, allowZero bool) (string, error) {
	low := 1
	if allowZero {
		low = 0
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < low || n > 65535 || strings.HasPrefix(value, "+") {
		return "", fmt.Errorf("port must be a number from %d to 65535, got %q", low, value)
	}
	return strconv.Itoa(n), nil
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseServerAddress accepts "<port>" or "<host>:<port>" ("[::1]:8080" for
// IPv6). An empty host listens on every interface.
func parseServerAddress(arg string) (host, port string, err error) {
	if strings.Contains(arg, ":") {
		h, p, splitErr := net.SplitHostPort(arg)
		if splitErr != nil {
			return "", "", fmt.Errorf("address must be <port> or <host>:<port>, got %q", arg)
		}
		host, arg = h, p
	}
	port, err = parsePort(arg, true)
	return host, port, err
}

// parseClientTarget reads "<host> <port>" or "<host>:<port>" from args and
// returns the dial address and how many arguments it used. A bare IPv6
// address such as ::1 does not split, so it is treated as a host.
func parseClientTarget(args []string) (address string, used int, err error) {
	if len(args) >= 1 {
		if host, port, splitErr := net.SplitHostPort(args[0]); splitErr == nil && (len(args) == 1 || !isDecimal(args[1])) {
			p, err := parsePort(port, false)
			if err != nil {
				return "", 0, err
			}
			return net.JoinHostPort(host, p), 1, nil
		}
	}
	if len(args) < 2 {
		return "", 0, errMissingTarget
	}
	p, err := parsePort(args[1], false)
	if err != nil {
		return "", 0, err
	}
	return net.JoinHostPort(args[0], p), 2, nil
}

var errMissingTarget = errors.New("missing host and port")

func writeConn(conn net.Conn, data []byte, writeTimeout time.Duration) error {
	if writeTimeout > 0 {
		if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return err
		}
	}
	n, err := conn.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

// splitHead splits on the first space or tab. Leading whitespace is skipped
// before the head token; everything after that single separator is returned
// unchanged so that message payloads can keep repeated, leading, and trailing spaces.
func splitHead(s string) (head, rest string) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", ""
	}
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

// displayText makes a payload safe to print on a terminal. Control bytes, C1
// controls, and invalid UTF-8 are written with the same escape syntax that
// outgoing messages accept (\r, \n, \t, \xHH), so a peer cannot move the
// cursor, retitle the window, or hide the rest of a log line. A backslash in the
// payload is left unchanged.
func displayText(s string) string {
	needsEscape := false
	for _, r := range s {
		if r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) || r == utf8.RuneError {
			needsEscape = true
			break
		}
	}
	if !needsEscape {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) || (r == utf8.RuneError && size == 1):
			for _, c := range []byte(s[i : i+size]) {
				fmt.Fprintf(&b, `\x%02X`, c)
			}
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// formatServerLine renders one server-side data event:
// "[addr] timestamp | Verb: message (Bytes: n, HEX: ...)".
func formatServerLine(timestamp, addr, verb, verbColor, message string, raw []byte, color bool) string {
	hexData := fmt.Sprintf("%x", raw)
	message = displayText(message)
	if color {
		return fmt.Sprintf("%s[%s]%s %s%s%s | %s%s:%s %s (Bytes: %s%d%s, HEX: %s%s%s)",
			colorBlue, addr, colorReset,
			colorYellow, timestamp, colorReset,
			verbColor, verb, colorReset, message,
			colorCyan, len(raw), colorReset,
			colorPurple, hexData, colorReset)
	}
	return fmt.Sprintf("[%s] %s | %s: %s (Bytes: %d, HEX: %s)",
		addr, timestamp, verb, message, len(raw), hexData)
}

// formatClientLine renders one client-side data event:
// "[Tag] timestamp | message (Bytes: n, HEX: ...)".
func formatClientLine(timestamp, tag, tagColor, message string, raw []byte, color bool) string {
	hexData := fmt.Sprintf("%x", raw)
	message = displayText(message)
	if color {
		return fmt.Sprintf("%s[%s]%s %s%s%s | %s (Bytes: %s%d%s, HEX: %s%s%s)",
			tagColor, tag, colorReset,
			colorYellow, timestamp, colorReset,
			message,
			colorCyan, len(raw), colorReset,
			colorPurple, hexData, colorReset)
	}
	return fmt.Sprintf("[%s] %s | %s (Bytes: %d, HEX: %s)",
		tag, timestamp, message, len(raw), hexData)
}

// logServerData prints one server-side data event and mirrors it to --log.
func logServerData(addr, verb, verbColor, message string, raw []byte) {
	timestamp := logTimestamp()
	fmt.Println(formatServerLine(timestamp, addr, verb, verbColor, message, raw, colorEnabled))
	logLine(formatServerLine(timestamp, addr, verb, verbColor, message, raw, false))
}

// logClientDataTo prints one client-side data event to w and mirrors it to --log.
func logClientDataTo(w io.Writer, tag, tagColor, message string, raw []byte) {
	timestamp := logTimestamp()
	fmt.Fprintln(w, formatClientLine(timestamp, tag, tagColor, message, raw, colorEnabled))
	logLine(formatClientLine(timestamp, tag, tagColor, message, raw, false))
}

func logClientData(tag, tagColor, message string, raw []byte) {
	logClientDataTo(os.Stdout, tag, tagColor, message, raw)
}

// appendAndFlushBySuffix frames messages by recvTerm. When recvTerm is CR only, an LF immediately
// after a completed frame is dropped so peer CRLF does not appear as a second (empty) message.
// maxFrameSize counts buffered bytes including the terminator; 0 means no limit.
func appendAndFlushBySuffix(messageBuffer *bytes.Buffer, data []byte, recvTerm []byte, pendingSkipLF *bool, maxFrameSize int, onMessage func(msg string)) error {
	swallowLFAfterCR := len(recvTerm) == 1 && recvTerm[0] == 0x0D
	for _, b := range data {
		if *pendingSkipLF {
			*pendingSkipLF = false
			if b == 0x0A {
				continue
			}
		}
		messageBuffer.WriteByte(b)
		if maxFrameSize > 0 && messageBuffer.Len() > maxFrameSize {
			return errFrameTooLarge
		}
		bs := messageBuffer.Bytes()
		if len(bs) >= len(recvTerm) && bytes.Equal(bs[len(bs)-len(recvTerm):], recvTerm) {
			msg := string(bs[:len(bs)-len(recvTerm)])
			onMessage(msg)
			messageBuffer.Reset()
			if swallowLFAfterCR {
				*pendingSkipLF = true
			}
		}
	}
	return nil
}

func discardOrphanLFAfterCR(messageBuffer *bytes.Buffer, recvTerm []byte) bool {
	if len(recvTerm) == 1 && recvTerm[0] == 0x0D &&
		messageBuffer.Len() == 1 && messageBuffer.Bytes()[0] == 0x0A {
		messageBuffer.Reset()
		return true
	}
	return false
}

// isClosedByPeer reports a normal end of the connection rather than a failure.
func isClosedByPeer(err error) bool {
	return errors.Is(err, io.EOF)
}

func main() {
	exitCode := runMain()
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

func runMain() int {
	if len(os.Args) < 2 {
		shortUsage()
		return exitOK
	}

	mode := os.Args[1]

	switch mode {
	case "-s", "--server":
		return runServer()
	case "-c", "--client":
		return runClientWithExitCode()
	case "-h", "--help", "help":
		fullUsage()
		return exitOK
	case "-v", "--version", "version":
		fmt.Printf("coe %s\n", version)
		return exitOK
	default:
		errorf("Error: Mode must be '-s'/'--server' or '-c'/'--client'\n")
		shortUsageTo(os.Stderr)
		return exitUsage
	}
}

func showLogo() {
	showLogoTo(os.Stdout)
}

func showLogoTo(w io.Writer) {
	logoLines := []string{
		" ██████╗ ██████╗ ███████╗",
		"██╔════╝██╔═══██╗██╔════╝",
		"██║     ██║   ██║█████╗",
		"██║     ██║   ██║██╔══╝",
		"╚██████╗╚██████╔╝███████╗",
		" ╚═════╝ ╚═════╝ ╚══════╝",
		"",
		" coe - Communicate and echo through sockets.",
		" Version " + version,
	}
	for _, line := range logoLines {
		fmt.Fprintln(w, line)
	}
}

func shortUsage() {
	shortUsageTo(os.Stdout)
}

func shortUsageTo(w io.Writer) {
	showLogoTo(w)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "USAGE")
	fmt.Fprintln(w, "  Server mode:  coe -s [host:]<port> [terminator] [options]")
	fmt.Fprintln(w, "  Client mode:  coe -c <host> <port> [terminator] [options]")
	fmt.Fprintln(w, "                coe -c <host>:<port> [terminator] [options]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Use 'coe --help' for detailed options and examples.")
}

func fullUsage() {
	showLogo()
	fmt.Println("")
	fmt.Println("USAGE")
	fmt.Println("  Server mode:   coe -s, --server [host:]<port> [terminator] [-tt T] [-rt T] [--tx-term T] [--rx-term T]")
	fmt.Println("                 [--bind <host>] [--no-echo] [--buffer-size <size>] [--max-frame-size <size>]")
	fmt.Println("                 [--max-clients <n>] [--write-timeout <duration>] [--flush-timeout <duration>]")
	fmt.Println("                 [--log <file>] [--tui] [--color] [--no-color]")
	fmt.Println("  Client mode:   coe -c, --client <host> <port> | <host>:<port> [terminator] [-tt T] [-rt T]")
	fmt.Println("                 [--tx-term T] [--rx-term T] [-m, --message <message> [--wait <duration>] [--quiet]]")
	fmt.Println("                 [--connect-timeout <duration>] [--buffer-size <size>] [--max-frame-size <size>]")
	fmt.Println("                 [--write-timeout <duration>] [--flush-timeout <duration>] [--log <file>] [--tui]")
	fmt.Println("                 [--color] [--no-color]")
	fmt.Println("  Version:       coe -v, --version")
	fmt.Println("")
	fmt.Println("OPTIONS")
	fmt.Println("Terminator: LF, CR, or CRLF (CR+LF). Positional sets both sides unless overridden; omit when -tt and -rt cover both.")
	fmt.Println("-tt T, --tx-term T   Outgoing delimiter (#send/#broadcast, echo, client send); --send-terminator is an alias")
	fmt.Println("-rt T, --rx-term T   Frame incoming data until this sequence; --recv-terminator is an alias")
	fmt.Println("--bind <host>    Listen on one address only, e.g. 127.0.0.1 (Server mode only); default is every interface")
	fmt.Println("--no-echo        Disable echo back (Server mode only)")
	fmt.Println("--buffer-size    Specify read chunk size (bytes) - Default is 1024")
	fmt.Println("--max-frame-size Maximum assembled receive frame in bytes; 0 is unlimited - Default is 1048576")
	fmt.Println("--max-clients    Maximum concurrent clients (Server mode only); 0 is unlimited - Default is 64")
	fmt.Println("--write-timeout  Deadline for each socket write (e.g. 5s); 0/off disables - Default is 5s")
	fmt.Println("--connect-timeout Give up connecting after this long (Client mode only); 0/off disables - Default is 10s")
	fmt.Println("--flush-timeout  Show incomplete buffered data after inactivity (e.g. 100ms, at least 1ms); Default is off")
	fmt.Println("-m, --message    Send one message and exit without the interactive prompt")
	fmt.Println("--wait           With -m, receive frames for this duration after sending; default is off")
	fmt.Println("                 With piped input, wait at most this long for replies after the input ends")
	fmt.Println("--quiet          With -m, write response payloads to stdout and logs to stderr")
	fmt.Println("--log <file>     Append frames and connection events to a file (no colors)")
	fmt.Println("--tui            Interactive TUI (requires a terminal). Not used with -m / --message")
	fmt.Println("--color          Always color the output")
	fmt.Println("--no-color       Never color the output")
	fmt.Println("                 Default: color only on a terminal, and not when NO_COLOR is set")
	fmt.Println("")
	fmt.Println("COLOR CODING")
	fmt.Println("  Blue    - Client IP addresses")
	fmt.Println("  Green   - Received messages")
	fmt.Println("  Red     - Sent messages")
	fmt.Println("  Yellow  - Timestamps")
	fmt.Println("  Cyan    - Byte counts")
	fmt.Println("  Purple  - Hexadecimal data")
	fmt.Println("")
	fmt.Println("ESCAPE SEQUENCES (in messages)")
	fmt.Println("  \\r     - CR (0x0D)")
	fmt.Println("  \\n     - LF (0x0A)")
	fmt.Println("  \\t     - TAB (0x09)")
	fmt.Println("  \\\\     - Backslash (0x5C)")
	fmt.Println("  \\xHH   - Arbitrary byte in hex (e.g., \\x1B for ESC)")
	fmt.Println("  An empty line in the interactive client sends just the terminator.")
	fmt.Println("")
	fmt.Println("EXAMPLES")
	fmt.Println("  coe -s 8080")
	fmt.Println("  coe -s 8080 CR")
	fmt.Println("  coe -s 127.0.0.1:8080")
	fmt.Println("  coe -s 8080 LF --no-echo")
	fmt.Println("  coe -s 8080 --buffer-size 2048")
	fmt.Println("  coe -s 8080 --flush-timeout 100ms")
	fmt.Println("  coe -s 8080 --max-frame-size 4096 --max-clients 8 --write-timeout 2s")
	fmt.Println("  coe -s 8080 --log session.log")
	fmt.Println("  coe -s 8080 --tui")
	fmt.Println("  coe -c 127.0.0.1 8080 LF")
	fmt.Println("  coe -c 127.0.0.1:8080")
	fmt.Println("  coe -c 127.0.0.1 8080 LF --flush-timeout 100ms")
	fmt.Println("  coe -c 127.0.0.1 8080 -tt CR -rt CRLF")
	fmt.Println("  coe -c 127.0.0.1 8080 -m \"STATUS\\r\" --wait 1s --quiet")
	fmt.Println("  coe -c 127.0.0.1 8080 CR -rt CRLF")
	fmt.Println("  coe --client 192.168.1.100 8080 CR --buffer-size 512 --connect-timeout 3s")
	fmt.Println("  coe --client 192.168.1.100 8080 CR --no-color")
	fmt.Println("  coe --client 192.168.1.100 8080 --tui")
}

func serverUsage() string {
	return "Usage: -s, --server [host:]<port> [terminator] [-tt T] [-rt T] [--tx-term T] [--rx-term T] [--bind <host>] [--no-echo] [--buffer-size <size>] [--max-frame-size <size>] [--max-clients <n>] [--write-timeout <duration>] [--flush-timeout <duration>] [--log <file>] [--tui] [--color] [--no-color]"
}

// parseServerConfig reads the server arguments. The returned error is
// already worded for the user.
func parseServerConfig() (serverConfig, string, colorChoice, error) {
	if len(os.Args) < 3 {
		return serverConfig{}, "", colorAuto, errors.New(serverUsage())
	}

	cfg := serverConfig{
		echoEnabled:  true,
		bufferSize:   defaultBufferSize,
		maxFrameSize: defaultMaxFrameSize,
		maxClients:   defaultMaxClients,
		writeTimeout: defaultWriteTimeout,
	}
	var err error
	cfg.host, cfg.port, err = parseServerAddress(os.Args[2])
	if err != nil {
		return serverConfig{}, "", colorAuto, fmt.Errorf("Error: %w", err)
	}
	hostInAddress := cfg.host != ""

	var legacyTerminator, logPath string
	choice := colorAuto
	value := func(i int, what string) (string, error) {
		if i+1 >= len(os.Args) {
			return "", fmt.Errorf("Error: %s must be specified after %s", what, os.Args[i])
		}
		return os.Args[i+1], nil
	}

	for i := 3; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch {
		case arg == "--no-echo":
			cfg.echoEnabled = false
		case arg == "--buffer-size":
			v, err := value(i, "Buffer size")
			if err != nil {
				return serverConfig{}, "", choice, err
			}
			if cfg.bufferSize, err = parseBufferSize(v); err != nil {
				return serverConfig{}, "", choice, fmt.Errorf("Error: %w", err)
			}
			i++
		case arg == "--max-frame-size":
			v, err := value(i, "Max frame size")
			if err != nil {
				return serverConfig{}, "", choice, err
			}
			if cfg.maxFrameSize, err = parseMaxFrameSize(v); err != nil {
				return serverConfig{}, "", choice, fmt.Errorf("Error: %w", err)
			}
			i++
		case arg == "--max-clients":
			v, err := value(i, "Max clients")
			if err != nil {
				return serverConfig{}, "", choice, err
			}
			if cfg.maxClients, err = parseMaxClients(v); err != nil {
				return serverConfig{}, "", choice, fmt.Errorf("Error: %w", err)
			}
			i++
		case arg == "--write-timeout":
			v, err := value(i, "Write timeout")
			if err != nil {
				return serverConfig{}, "", choice, err
			}
			if cfg.writeTimeout, err = parseWriteTimeout(v); err != nil {
				return serverConfig{}, "", choice, fmt.Errorf("Error: %w", err)
			}
			i++
		case arg == "--flush-timeout":
			v, err := value(i, "Flush timeout")
			if err != nil {
				return serverConfig{}, "", choice, err
			}
			if cfg.flushTimeout, err = parseFlushTimeout(v); err != nil {
				return serverConfig{}, "", choice, fmt.Errorf("Error: %w", err)
			}
			i++
		case arg == "--bind":
			v, err := value(i, "Address")
			if err != nil {
				return serverConfig{}, "", choice, err
			}
			if hostInAddress {
				return serverConfig{}, "", choice, fmt.Errorf("Error: --bind cannot be combined with host:port")
			}
			cfg.host = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
			i++
		case arg == "--log":
			v, err := value(i, "Log file")
			if err != nil {
				return serverConfig{}, "", choice, err
			}
			logPath = v
			i++
		case arg == "-tt" || arg == "--tx-term" || arg == "--send-terminator":
			if i+1 >= len(os.Args) {
				return serverConfig{}, "", choice, fmt.Errorf("Error: Value required after -tt / --tx-term (LF, CR, or CRLF)")
			}
			cfg.sendTerminatorName = os.Args[i+1]
			i++
		case arg == "-rt" || arg == "--rx-term" || arg == "--recv-terminator":
			if i+1 >= len(os.Args) {
				return serverConfig{}, "", choice, fmt.Errorf("Error: Value required after -rt / --rx-term (LF, CR, or CRLF)")
			}
			cfg.recvTerminatorName = os.Args[i+1]
			i++
		case arg == "--tui":
			cfg.tui = true
		case arg == "--color":
			choice = colorOn
		case arg == "--no-color":
			choice = colorOff
		case legacyTerminator == "" && terminatorToken(arg):
			legacyTerminator = arg
		default:
			return serverConfig{}, "", choice, fmt.Errorf("Error: Unknown option or terminator %s", arg)
		}
	}

	if legacyTerminator != "" {
		if cfg.sendTerminatorName == "" {
			cfg.sendTerminatorName = legacyTerminator
		}
		if cfg.recvTerminatorName == "" {
			cfg.recvTerminatorName = legacyTerminator
		}
	}
	if cfg.sendTerminatorName == "" {
		cfg.sendTerminatorName = "LF"
	}
	if cfg.recvTerminatorName == "" {
		cfg.recvTerminatorName = "LF"
	}
	if cfg.sendTerminatorBytes, err = parseTerminator(cfg.sendTerminatorName); err != nil {
		return serverConfig{}, "", choice, fmt.Errorf("Error (-tt / --tx-term): %w", err)
	}
	if cfg.recvTerminatorBytes, err = parseTerminator(cfg.recvTerminatorName); err != nil {
		return serverConfig{}, "", choice, fmt.Errorf("Error (-rt / --rx-term): %w", err)
	}
	return cfg, logPath, choice, nil
}

func runServer() int {
	cfg, logPath, choice, err := parseServerConfig()
	if err != nil {
		errorf("%v\n", err)
		return exitUsage
	}
	colorEnabled = resolveColor(choice, os.Stdout)
	colorMode = choice
	terminalControlEnabled = promptRedrawAvailable()
	if cfg.tui && !tuiAvailable() {
		errorf("Error: --tui requires a terminal\n")
		return exitUsage
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.host, cfg.port))
	if err != nil {
		errorf("Server startup error: %v\n", err)
		return exitIO
	}
	defer listener.Close()
	// With port 0 the system picks one; report the port actually in use.
	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
		cfg.port = strconv.Itoa(tcpAddr.Port)
	}

	if logPath != "" {
		closer, err := openLogFile(logPath)
		if err != nil {
			errorf("Error: cannot open log file: %v\n", err)
			return exitIO
		}
		defer func() {
			setLogWriter(nil)
			closer.Close()
		}()
		logEventf("coe %s server listening on %s", version, listener.Addr())
	}

	if cfg.tui {
		return runServerTUI(cfg, listener)
	}

	fmt.Printf("Server started on port: %s\n", cfg.port)
	fmt.Printf("Listening on: %s\n", listener.Addr())
	fmt.Printf("Send terminator:    %s (%s)\n", strings.ToUpper(cfg.sendTerminatorName), terminatorHexDescription(cfg.sendTerminatorBytes))
	fmt.Printf("Receive terminator: %s (%s)\n", strings.ToUpper(cfg.recvTerminatorName), terminatorHexDescription(cfg.recvTerminatorBytes))
	fmt.Printf("Buffer size: %d bytes\n", cfg.bufferSize)
	if cfg.maxFrameSize > 0 {
		fmt.Printf("Max frame size: %d bytes\n", cfg.maxFrameSize)
	} else {
		fmt.Println("Max frame size: Unlimited")
	}
	if cfg.maxClients > 0 {
		fmt.Printf("Max clients: %d\n", cfg.maxClients)
	} else {
		fmt.Println("Max clients: Unlimited")
	}
	if cfg.writeTimeout > 0 {
		fmt.Printf("Write timeout: %s\n", cfg.writeTimeout)
	} else {
		fmt.Println("Write timeout: Off")
	}
	if cfg.flushTimeout > 0 {
		fmt.Printf("Flush timeout: %s\n", cfg.flushTimeout)
	} else {
		fmt.Println("Flush timeout: Off")
	}
	if cfg.echoEnabled {
		fmt.Println("Echo back: Enabled")
	} else {
		fmt.Println("Echo back: Disabled")
	}
	fmt.Println("Waiting for client connections...")
	fmt.Println("Commands: '#send <clientAddr> <message>' to send to specific client")
	fmt.Println("Commands: '#broadcast <message>' to send to all clients")
	fmt.Println("Commands: '#list' to show connected clients")
	fmt.Println("Commands: '#help' to show available commands")
	fmt.Println("Commands: '#quit', '#exit' to shut down the server")
	fmt.Println("----------------------------------------")

	var clients sync.Map
	var clientCount atomic.Int64
	var shuttingDown atomic.Bool
	var handlers sync.WaitGroup

	shutdown := func() {
		hidePrompt()
		lineOutput(func() { fmt.Println("Shutting down server...") })
		logEventf("Server shutting down")
		shuttingDown.Store(true)
		listener.Close()
		clients.Range(func(key, value interface{}) bool {
			value.(net.Conn).Close()
			sink().printf("Disconnected client: %s\n", key)
			return true
		})
		// Let the handlers print their last lines (and reach --log) before
		// the caller restores the terminal or exits.
		waitBounded(&handlers, time.Second)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	go acceptClients(listener, cfg, &clients, &clientCount, &shuttingDown, &handlers)

	// Read commands on a separate goroutine so that signals are handled in
	// one place and the server can keep running after stdin ends.
	lines := make(chan string)
	inputDone := make(chan error, 1)
	stopInput := make(chan struct{})
	defer close(stopInput)
	stdin := os.Stdin // read once; the goroutine may outlive this call
	go func() {
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 1024), 1024*1024)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-stopInput:
				return
			}
		}
		inputDone <- scanner.Err()
	}()

	setPrompt("Command> ")
	showPrompt()
	for {
		select {
		case <-sigChan:
			shutdown()
			return exitOK
		case line := <-lines:
			promptConsumed()
			if quit := runServerCommand(line, &clients, cfg); quit {
				shutdown()
				return exitOK
			}
			showPrompt()
		case err := <-inputDone:
			inputDone = nil
			if stdinIsTerminal() {
				if err != nil {
					hidePrompt()
					errorf("Input error: %v\n", err)
					shutdown()
					return exitIO
				}
				// Ctrl+D on a terminal is a request to leave.
				shutdown()
				return exitOK
			}
			// Background jobs, services, and containers often start with
			// stdin closed, at /dev/null, or unreadable (GNU nohup reopens it
			// write-only); none of that may stop the server.
			reason := "Standard input closed"
			if err != nil {
				reason = fmt.Sprintf("Standard input unreadable (%v)", err)
			}
			lineOutput(func() {
				fmt.Printf("%s; commands are off. The server keeps running until Ctrl+C or SIGTERM.\n", reason)
			})
		}
	}
}

// runServerCommand runs one line of the server prompt and reports whether
// the server should shut down.
func runServerCommand(line string, clients *sync.Map, cfg serverConfig) (quit bool) {
	cmd, rest := splitHead(line)
	if cmd == "" {
		return false
	}
	switch cmd {
	case "#send":
		clientAddr, message := splitHead(rest)
		if clientAddr == "" {
			lineOutput(func() { fmt.Println("Usage: #send <clientAddr> <message>") })
		} else {
			sendToClient(clients, clientAddr, message, cfg.sendTerminatorBytes, cfg.writeTimeout)
		}
	case "#broadcast":
		broadcastToAll(clients, rest, cfg.sendTerminatorBytes, cfg.writeTimeout)
	case "#list":
		listClients(clients)
	case "#help":
		sub, _ := splitHead(rest)
		if sub == "program" {
			lineOutput(fullUsage)
		} else {
			lineOutput(printServerHelp)
		}
	case "#quit", "#exit":
		return true
	default:
		lineOutput(func() {
			fmt.Printf("Unknown command: %s\n", cmd)
			fmt.Println("Available commands: #send, #broadcast, #list, #help, #quit")
		})
	}
	return false
}

// waitBounded waits for wg, but gives up after limit so a stuck connection
// cannot hold up shutdown.
func waitBounded(wg *sync.WaitGroup, limit time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
	}
}

// acceptClients serves connections until the listener closes. handlers, if
// not nil, tracks the per-connection goroutines.
func acceptClients(listener net.Listener, cfg serverConfig, clients *sync.Map, clientCount *atomic.Int64, shuttingDown *atomic.Bool, handlers *sync.WaitGroup) {
	var delay time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			if shuttingDown.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			// Errors such as EMFILE (too many open files) repeat at once;
			// back off so the loop does not spin and flood the log.
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else {
				delay = min(2*delay, time.Second)
			}
			sink().printf("Connection error: %v (retrying in %s)\n", err, delay)
			time.Sleep(delay)
			continue
		}
		delay = 0

		clientAddr := conn.RemoteAddr().String()
		if cfg.maxClients > 0 && int(clientCount.Load()) >= cfg.maxClients {
			sink().printf("Rejected client %s: max clients (%d) reached\n", clientAddr, cfg.maxClients)
			conn.Close()
			continue
		}

		sink().clientConnected(clientAddr)
		clients.Store(clientAddr, conn)
		clientCount.Add(1)

		if handlers != nil {
			handlers.Add(1)
		}
		go func() {
			if handlers != nil {
				defer handlers.Done()
			}
			handleClient(conn, cfg)
			clients.Delete(clientAddr)
			clientCount.Add(-1)
		}()
	}
}

func handleClient(conn net.Conn, cfg serverConfig) {
	clientAddr := conn.RemoteAddr().String()
	defer conn.Close()
	defer func() { sink().clientDisconnected(clientAddr) }()

	recvTerminatorBytes := cfg.recvTerminatorBytes
	sendTerminatorBytes := cfg.sendTerminatorBytes
	echoEnabled := cfg.echoEnabled
	bufferSize := cfg.bufferSize
	flushTimeout := cfg.flushTimeout
	maxFrameSize := cfg.maxFrameSize
	writeTimeout := cfg.writeTimeout

	buffer := make([]byte, bufferSize)
	var messageBuffer bytes.Buffer
	var pendingSkipLF bool

	flushIncomplete := func() {
		if discardOrphanLFAfterCR(&messageBuffer, recvTerminatorBytes) {
			return
		}
		if messageBuffer.Len() == 0 {
			return
		}
		sink().serverFrame(clientAddr, "Received", colorGreen, messageBuffer.String(), messageBuffer.Bytes())
		messageBuffer.Reset()
	}

	for {
		if flushTimeout > 0 {
			conn.SetReadDeadline(time.Now().Add(flushTimeout))
		}
		n, err := conn.Read(buffer)

		// Per io.Reader contract, process the returned bytes before handling err:
		// the final data may be delivered together with io.EOF.
		if n > 0 {
			data := buffer[:n]
			stopClient := false
			frameErr := appendAndFlushBySuffix(&messageBuffer, data, recvTerminatorBytes, &pendingSkipLF, maxFrameSize, func(message string) {
				if stopClient {
					return
				}
				fullFrame := message + string(recvTerminatorBytes)
				sink().serverFrame(clientAddr, "Received", colorGreen, message, []byte(fullFrame))

				if echoEnabled {
					response := message + string(sendTerminatorBytes)
					if err := writeConn(conn, []byte(response), writeTimeout); err != nil {
						sink().printf("[%s] Send error: %v\n", clientAddr, err)
						stopClient = true
						return
					}
					sink().serverFrame(clientAddr, "Sent", colorRed, message, []byte(response))
				}
			})
			if frameErr != nil {
				sink().printf("[%s] %v (limit %d bytes)\n", clientAddr, frameErr, maxFrameSize)
				return
			}
			if stopClient {
				return
			}
		}

		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			flushIncomplete()
			continue
		}

		if err != nil {
			flushIncomplete()
			// A peer that closes normally, or a connection this side closed
			// (#quit, a failed write), is reported by clientDisconnected.
			if !isClosedByPeer(err) && !errors.Is(err, net.ErrClosed) {
				sink().printf("[%s] Receive error: %v\n", clientAddr, err)
			}
			break
		}
	}
}

func dropClient(clients *sync.Map, addr string, conn net.Conn) {
	if conn != nil {
		conn.Close()
	}
	clients.Delete(addr)
}

func sendToClient(clients *sync.Map, clientAddr string, message string, terminatorBytes []byte, writeTimeout time.Duration) {
	processedMessage := processEscapeSequences(message)

	value, ok := clients.Load(clientAddr)
	if !ok {
		sink().printf("Client not found: %s\n", clientAddr)
		return
	}
	conn := value.(net.Conn)
	response := processedMessage + string(terminatorBytes)
	if err := writeConn(conn, []byte(response), writeTimeout); err != nil {
		sink().printf("Send error [%s]: %v\n", clientAddr, err)
		dropClient(clients, clientAddr, conn)
		return
	}
	sink().serverFrame(clientAddr, "Sent", colorRed, message, []byte(response))
}

func broadcastToAll(clients *sync.Map, message string, terminatorBytes []byte, writeTimeout time.Duration) {
	processedMessage := processEscapeSequences(message)
	responseBytes := []byte(processedMessage + string(terminatorBytes))

	var wg sync.WaitGroup
	var mu sync.Mutex
	count := 0

	clients.Range(func(key, value interface{}) bool {
		addr := key.(string)
		conn := value.(net.Conn)
		wg.Add(1)
		go func(addr string, conn net.Conn) {
			defer wg.Done()
			if err := writeConn(conn, responseBytes, writeTimeout); err != nil {
				sink().printf("Send error [%s]: %v\n", addr, err)
				dropClient(clients, addr, conn)
				return
			}
			sink().serverFrame(addr, "Sent", colorRed, message, responseBytes)
			mu.Lock()
			count++
			mu.Unlock()
		}(addr, conn)
		return true
	})
	wg.Wait()
	sink().printf("Broadcast completed: sent to %d clients\n", count)
}

func listClients(clients *sync.Map) {
	count := 0
	sink().printf("Connected clients:\n")
	clients.Range(func(key, value interface{}) bool {
		sink().printf("  %s\n", key)
		count++
		return true
	})
	if count == 0 {
		sink().printf("  No clients connected\n")
	} else {
		sink().printf("Total: %d clients\n", count)
	}
}

func printServerHelp() {
	fmt.Println("Server mode commands:")
	fmt.Println("  #send <clientAddr> <message>: Send a message to a specific client (use the IP:port shown by #list)")
	fmt.Println("  #broadcast <message>: Send a message to all connected clients")
	fmt.Println("  #list: Show all connected clients")
	fmt.Println("  #help: Show this help message")
	fmt.Println("  #quit, #exit: Shut down the server")
	fmt.Println("")
	fmt.Println("Escape sequences in messages:")
	fmt.Println("  \\r  → CR (0x0D)")
	fmt.Println("  \\n  → LF (0x0A)")
	fmt.Println("  \\t  → TAB (0x09)")
	fmt.Println("  \\\\  → Backslash (0x5C)")
	fmt.Println("  \\xHH → Arbitrary byte (e.g., \\x1B for ESC)")
	fmt.Println("")
	fmt.Println("Program help: Type '#help program' for full program usage")
}

// processEscapeSequences converts escape sequences in a string to their byte values
func processEscapeSequences(input string) string {
	var result strings.Builder
	i := 0
	for i < len(input) {
		if input[i] == '\\' && i+1 < len(input) {
			switch input[i+1] {
			case 'r':
				result.WriteByte(0x0D) // CR
				i += 2
			case 'n':
				result.WriteByte(0x0A) // LF
				i += 2
			case 't':
				result.WriteByte(0x09) // TAB
				i += 2
			case '\\':
				result.WriteByte(0x5C) // Backslash
				i += 2
			case 'x':
				// Handle \xHH format; both digits must be valid hex or the
				// sequence is kept as-is (Sscanf would accept "1g" as 0x01).
				if i+3 < len(input) {
					hexStr := input[i+2 : i+4]
					if v, err := strconv.ParseUint(hexStr, 16, 8); err == nil {
						result.WriteByte(byte(v))
						i += 4
						continue
					}
				}
				// Invalid \x sequence, keep as-is
				result.WriteByte(input[i])
				i++
			default:
				// Unknown escape sequence, keep as-is
				result.WriteByte(input[i])
				i++
			}
		} else {
			result.WriteByte(input[i])
			i++
		}
	}
	return result.String()
}

type clientConfig struct {
	address             string
	legacyTerminator    string
	sendTerminatorName  string
	recvTerminatorName  string
	sendTerminatorBytes []byte
	recvTerminatorBytes []byte
	bufferSize          int
	maxFrameSize        int
	writeTimeout        time.Duration
	connectTimeout      time.Duration
	flushTimeout        time.Duration
	message             string
	messageSet          bool
	wait                time.Duration
	waitSet             bool
	quiet               bool
	tui                 bool
	logPath             string
	color               colorChoice
}

func clientUsage() string {
	return "Usage: -c, --client <host> <port> | <host>:<port> [terminator] [-tt T] [-rt T] [--tx-term T] [--rx-term T] [-m, --message <message> [--wait <duration>] [--quiet]] [--connect-timeout <duration>] [--buffer-size <size>] [--max-frame-size <size>] [--write-timeout <duration>] [--flush-timeout <duration>] [--log <file>] [--tui] [--color] [--no-color]"
}

func parseClientConfig() (clientConfig, error) {
	address, used, err := parseClientTarget(os.Args[2:])
	if errors.Is(err, errMissingTarget) {
		return clientConfig{}, fmt.Errorf("%s\nTerminator: LF, CR, or CRLF. Positional is optional if -tt and -rt specify send and receive.", clientUsage())
	}
	if err != nil {
		return clientConfig{}, fmt.Errorf("Error: %w", err)
	}

	cfg := clientConfig{
		address:        address,
		bufferSize:     defaultBufferSize,
		maxFrameSize:   defaultMaxFrameSize,
		writeTimeout:   defaultWriteTimeout,
		connectTimeout: defaultConnectTimeout,
	}

	argi := 2 + used
	if argi < len(os.Args) && !strings.HasPrefix(os.Args[argi], "-") {
		if !terminatorToken(os.Args[argi]) {
			return clientConfig{}, fmt.Errorf("Error: Terminator must be LF, CR, or CRLF (or omit and use -tt / -rt)")
		}
		cfg.legacyTerminator = os.Args[argi]
		argi++
	}

	value := func(i int, what string) (string, error) {
		if i+1 >= len(os.Args) {
			return "", fmt.Errorf("Error: %s must be specified after %s", what, os.Args[i])
		}
		return os.Args[i+1], nil
	}

	for i := argi; i < len(os.Args); i++ {
		arg := os.Args[i]
		var err error
		var v string
		switch arg {
		case "--buffer-size":
			if v, err = value(i, "Buffer size"); err != nil {
				return clientConfig{}, err
			}
			if cfg.bufferSize, err = parseBufferSize(v); err != nil {
				return clientConfig{}, fmt.Errorf("Error: %w", err)
			}
			i++
		case "--max-frame-size":
			if v, err = value(i, "Max frame size"); err != nil {
				return clientConfig{}, err
			}
			if cfg.maxFrameSize, err = parseMaxFrameSize(v); err != nil {
				return clientConfig{}, fmt.Errorf("Error: %w", err)
			}
			i++
		case "--write-timeout":
			if v, err = value(i, "Write timeout"); err != nil {
				return clientConfig{}, err
			}
			if cfg.writeTimeout, err = parseWriteTimeout(v); err != nil {
				return clientConfig{}, fmt.Errorf("Error: %w", err)
			}
			i++
		case "--connect-timeout":
			if v, err = value(i, "Connect timeout"); err != nil {
				return clientConfig{}, err
			}
			if cfg.connectTimeout, err = parseConnectTimeout(v); err != nil {
				return clientConfig{}, fmt.Errorf("Error: %w", err)
			}
			i++
		case "--flush-timeout":
			if v, err = value(i, "Flush timeout"); err != nil {
				return clientConfig{}, err
			}
			if cfg.flushTimeout, err = parseFlushTimeout(v); err != nil {
				return clientConfig{}, fmt.Errorf("Error: %w", err)
			}
			i++
		case "-tt", "--tx-term", "--send-terminator":
			if i+1 >= len(os.Args) {
				return clientConfig{}, fmt.Errorf("Error: Value required after -tt / --tx-term (LF, CR, or CRLF)")
			}
			cfg.sendTerminatorName = os.Args[i+1]
			i++
		case "-rt", "--rx-term", "--recv-terminator":
			if i+1 >= len(os.Args) {
				return clientConfig{}, fmt.Errorf("Error: Value required after -rt / --rx-term (LF, CR, or CRLF)")
			}
			cfg.recvTerminatorName = os.Args[i+1]
			i++
		case "-m", "--message":
			if i+1 >= len(os.Args) {
				return clientConfig{}, fmt.Errorf("Error: Message must be specified after -m / --message")
			}
			cfg.message = os.Args[i+1]
			cfg.messageSet = true
			i++
		case "--wait":
			if v, err = value(i, "Wait duration"); err != nil {
				return clientConfig{}, err
			}
			if cfg.wait, err = parseWaitDuration(v); err != nil {
				return clientConfig{}, fmt.Errorf("Error: %w", err)
			}
			cfg.waitSet = true
			i++
		case "--log":
			if v, err = value(i, "Log file"); err != nil {
				return clientConfig{}, err
			}
			cfg.logPath = v
			i++
		case "--quiet":
			cfg.quiet = true
		case "--tui":
			cfg.tui = true
		case "--color":
			cfg.color = colorOn
		case "--no-color":
			cfg.color = colorOff
		default:
			return clientConfig{}, fmt.Errorf("Error: Unknown option %s", arg)
		}
	}

	if cfg.quiet && !cfg.messageSet {
		return clientConfig{}, fmt.Errorf("Error: --quiet is only used with -m / --message")
	}

	if cfg.legacyTerminator != "" {
		if cfg.sendTerminatorName == "" {
			cfg.sendTerminatorName = cfg.legacyTerminator
		}
		if cfg.recvTerminatorName == "" {
			cfg.recvTerminatorName = cfg.legacyTerminator
		}
	}
	if cfg.sendTerminatorName == "" {
		cfg.sendTerminatorName = "LF"
	}
	if cfg.recvTerminatorName == "" {
		cfg.recvTerminatorName = "LF"
	}

	cfg.sendTerminatorBytes, err = parseTerminator(cfg.sendTerminatorName)
	if err != nil {
		return clientConfig{}, fmt.Errorf("Error (-tt / --tx-term): %w", err)
	}
	cfg.recvTerminatorBytes, err = parseTerminator(cfg.recvTerminatorName)
	if err != nil {
		return clientConfig{}, fmt.Errorf("Error (-rt / --rx-term): %w", err)
	}
	return cfg, nil
}

func runClientWithExitCode() int {
	cfg, err := parseClientConfig()
	if err != nil {
		errorf("%v\n", err)
		return exitUsage
	}
	if cfg.messageSet && cfg.tui {
		errorf("Error: --tui cannot be used with -m / --message\n")
		return exitUsage
	}
	// Without -m, --wait limits how long piped input waits for replies.
	if cfg.waitSet && !cfg.messageSet && (cfg.tui || stdinIsTerminal()) {
		errorf("Error: --wait is only used with -m / --message or with piped input\n")
		return exitUsage
	}
	if cfg.tui && !tuiAvailable() {
		errorf("Error: --tui requires a terminal\n")
		return exitUsage
	}
	logOut := os.Stdout
	if cfg.quiet {
		logOut = os.Stderr
	}
	colorEnabled = resolveColor(cfg.color, logOut)
	colorMode = cfg.color
	terminalControlEnabled = promptRedrawAvailable()

	if cfg.logPath != "" {
		closer, err := openLogFile(cfg.logPath)
		if err != nil {
			errorf("Error: cannot open log file: %v\n", err)
			return exitIO
		}
		defer func() {
			setLogWriter(nil)
			closer.Close()
		}()
		logEventf("coe %s client connecting to %s", version, cfg.address)
	}

	if cfg.messageSet {
		return runOneShotClient(cfg)
	}
	if cfg.tui {
		return runClientTUI(cfg)
	}
	return runInteractiveClient(cfg)
}

// dialServer connects with the configured timeout so that an address that
// drops packets fails in seconds rather than after the OS default (minutes).
func dialServer(cfg clientConfig) (net.Conn, error) {
	d := net.Dialer{Timeout: cfg.connectTimeout}
	return d.Dial("tcp", cfg.address)
}

func printClientConnectionInfo(w io.Writer, cfg clientConfig) {
	fmt.Fprintf(w, "Connection successful: %s\n", cfg.address)
	fmt.Fprintf(w, "Send terminator:    %s (%s)\n", strings.ToUpper(cfg.sendTerminatorName), terminatorHexDescription(cfg.sendTerminatorBytes))
	fmt.Fprintf(w, "Receive terminator: %s (%s)\n", strings.ToUpper(cfg.recvTerminatorName), terminatorHexDescription(cfg.recvTerminatorBytes))
	fmt.Fprintf(w, "Buffer size: %d bytes\n", cfg.bufferSize)
	if cfg.maxFrameSize > 0 {
		fmt.Fprintf(w, "Max frame size: %d bytes\n", cfg.maxFrameSize)
	} else {
		fmt.Fprintln(w, "Max frame size: Unlimited")
	}
	if cfg.writeTimeout > 0 {
		fmt.Fprintf(w, "Write timeout: %s\n", cfg.writeTimeout)
	} else {
		fmt.Fprintln(w, "Write timeout: Off")
	}
	if cfg.flushTimeout > 0 {
		fmt.Fprintf(w, "Flush timeout: %s\n", cfg.flushTimeout)
	} else {
		fmt.Fprintln(w, "Flush timeout: Off")
	}
}

func runOneShotClient(cfg clientConfig) int {
	logWriter := io.Writer(os.Stdout)
	if cfg.quiet {
		logWriter = os.Stderr
	}

	conn, err := dialServer(cfg)
	if err != nil {
		errorf("Connection error: %v\n", err)
		return exitIO
	}
	defer conn.Close()

	printClientConnectionInfo(logWriter, cfg)

	processedText := processEscapeSequences(cfg.message)
	frame := []byte(processedText + string(cfg.sendTerminatorBytes))
	if err := writeConn(conn, frame, cfg.writeTimeout); err != nil {
		errorf("Send error: %v\n", err)
		return exitUsage
	}
	logClientDataTo(logWriter, "Send", colorCyan, cfg.message, frame)

	if !cfg.waitSet {
		return exitOK
	}
	return waitForOneShotResponses(conn, cfg, logWriter)
}

func waitForOneShotResponses(conn net.Conn, cfg clientConfig, logWriter io.Writer) int {
	deadline := time.Now().Add(cfg.wait)
	buffer := make([]byte, cfg.bufferSize)
	var messageBuffer bytes.Buffer
	var pendingSkipLF bool
	received := false

	emit := func(message string, raw []byte) {
		if cfg.quiet {
			// A newline separates multiple response frames while keeping stdout
			// free of connection metadata and timestamped logs.
			fmt.Fprintln(os.Stdout, message)
			logLine(formatClientLine(logTimestamp(), "Recv", colorGreen, message, raw, false))
			return
		}
		logClientDataTo(logWriter, "Recv", colorGreen, message, raw)
	}

	flushIncomplete := func() {
		if discardOrphanLFAfterCR(&messageBuffer, cfg.recvTerminatorBytes) {
			return
		}
		if messageBuffer.Len() == 0 {
			return
		}
		raw := append([]byte(nil), messageBuffer.Bytes()...)
		emit(messageBuffer.String(), raw)
		received = true
		messageBuffer.Reset()
	}

	for time.Now().Before(deadline) {
		readDeadline := deadline
		if cfg.flushTimeout > 0 {
			flushDeadline := time.Now().Add(cfg.flushTimeout)
			if flushDeadline.Before(readDeadline) {
				readDeadline = flushDeadline
			}
		}
		if err := conn.SetReadDeadline(readDeadline); err != nil {
			errorf("Receive error: %v\n", err)
			return exitIO
		}

		n, err := conn.Read(buffer)
		// Per io.Reader contract, process returned bytes before handling err.
		if n > 0 {
			if frameErr := appendAndFlushBySuffix(&messageBuffer, buffer[:n], cfg.recvTerminatorBytes, &pendingSkipLF, cfg.maxFrameSize, func(message string) {
				received = true
				fullFrame := append([]byte(message), cfg.recvTerminatorBytes...)
				emit(message, fullFrame)
			}); frameErr != nil {
				errorf("Receive error: %v\n", frameErr)
				return exitIO
			}
		}

		if err == nil {
			continue
		}
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			if !time.Now().Before(deadline) {
				if cfg.flushTimeout > 0 {
					flushIncomplete()
				}
				break
			}
			flushIncomplete()
			continue
		}
		if err != io.EOF {
			errorf("Receive error: %v\n", err)
			flushIncomplete()
			return exitIO
		}
		flushIncomplete()
		break
	}

	if !received {
		discardOrphanLFAfterCR(&messageBuffer, cfg.recvTerminatorBytes)
		if messageBuffer.Len() > 0 {
			fmt.Fprintf(logWriter, "No response received within %s (%d bytes arrived without the %s terminator; use --flush-timeout to show them)\n",
				cfg.wait, messageBuffer.Len(), strings.ToUpper(cfg.recvTerminatorName))
		} else {
			fmt.Fprintf(logWriter, "No response received within %s\n", cfg.wait)
		}
		return exitNoResponse
	}
	return exitOK
}

func runInteractiveClient(cfg clientConfig) int {
	conn, err := dialServer(cfg)
	if err != nil {
		errorf("Connection error: %v\n", err)
		return exitIO
	}
	defer conn.Close()

	printClientConnectionInfo(os.Stdout, cfg)
	fmt.Println("Chat started. Enter messages (an empty line sends just the terminator):")
	fmt.Println("----------------------------------------")
	setPrompt("Send> ")

	var clientClosing atomic.Bool
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	go func() {
		if _, ok := <-sigChan; !ok {
			return
		}
		hidePrompt()
		lineOutput(func() { fmt.Println("Disconnecting...") })
		clientClosing.Store(true)
		conn.Close()
		os.Exit(0)
	}()

	recvDone := make(chan error, 1)
	go func() {
		var result error
		defer func() { recvDone <- result }()
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
			raw := append([]byte(nil), messageBuffer.Bytes()...)
			text := messageBuffer.String()
			lineOutput(func() { logClientData("Recv", colorGreen, text, raw) })
			messageBuffer.Reset()
		}

		for {
			if cfg.flushTimeout > 0 {
				conn.SetReadDeadline(time.Now().Add(cfg.flushTimeout))
			}
			n, err := conn.Read(buffer)

			// Per io.Reader contract, process the returned bytes before handling err:
			// the final data may be delivered together with io.EOF.
			if n > 0 {
				data := buffer[:n]
				if frameErr := appendAndFlushBySuffix(&messageBuffer, data, cfg.recvTerminatorBytes, &pendingSkipLF, cfg.maxFrameSize, func(message string) {
					fullFrame := []byte(message + string(cfg.recvTerminatorBytes))
					lineOutput(func() { logClientData("Recv", colorGreen, message, fullFrame) })
				}); frameErr != nil {
					if clientClosing.Load() {
						return
					}
					hidePrompt()
					lineOutput(func() { errorf("Receive error: %v\n", frameErr) })
					logEventf("Receive error: %v", frameErr)
					result = frameErr
					return
				}
			}

			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				flushIncomplete()
				continue
			}

			if err != nil {
				if clientClosing.Load() {
					return
				}
				flushIncomplete()
				hidePrompt()
				if isClosedByPeer(err) {
					lineOutput(func() { fmt.Println("Connection closed by peer") })
					logEventf("Connection closed by peer")
					return
				}
				lineOutput(func() { errorf("Receive error: %v\n", err) })
				logEventf("Receive error: %v", err)
				result = err
				return
			}
		}
	}()

	sendDone := make(chan error, 1)
	stdin := os.Stdin // read once; the goroutines may outlive this call
	go func() {
		var result error
		defer func() { sendDone <- result }()
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 1024), 1024*1024)
		showPrompt()
		for scanner.Scan() {
			promptConsumed()
			if clientClosing.Load() {
				return
			}
			// An empty line is sent as a bare terminator (an empty frame).
			text := scanner.Text()
			processedText := processEscapeSequences(text)
			message := []byte(processedText + string(cfg.sendTerminatorBytes))
			if err := writeConn(conn, message, cfg.writeTimeout); err != nil {
				if clientClosing.Load() {
					return
				}
				lineOutput(func() { errorf("Send error: %v\n", err) })
				logEventf("Send error: %v", err)
				result = err
				return
			}
			lineOutput(func() { logClientData("Send", colorCyan, text, message) })
			showPrompt()
		}
		if err := scanner.Err(); err != nil && !clientClosing.Load() {
			lineOutput(func() { errorf("Input error: %v\n", err) })
			result = err
		}
	}()

	var recvErr, sendErr error
	select {
	case recvErr = <-recvDone:
		clientClosing.Store(true)
		conn.Close()
		// The input goroutine may be blocked reading stdin, and closing stdin
		// does not reliably unblock it (on Windows the close itself waits for a
		// pending console read). Do not wait for it beyond a moment: the
		// process exits right after this returns, which ends that goroutine.
		go func() { _ = stdin.Close() }()
		select {
		case sendErr = <-sendDone:
		case <-time.After(100 * time.Millisecond):
		}
	case sendErr = <-sendDone:
		if sendErr == nil && !stdinIsTerminal() && halfClose(conn) {
			// Piped input ended. The write side is closed so the peer sees
			// EOF, but replies to the last lines are still read until the
			// peer closes the connection, or for --wait if it was given.
			var limit <-chan time.Time
			if cfg.waitSet {
				limit = time.After(cfg.wait)
			}
			select {
			case recvErr = <-recvDone:
				clientClosing.Store(true)
			case <-limit:
				clientClosing.Store(true)
				conn.Close()
				<-recvDone
			}
			break
		}
		clientClosing.Store(true)
		conn.Close()
		recvErr = <-recvDone
	}
	if recvErr != nil || sendErr != nil {
		return exitIO
	}
	return exitOK
}

// halfClose shuts down the sending side of a TCP connection and reports
// whether that worked.
func halfClose(conn net.Conn) bool {
	cw, ok := conn.(interface{ CloseWrite() error })
	return ok && cw.CloseWrite() == nil
}
