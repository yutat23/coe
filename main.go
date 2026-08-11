package main

import (
	"bufio"
	"bytes"
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
)

const version = "0.1.7"

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

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && (info.Mode()&os.ModeCharDevice) != 0
}

func clearInputLine() {
	if terminalControlEnabled {
		fmt.Print("\r\033[K")
	}
}

func printPrompt(prompt string) {
	if terminalControlEnabled {
		fmt.Print(prompt)
	}
}

// logServerData prints one server-side data event:
// "[addr] timestamp | Verb: message (Bytes: n, HEX: ...)".
func logServerData(addr, verb, verbColor, message string, raw []byte) {
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	hexData := fmt.Sprintf("%x", raw)
	if colorEnabled {
		fmt.Printf("%s[%s]%s %s%s%s | %s%s:%s %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
			colorBlue, addr, colorReset,
			colorYellow, timestamp, colorReset,
			verbColor, verb, colorReset, message,
			colorCyan, len(raw), colorReset,
			colorPurple, hexData, colorReset)
	} else {
		fmt.Printf("[%s] %s | %s: %s (Bytes: %d, HEX: %s)\n",
			addr, timestamp, verb, message, len(raw), hexData)
	}
}

// logClientDataTo prints one client-side data event:
// "[Tag] timestamp | message (Bytes: n, HEX: ...)".
func logClientDataTo(w io.Writer, tag, tagColor, message string, raw []byte) {
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	hexData := fmt.Sprintf("%x", raw)
	if colorEnabled {
		fmt.Fprintf(w, "%s[%s]%s %s%s%s | %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
			tagColor, tag, colorReset,
			colorYellow, timestamp, colorReset,
			message,
			colorCyan, len(raw), colorReset,
			colorPurple, hexData, colorReset)
	} else {
		fmt.Fprintf(w, "[%s] %s | %s (Bytes: %d, HEX: %s)\n",
			tag, timestamp, message, len(raw), hexData)
	}
}

func logClientData(tag, tagColor, message string, raw []byte) {
	logClientDataTo(os.Stdout, tag, tagColor, message, raw)
}

// appendAndFlushBySuffix frames messages by recvTerm. When recvTerm is CR only, an LF immediately
// after a completed frame is dropped so peer CRLF does not appear as a second (empty) message.
func appendAndFlushBySuffix(messageBuffer *bytes.Buffer, data []byte, recvTerm []byte, pendingSkipLF *bool, onMessage func(msg string)) {
	swallowLFAfterCR := len(recvTerm) == 1 && recvTerm[0] == 0x0D
	for _, b := range data {
		if *pendingSkipLF {
			*pendingSkipLF = false
			if b == 0x0A {
				continue
			}
		}
		messageBuffer.WriteByte(b)
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
}

func discardOrphanLFAfterCR(messageBuffer *bytes.Buffer, recvTerm []byte) bool {
	if len(recvTerm) == 1 && recvTerm[0] == 0x0D &&
		messageBuffer.Len() == 1 && messageBuffer.Bytes()[0] == 0x0A {
		messageBuffer.Reset()
		return true
	}
	return false
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
		return 0
	}

	mode := os.Args[1]

	switch mode {
	case "-s", "--server":
		runServer()
		return 0
	case "-c", "--client":
		return runClientWithExitCode()
	case "-h", "--help", "help":
		fullUsage()
		return 0
	default:
		fmt.Println("Error: Mode must be '-s'/'--server' or '-c'/'--client'")
		shortUsage()
		return 0
	}
}

func showLogo() {
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
		fmt.Println(line)
	}
}

func shortUsage() {
	showLogo()
	fmt.Println("")
	fmt.Println("USAGE")
	fmt.Println("  Server mode:  coe -s <port> [terminator] [options]")
	fmt.Println("  Client mode:  coe -c <IP> <port> [terminator] [options]")
	fmt.Println("")
	fmt.Println("Use 'coe --help' for detailed options and examples.")
}

func fullUsage() {
	showLogo()
	fmt.Println("")
	fmt.Println("USAGE")
	fmt.Println("  Server mode:   coe -s, --server <port> [terminator] [-tt T] [-rt T] [--tx-term T] [--rx-term T]")
	fmt.Println("                 [--no-echo] [--buffer-size <size>] [--flush-timeout <duration>] [--color] [--no-color]")
	fmt.Println("  Client mode:   coe -c, --client <IP> <port> [terminator] [-tt T] [-rt T] [--tx-term T] [--rx-term T]")
	fmt.Println("                 [-m, --message <message>] [--wait <duration>] [--quiet]")
	fmt.Println("                 [--buffer-size <size>] [--flush-timeout <duration>] [--color] [--no-color]")
	fmt.Println("")
	fmt.Println("OPTIONS")
	fmt.Println("Terminator: LF, CR, or CRLF (CR+LF). Positional sets both sides unless overridden; omit when -tt and -rt cover both.")
	fmt.Println("-tt T, --tx-term T   Outgoing delimiter (#send/#broadcast, echo, client send); --send-terminator is an alias")
	fmt.Println("-rt T, --rx-term T   Frame incoming data until this sequence; --recv-terminator is an alias")
	fmt.Println("--no-echo        Disable echo back (Server mode only)")
	fmt.Println("--buffer-size    Specify buffer size (bytes) - Default is 1024")
	fmt.Println("--flush-timeout  Show incomplete buffered data after inactivity (e.g. 100ms); Default is off")
	fmt.Println("-m, --message    Send one message and exit without the interactive prompt")
	fmt.Println("--wait           After sending, receive frames for this duration; default is off")
	fmt.Println("--quiet          In one-shot mode, write response payloads to stdout and logs to stderr")
	fmt.Println("--color          Enable colored output for better readability (Default: enabled)")
	fmt.Println("--no-color       Disable colored output")
	fmt.Println("")
	fmt.Println("COLOR CODING (when --color is enabled)")
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
	fmt.Println("")
	fmt.Println("EXAMPLES")
	fmt.Println("  coe -s 8080")
	fmt.Println("  coe -s 8080 CR")
	fmt.Println("  coe -s 8080 LF --no-echo")
	fmt.Println("  coe -s 8080 --buffer-size 2048")
	fmt.Println("  coe -s 8080 --flush-timeout 100ms")
	fmt.Println("  coe -s 8080 --color")
	fmt.Println("  coe -s 8080 --no-color")
	fmt.Println("  coe -c 127.0.0.1 8080 LF")
	fmt.Println("  coe -c 127.0.0.1 8080 LF --flush-timeout 100ms")
	fmt.Println("  coe -c 127.0.0.1 8080 -tt CR -rt CRLF")
	fmt.Println("  coe -c 127.0.0.1 8080 -m \"STATUS\\r\" --wait 1s --quiet")
	fmt.Println("  coe -c 127.0.0.1 8080 CR -rt CRLF")
	fmt.Println("  coe --client 192.168.1.100 8080 CR --buffer-size 512 --color")
	fmt.Println("  coe --client 192.168.1.100 8080 CR --no-color")
}

func runServer() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: -s, --server <port> [terminator] [-tt T] [-rt T] [--tx-term T] [--rx-term T] [--no-echo] [--buffer-size <size>] [--flush-timeout <duration>] [--color] [--no-color]")
		return
	}

	port := os.Args[2]
	var legacyTerminator string
	var sendTerminatorName, recvTerminatorName string
	echoEnabled := true // Default echo enabled
	bufferSize := 1024  // Default buffer size
	flushTimeout := time.Duration(0)
	colorEnabled = true // Default color enabled
	terminalControlEnabled = stdinIsTerminal()

	// Parse arguments
	for i := 3; i < len(os.Args); i++ {
		arg := os.Args[i]
		if arg == "--no-echo" {
			echoEnabled = false
		} else if arg == "--buffer-size" {
			if i+1 >= len(os.Args) {
				fmt.Println("Error: Buffer size must be specified after --buffer-size")
				return
			}
			var err error
			bufferSize, err = parseBufferSize(os.Args[i+1])
			if err != nil {
				fmt.Println("Error:", err)
				return
			}
			i++
		} else if arg == "--flush-timeout" {
			if i+1 >= len(os.Args) {
				fmt.Println("Error: Flush timeout must be specified after --flush-timeout")
				return
			}
			var err error
			flushTimeout, err = parseFlushTimeout(os.Args[i+1])
			if err != nil {
				fmt.Println("Error:", err)
				return
			}
			i++
		} else if arg == "-tt" || arg == "--tx-term" || arg == "--send-terminator" {
			if i+1 >= len(os.Args) {
				fmt.Println("Error: Value required after -tt / --tx-term (LF, CR, or CRLF)")
				return
			}
			sendTerminatorName = os.Args[i+1]
			i++
		} else if arg == "-rt" || arg == "--rx-term" || arg == "--recv-terminator" {
			if i+1 >= len(os.Args) {
				fmt.Println("Error: Value required after -rt / --rx-term (LF, CR, or CRLF)")
				return
			}
			recvTerminatorName = os.Args[i+1]
			i++
		} else if arg == "--color" {
			colorEnabled = true
		} else if arg == "--no-color" {
			colorEnabled = false
		} else if legacyTerminator == "" && terminatorToken(arg) {
			legacyTerminator = arg
		} else {
			fmt.Printf("Error: Unknown option or terminator %s\n", arg)
			return
		}
	}

	if legacyTerminator != "" {
		if sendTerminatorName == "" {
			sendTerminatorName = legacyTerminator
		}
		if recvTerminatorName == "" {
			recvTerminatorName = legacyTerminator
		}
	}
	if sendTerminatorName == "" {
		sendTerminatorName = "LF"
	}
	if recvTerminatorName == "" {
		recvTerminatorName = "LF"
	}

	sendTerminatorBytes, err := parseTerminator(sendTerminatorName)
	if err != nil {
		fmt.Println("Error (-tt / --tx-term):", err)
		return
	}
	recvTerminatorBytes, err := parseTerminator(recvTerminatorName)
	if err != nil {
		fmt.Println("Error (-rt / --rx-term):", err)
		return
	}

	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Println("Server startup error:", err)
		return
	}
	defer listener.Close()

	fmt.Printf("Server started on port: %s\n", port)
	fmt.Printf("Send terminator:    %s (%s)\n", strings.ToUpper(sendTerminatorName), terminatorHexDescription(sendTerminatorBytes))
	fmt.Printf("Receive terminator: %s (%s)\n", strings.ToUpper(recvTerminatorName), terminatorHexDescription(recvTerminatorBytes))
	fmt.Printf("Buffer size: %d bytes\n", bufferSize)
	if flushTimeout > 0 {
		fmt.Printf("Flush timeout: %s\n", flushTimeout)
	} else {
		fmt.Println("Flush timeout: Off")
	}
	if echoEnabled {
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

	// Manage connected clients
	var clients sync.Map
	var shuttingDown atomic.Bool

	closeClients := func() {
		clients.Range(func(key, value interface{}) bool {
			conn := value.(net.Conn)
			conn.Close()
			fmt.Printf("Disconnected client: %s\n", key)
			return true
		})
	}

	// Handle Ctrl-C (SIGINT) signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nShutting down server...")
		shuttingDown.Store(true)
		closeClients()
		listener.Close()
		os.Exit(0)
	}()

	// Client connection handling
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if shuttingDown.Load() {
					return
				}
				fmt.Println("Connection error:", err)
				continue
			}

			clientAddr := conn.RemoteAddr().String()
			fmt.Printf("Client connected: %s\n", clientAddr)

			// Add to client list
			clients.Store(clientAddr, conn)

			// Handle each client in separate goroutine
			go func() {
				handleClient(conn, recvTerminatorBytes, sendTerminatorBytes, echoEnabled, bufferSize, flushTimeout)

				// Remove from client list when disconnected
				clients.Delete(clientAddr)
			}()
		}
	}()

	// Command input handling
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	printPrompt("Command> ")
	for scanner.Scan() {
		command := scanner.Text()
		if command == "" {
			printPrompt("Command> ")
			continue
		}

		parts := strings.Fields(command)
		if len(parts) == 0 {
			printPrompt("Command> ")
			continue
		}

		switch parts[0] {
		case "#send":
			if len(parts) < 3 {
				fmt.Println("Usage: #send <clientAddr> <message>")
			} else {
				clientAddr := parts[1]
				message := strings.Join(parts[2:], " ")
				sendToClient(&clients, clientAddr, message, sendTerminatorBytes)
			}
		case "#broadcast":
			if len(parts) < 2 {
				fmt.Println("Usage: #broadcast <message>")
			} else {
				message := strings.Join(parts[1:], " ")
				broadcastToAll(&clients, message, sendTerminatorBytes)
			}
		case "#list":
			listClients(&clients)
		case "#help":
			if len(parts) > 1 && parts[1] == "program" {
				fullUsage()
			} else {
				printServerHelp()
			}
		case "#quit", "#exit":
			fmt.Println("Shutting down server...")
			shuttingDown.Store(true)
			closeClients()
			listener.Close()
			return
		default:
			fmt.Printf("Unknown command: %s\n", parts[0])
			fmt.Println("Available commands: #send, #broadcast, #list, #help, #quit")
		}

		printPrompt("Command> ")
	}
	if err := scanner.Err(); err != nil {
		fmt.Println("Input error:", err)
	}
}

func handleClient(conn net.Conn, recvTerminatorBytes, sendTerminatorBytes []byte, echoEnabled bool, bufferSize int, flushTimeout time.Duration) {
	defer conn.Close()
	defer fmt.Printf("Client disconnected: %s\n", conn.RemoteAddr().String())

	clientAddr := conn.RemoteAddr().String()

	// Receive with specified buffer size
	buffer := make([]byte, bufferSize)
	var messageBuffer bytes.Buffer
	var pendingSkipLF bool

	// Display incomplete buffered data (on flush timeout or disconnect)
	flushIncomplete := func() {
		if discardOrphanLFAfterCR(&messageBuffer, recvTerminatorBytes) {
			return
		}
		if messageBuffer.Len() == 0 {
			return
		}
		logServerData(clientAddr, "Received", colorGreen, messageBuffer.String(), messageBuffer.Bytes())
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
			// Process received data (frame by recv terminator suffix)
			data := buffer[:n]
			stopClient := false
			appendAndFlushBySuffix(&messageBuffer, data, recvTerminatorBytes, &pendingSkipLF, func(message string) {
				if stopClient {
					return
				}
				fullFrame := message + string(recvTerminatorBytes)
				logServerData(clientAddr, "Received", colorGreen, message, []byte(fullFrame))

				if echoEnabled {
					response := message + string(sendTerminatorBytes)
					_, err := conn.Write([]byte(response))
					if err != nil {
						fmt.Printf("[%s] Send error: %v\n", clientAddr, err)
						stopClient = true
						return
					}
					logServerData(clientAddr, "Sent", colorRed, message, []byte(response))
				}
			})
			if stopClient {
				return
			}
		}

		// Check if it's a timeout error
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			flushIncomplete()
			continue // Continue reading
		}

		if err != nil {
			flushIncomplete()
			fmt.Printf("[%s] Receive error: %v\n", clientAddr, err)
			break
		}
	}
}

func sendToClient(clients *sync.Map, clientAddr string, message string, terminatorBytes []byte) {
	// Process escape sequences in message
	processedMessage := processEscapeSequences(message)

	if conn, ok := clients.Load(clientAddr); ok {
		response := processedMessage + string(terminatorBytes)
		_, err := conn.(net.Conn).Write([]byte(response))
		if err != nil {
			fmt.Printf("Send error [%s]: %v\n", clientAddr, err)
		} else {
			// Display original message (with escape sequences) for readability
			logServerData(clientAddr, "Sent", colorRed, message, []byte(response))
		}
	} else {
		fmt.Printf("Client not found: %s\n", clientAddr)
	}
}

func broadcastToAll(clients *sync.Map, message string, terminatorBytes []byte) {
	// Process escape sequences in message
	processedMessage := processEscapeSequences(message)

	count := 0
	response := processedMessage + string(terminatorBytes)
	responseBytes := []byte(response)

	clients.Range(func(key, value interface{}) bool {
		conn := value.(net.Conn)
		_, err := conn.Write(responseBytes)
		if err != nil {
			fmt.Printf("Send error [%s]: %v\n", key, err)
		} else {
			logServerData(key.(string), "Sent", colorRed, message, responseBytes)
			count++
		}
		return true
	})
	fmt.Printf("Broadcast completed: sent to %d clients\n", count)
}

func listClients(clients *sync.Map) {
	count := 0
	fmt.Println("Connected clients:")
	clients.Range(func(key, value interface{}) bool {
		fmt.Printf("  %s\n", key)
		count++
		return true
	})
	if count == 0 {
		fmt.Println("  No clients connected")
	} else {
		fmt.Printf("Total: %d clients\n", count)
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
	flushTimeout        time.Duration
	message             string
	messageSet          bool
	wait                time.Duration
	waitSet             bool
	quiet               bool
}

func clientUsage() string {
	return "Usage: -c, --client <IP> <port> [terminator] [-tt T] [-rt T] [--tx-term T] [--rx-term T] [-m, --message <message>] [--wait <duration>] [--quiet] [--buffer-size <size>] [--flush-timeout <duration>] [--color] [--no-color]"
}

func parseClientConfig() (clientConfig, error) {
	if len(os.Args) < 4 {
		return clientConfig{}, fmt.Errorf("%s\nTerminator: LF, CR, or CRLF. Positional is optional if -tt and -rt specify send and receive.", clientUsage())
	}

	cfg := clientConfig{
		address:    os.Args[2] + ":" + os.Args[3],
		bufferSize: 1024,
	}
	colorEnabled = true
	terminalControlEnabled = stdinIsTerminal()

	argi := 4
	if argi < len(os.Args) && !strings.HasPrefix(os.Args[argi], "-") {
		if !terminatorToken(os.Args[argi]) {
			return clientConfig{}, fmt.Errorf("Error: Terminator must be LF, CR, or CRLF (or omit and use -tt / -rt)")
		}
		cfg.legacyTerminator = os.Args[argi]
		argi++
	}

	for i := argi; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch arg {
		case "--buffer-size":
			if i+1 >= len(os.Args) {
				return clientConfig{}, fmt.Errorf("Error: Buffer size must be specified after --buffer-size")
			}
			var err error
			cfg.bufferSize, err = parseBufferSize(os.Args[i+1])
			if err != nil {
				return clientConfig{}, fmt.Errorf("Error: %w", err)
			}
			i++
		case "--flush-timeout":
			if i+1 >= len(os.Args) {
				return clientConfig{}, fmt.Errorf("Error: Flush timeout must be specified after --flush-timeout")
			}
			var err error
			cfg.flushTimeout, err = parseFlushTimeout(os.Args[i+1])
			if err != nil {
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
			if i+1 >= len(os.Args) {
				return clientConfig{}, fmt.Errorf("Error: Wait duration must be specified after --wait")
			}
			var err error
			cfg.wait, err = parseWaitDuration(os.Args[i+1])
			if err != nil {
				return clientConfig{}, fmt.Errorf("Error: %w", err)
			}
			cfg.waitSet = true
			i++
		case "--quiet":
			cfg.quiet = true
		case "--color":
			colorEnabled = true
		case "--no-color":
			colorEnabled = false
		default:
			return clientConfig{}, fmt.Errorf("Error: Unknown option %s", arg)
		}
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

	var err error
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

func runClient() {
	_ = runClientWithExitCode()
}

func runClientWithExitCode() int {
	cfg, err := parseClientConfig()
	if err != nil {
		output := io.Writer(os.Stdout)
		for _, arg := range os.Args {
			if arg == "--quiet" {
				output = os.Stderr
				break
			}
		}
		fmt.Fprintln(output, err)
		return 0
	}
	if cfg.messageSet {
		return runOneShotClient(cfg)
	}
	return runInteractiveClient(cfg)
}

func printClientConnectionInfo(w io.Writer, cfg clientConfig) {
	fmt.Fprintf(w, "Connection successful: %s\n", cfg.address)
	fmt.Fprintf(w, "Send terminator:    %s (%s)\n", strings.ToUpper(cfg.sendTerminatorName), terminatorHexDescription(cfg.sendTerminatorBytes))
	fmt.Fprintf(w, "Receive terminator: %s (%s)\n", strings.ToUpper(cfg.recvTerminatorName), terminatorHexDescription(cfg.recvTerminatorBytes))
	fmt.Fprintf(w, "Buffer size: %d bytes\n", cfg.bufferSize)
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

	conn, err := net.Dial("tcp", cfg.address)
	if err != nil {
		fmt.Fprintln(logWriter, "Connection error:", err)
		return 1
	}
	defer conn.Close()

	printClientConnectionInfo(logWriter, cfg)

	processedText := processEscapeSequences(cfg.message)
	frame := []byte(processedText + string(cfg.sendTerminatorBytes))
	n, err := conn.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	if err != nil {
		fmt.Fprintln(logWriter, "Send error:", err)
		return 2
	}
	logClientDataTo(logWriter, "Send", colorCyan, cfg.message, frame)

	if !cfg.waitSet {
		return 0
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
			fmt.Fprintln(logWriter, "Receive error:", err)
			break
		}

		n, err := conn.Read(buffer)
		// Per io.Reader contract, process returned bytes before handling err.
		if n > 0 {
			appendAndFlushBySuffix(&messageBuffer, buffer[:n], cfg.recvTerminatorBytes, &pendingSkipLF, func(message string) {
				received = true
				fullFrame := append([]byte(message), cfg.recvTerminatorBytes...)
				emit(message, fullFrame)
			})
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
			fmt.Fprintln(logWriter, "Receive error:", err)
		}
		flushIncomplete()
		break
	}

	if !received {
		fmt.Fprintf(logWriter, "No response received within %s\n", cfg.wait)
		return 3
	}
	return 0
}

func runInteractiveClient(cfg clientConfig) int {
	conn, err := net.Dial("tcp", cfg.address)
	if err != nil {
		fmt.Println("Connection error:", err)
		return 0
	}
	defer conn.Close()

	printClientConnectionInfo(os.Stdout, cfg)
	fmt.Println("Chat started. Enter messages:")
	fmt.Println("----------------------------------------")

	// Handle Ctrl-C (SIGINT) signal
	var clientClosing atomic.Bool
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nDisconnecting...")
		clientClosing.Store(true)
		conn.Close()
		os.Exit(0)
	}()

	// Mutex for output synchronization
	var outputMutex sync.Mutex

	// Receive-only goroutine
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buffer := make([]byte, cfg.bufferSize)
		var messageBuffer bytes.Buffer
		var pendingSkipLF bool

		// Display incomplete buffered data (on flush timeout or disconnect)
		flushIncomplete := func(withPrompt bool) {
			if discardOrphanLFAfterCR(&messageBuffer, cfg.recvTerminatorBytes) {
				return
			}
			if messageBuffer.Len() == 0 {
				return
			}
			outputMutex.Lock()
			clearInputLine()
			logClientData("Recv", colorGreen, messageBuffer.String(), messageBuffer.Bytes())
			if withPrompt {
				printPrompt("Send> ")
			}
			outputMutex.Unlock()
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
				// Process received data (frame by recv terminator suffix)
				data := buffer[:n]
				appendAndFlushBySuffix(&messageBuffer, data, cfg.recvTerminatorBytes, &pendingSkipLF, func(message string) {
					fullFrame := message + string(cfg.recvTerminatorBytes)
					outputMutex.Lock()
					clearInputLine()
					logClientData("Recv", colorGreen, message, []byte(fullFrame))
					printPrompt("Send> ")
					outputMutex.Unlock()
				})
			}

			// Check if it's a timeout error
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				flushIncomplete(true)
				continue // Continue reading
			}

			if err != nil {
				if clientClosing.Load() {
					return
				}
				flushIncomplete(false)
				outputMutex.Lock()
				clearInputLine()
				fmt.Println("Receive error:", err)
				outputMutex.Unlock()
				return
			}
		}
	}()

	// Send processing
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	printPrompt("Send> ")
	for scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			printPrompt("Send> ")
			continue
		}

		// Process escape sequences and send with specified terminator
		processedText := processEscapeSequences(text)
		message := processedText + string(cfg.sendTerminatorBytes)
		_, err := conn.Write([]byte(message))
		if err != nil {
			fmt.Println("Send error:", err)
			break
		}

		outputMutex.Lock()
		logClientData("Send", colorCyan, text, []byte(message))
		printPrompt("Send> ")
		outputMutex.Unlock()
	}
	if err := scanner.Err(); err != nil {
		fmt.Println("Input error:", err)
	}
	clientClosing.Store(true)
	conn.Close()

	wg.Wait()
	return 0
}
