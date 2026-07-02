package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const version = "0.1.6"

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
	if len(os.Args) < 2 {
		shortUsage()
		return
	}

	mode := os.Args[1]

	switch mode {
	case "-s", "--server":
		runServer()
	case "-c", "--client":
		runClient()
	case "-h", "--help", "help":
		fullUsage()
	default:
		fmt.Println("Error: Mode must be '-s'/'--server' or '-c'/'--client'")
		shortUsage()
	}
}

func showLogo() error {
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

	return nil
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
	fmt.Println("                 [--buffer-size <size>] [--flush-timeout <duration>] [--color] [--no-color]")
	fmt.Println("")
	fmt.Println("OPTIONS")
	fmt.Println("Terminator: LF, CR, or CRLF (CR+LF). Positional sets both sides unless overridden; omit when -tt and -rt cover both.")
	fmt.Println("-tt T, --tx-term T   Outgoing delimiter (#send/#broadcast, echo, client send); --send-terminator is an alias")
	fmt.Println("-rt T, --rx-term T   Frame incoming data until this sequence; --recv-terminator is an alias")
	fmt.Println("--no-echo        Disable echo back (Server mode only)")
	fmt.Println("--buffer-size    Specify buffer size (bytes) - Default is 1024")
	fmt.Println("--flush-timeout  Show incomplete buffered data after inactivity (e.g. 100ms); Default is off")
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
			if i+1 < len(os.Args) {
				if size, err := fmt.Sscanf(os.Args[i+1], "%d", &bufferSize); err != nil || size != 1 {
					fmt.Println("Error: Buffer size must be a number")
					return
				}
				if bufferSize <= 0 {
					fmt.Println("Error: Buffer size must be 1 or greater")
					return
				}
				i++ // Skip next argument
			} else {
				fmt.Println("Error: Buffer size must be specified after --buffer-size")
				return
			}
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
	var clientsMutex sync.RWMutex
	var shuttingDown atomic.Bool

	closeClients := func() {
		clientsMutex.Lock()
		defer clientsMutex.Unlock()
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
			clientsMutex.Lock()
			clients.Store(clientAddr, conn)
			clientsMutex.Unlock()

			// Handle each client in separate goroutine
			go func() {
				handleClient(conn, recvTerminatorBytes, sendTerminatorBytes, echoEnabled, &clients, &clientsMutex, bufferSize, flushTimeout)

				// Remove from client list when disconnected
				clientsMutex.Lock()
				clients.Delete(clientAddr)
				clientsMutex.Unlock()
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
				sendToClient(&clients, &clientsMutex, clientAddr, message, sendTerminatorBytes)
			}
		case "#broadcast":
			if len(parts) < 2 {
				fmt.Println("Usage: #broadcast <message>")
			} else {
				message := strings.Join(parts[1:], " ")
				broadcastToAll(&clients, &clientsMutex, message, sendTerminatorBytes)
			}
		case "#list":
			listClients(&clients, &clientsMutex)
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

func handleClient(conn net.Conn, recvTerminatorBytes, sendTerminatorBytes []byte, echoEnabled bool, clients *sync.Map, clientsMutex *sync.RWMutex, bufferSize int, flushTimeout time.Duration) {
	defer conn.Close()
	defer fmt.Printf("Client disconnected: %s\n", conn.RemoteAddr().String())

	// Receive with specified buffer size
	buffer := make([]byte, bufferSize)
	var messageBuffer bytes.Buffer
	var pendingSkipLF bool

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
				timestamp := time.Now().Format("2006-01-02 15:04:05.000")
				fullFrame := message + string(recvTerminatorBytes)
				messageBytes := []byte(fullFrame)
				hexData := fmt.Sprintf("%x", messageBytes)
				if colorEnabled {
					fmt.Printf("%s[%s]%s %s%s%s | %sReceived:%s %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
						colorBlue, conn.RemoteAddr().String(), colorReset,
						colorYellow, timestamp, colorReset,
						colorGreen, colorReset, message,
						colorCyan, len(messageBytes), colorReset,
						colorPurple, hexData, colorReset)
				} else {
					fmt.Printf("[%s] %s | Received: %s (Bytes: %d, HEX: %s)\n",
						conn.RemoteAddr().String(), timestamp, message, len(messageBytes), hexData)
				}

				if echoEnabled {
					response := message + string(sendTerminatorBytes)
					_, err := conn.Write([]byte(response))
					if err != nil {
						fmt.Printf("[%s] Send error: %v\n", conn.RemoteAddr().String(), err)
						stopClient = true
						return
					}
					timestamp := time.Now().Format("2006-01-02 15:04:05.000")
					responseBytes := []byte(response)
					hexData := fmt.Sprintf("%x", responseBytes)
					if colorEnabled {
						fmt.Printf("%s[%s]%s %s%s%s | %sSent:%s %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
							colorBlue, conn.RemoteAddr().String(), colorReset,
							colorYellow, timestamp, colorReset,
							colorRed, colorReset, message,
							colorCyan, len(responseBytes), colorReset,
							colorPurple, hexData, colorReset)
					} else {
						fmt.Printf("[%s] %s | Sent: %s (Bytes: %d, HEX: %s)\n",
							conn.RemoteAddr().String(), timestamp, message, len(responseBytes), hexData)
					}
				}
			})
			if stopClient {
				return
			}
		}

		// Check if it's a timeout error
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			// Timeout occurred - display buffered data if any
			if discardOrphanLFAfterCR(&messageBuffer, recvTerminatorBytes) {
				continue
			}
			message := messageBuffer.String()
			if message != "" {
				timestamp := time.Now().Format("2006-01-02 15:04:05.000")
				messageBytes := []byte(message)
				hexData := fmt.Sprintf("%x", messageBytes)
				if colorEnabled {
					fmt.Printf("%s[%s]%s %s%s%s | %sReceived:%s %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
						colorBlue, conn.RemoteAddr().String(), colorReset,
						colorYellow, timestamp, colorReset,
						colorGreen, colorReset, message,
						colorCyan, len(messageBytes), colorReset,
						colorPurple, hexData, colorReset)
				} else {
					fmt.Printf("[%s] %s | Received: %s (Bytes: %d, HEX: %s)\n",
						conn.RemoteAddr().String(), timestamp, message, len(messageBytes), hexData)
				}
				messageBuffer.Reset()
			}
			continue // Continue reading
		}

		if err != nil {
			// Display any remaining buffered data before breaking
			if !discardOrphanLFAfterCR(&messageBuffer, recvTerminatorBytes) {
				message := messageBuffer.String()
				if message != "" {
					timestamp := time.Now().Format("2006-01-02 15:04:05.000")
					messageBytes := []byte(message)
					hexData := fmt.Sprintf("%x", messageBytes)
					if colorEnabled {
						fmt.Printf("%s[%s]%s %s%s%s | %sReceived:%s %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
							colorBlue, conn.RemoteAddr().String(), colorReset,
							colorYellow, timestamp, colorReset,
							colorGreen, colorReset, message,
							colorCyan, len(messageBytes), colorReset,
							colorPurple, hexData, colorReset)
					} else {
						fmt.Printf("[%s] %s | Received: %s (Bytes: %d, HEX: %s)\n",
							conn.RemoteAddr().String(), timestamp, message, len(messageBytes), hexData)
					}
				}
			}
			fmt.Printf("[%s] Receive error: %v\n", conn.RemoteAddr().String(), err)
			break
		}
	}
}

func sendToClient(clients *sync.Map, clientsMutex *sync.RWMutex, clientAddr string, message string, terminatorBytes []byte) {
	clientsMutex.RLock()
	defer clientsMutex.RUnlock()

	// Process escape sequences in message
	processedMessage := processEscapeSequences(message)

	if conn, ok := clients.Load(clientAddr); ok {
		response := processedMessage + string(terminatorBytes)
		_, err := conn.(net.Conn).Write([]byte(response))
		if err != nil {
			fmt.Printf("Send error [%s]: %v\n", clientAddr, err)
		} else {
			timestamp := time.Now().Format("2006-01-02 15:04:05.000")
			responseBytes := []byte(response)
			hexData := fmt.Sprintf("%x", responseBytes)
			// Display original message (with escape sequences) for readability
			if colorEnabled {
				fmt.Printf("%s[%s]%s %s%s%s | %sSent:%s %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
					colorBlue, clientAddr, colorReset,
					colorYellow, timestamp, colorReset,
					colorRed, colorReset, message,
					colorCyan, len(responseBytes), colorReset,
					colorPurple, hexData, colorReset)
			} else {
				fmt.Printf("[%s] %s | Sent: %s (Bytes: %d, HEX: %s)\n",
					clientAddr, timestamp, message, len(responseBytes), hexData)
			}
		}
	} else {
		fmt.Printf("Client not found: %s\n", clientAddr)
	}
}

func broadcastToAll(clients *sync.Map, clientsMutex *sync.RWMutex, message string, terminatorBytes []byte) {
	clientsMutex.RLock()
	defer clientsMutex.RUnlock()

	// Process escape sequences in message
	processedMessage := processEscapeSequences(message)

	count := 0
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	response := processedMessage + string(terminatorBytes)
	responseBytes := []byte(response)
	hexData := fmt.Sprintf("%x", responseBytes)

	clients.Range(func(key, value interface{}) bool {
		conn := value.(net.Conn)
		_, err := conn.Write([]byte(response))
		if err != nil {
			fmt.Printf("Send error [%s]: %v\n", key, err)
		} else {
			if colorEnabled {
				fmt.Printf("%s[%s]%s %s%s%s | %sSent:%s %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
					colorBlue, key, colorReset,
					colorYellow, timestamp, colorReset,
					colorRed, colorReset, message,
					colorCyan, len(responseBytes), colorReset,
					colorPurple, hexData, colorReset)
			} else {
				fmt.Printf("[%s] %s | Sent: %s (Bytes: %d, HEX: %s)\n",
					key, timestamp, message, len(responseBytes), hexData)
			}
			count++
		}
		return true
	})
	fmt.Printf("Broadcast completed: sent to %d clients\n", count)
}

func listClients(clients *sync.Map, clientsMutex *sync.RWMutex) {
	clientsMutex.RLock()
	defer clientsMutex.RUnlock()

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
				// Handle \xHH format
				if i+3 < len(input) {
					hexStr := input[i+2 : i+4]
					var byteVal byte
					_, err := fmt.Sscanf(hexStr, "%02x", &byteVal)
					if err == nil {
						result.WriteByte(byteVal)
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

func runClient() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: -c, --client <IP> <port> [terminator] [-tt T] [-rt T] [--tx-term T] [--rx-term T] [--buffer-size <size>] [--flush-timeout <duration>] [--color] [--no-color]")
		fmt.Println("Terminator: LF, CR, or CRLF. Positional is optional if -tt and -rt specify send and receive.")
		return
	}

	address := os.Args[2] + ":" + os.Args[3]
	var legacyTerminator string
	var sendTerminatorName, recvTerminatorName string
	bufferSize := 1024 // Default buffer size
	flushTimeout := time.Duration(0)
	colorEnabled = true // Default color enabled
	terminalControlEnabled = stdinIsTerminal()

	argi := 4
	if argi < len(os.Args) && !strings.HasPrefix(os.Args[argi], "-") {
		if !terminatorToken(os.Args[argi]) {
			fmt.Println("Error: Terminator must be LF, CR, or CRLF (or omit and use -tt / -rt)")
			return
		}
		legacyTerminator = os.Args[argi]
		argi++
	}

	for i := argi; i < len(os.Args); i++ {
		arg := os.Args[i]
		if arg == "--buffer-size" {
			if i+1 < len(os.Args) {
				if size, err := fmt.Sscanf(os.Args[i+1], "%d", &bufferSize); err != nil || size != 1 {
					fmt.Println("Error: Buffer size must be a number")
					return
				}
				if bufferSize <= 0 {
					fmt.Println("Error: Buffer size must be 1 or greater")
					return
				}
				i++ // Skip next argument
			} else {
				fmt.Println("Error: Buffer size must be specified after --buffer-size")
				return
			}
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
		} else {
			fmt.Printf("Error: Unknown option %s\n", arg)
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

	conn, err := net.Dial("tcp", address)
	if err != nil {
		fmt.Println("Connection error:", err)
		return
	}
	defer conn.Close()

	fmt.Println("Connection successful:", address)
	fmt.Printf("Send terminator:    %s (%s)\n", strings.ToUpper(sendTerminatorName), terminatorHexDescription(sendTerminatorBytes))
	fmt.Printf("Receive terminator: %s (%s)\n", strings.ToUpper(recvTerminatorName), terminatorHexDescription(recvTerminatorBytes))
	fmt.Printf("Buffer size: %d bytes\n", bufferSize)
	if flushTimeout > 0 {
		fmt.Printf("Flush timeout: %s\n", flushTimeout)
	} else {
		fmt.Println("Flush timeout: Off")
	}
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
		buffer := make([]byte, bufferSize)
		var messageBuffer bytes.Buffer
		var pendingSkipLF bool

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
				appendAndFlushBySuffix(&messageBuffer, data, recvTerminatorBytes, &pendingSkipLF, func(message string) {
					fullFrame := message + string(recvTerminatorBytes)
					messageBytes := []byte(fullFrame)
					outputMutex.Lock()
					clearInputLine()
					timestamp := time.Now().Format("2006-01-02 15:04:05.000")
					hexData := fmt.Sprintf("%x", messageBytes)
					if colorEnabled {
						fmt.Printf("%s[Recv]%s %s%s%s | %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
							colorGreen, colorReset,
							colorYellow, timestamp, colorReset,
							message,
							colorCyan, len(messageBytes), colorReset,
							colorPurple, hexData, colorReset)
					} else {
						fmt.Printf("[Recv] %s | %s (Bytes: %d, HEX: %s)\n",
							timestamp, message, len(messageBytes), hexData)
					}
					printPrompt("Send> ")
					outputMutex.Unlock()
				})
			}

			// Check if it's a timeout error
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Timeout occurred - display buffered data if any
				if discardOrphanLFAfterCR(&messageBuffer, recvTerminatorBytes) {
					continue
				}
				message := messageBuffer.String()
				if message != "" {
					outputMutex.Lock()
					clearInputLine()
					timestamp := time.Now().Format("2006-01-02 15:04:05.000")
					messageBytes := []byte(message)
					hexData := fmt.Sprintf("%x", messageBytes)
					if colorEnabled {
						fmt.Printf("%s[Recv]%s %s%s%s | %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
							colorGreen, colorReset,
							colorYellow, timestamp, colorReset,
							message,
							colorCyan, len(messageBytes), colorReset,
							colorPurple, hexData, colorReset)
					} else {
						fmt.Printf("[Recv] %s | %s (Bytes: %d, HEX: %s)\n",
							timestamp, message, len(messageBytes), hexData)
					}
					printPrompt("Send> ")
					outputMutex.Unlock()
					messageBuffer.Reset()
				}
				continue // Continue reading
			}

			if err != nil {
				if clientClosing.Load() {
					return
				}
				// Display any remaining buffered data before returning
				if !discardOrphanLFAfterCR(&messageBuffer, recvTerminatorBytes) {
					message := messageBuffer.String()
					if message != "" {
						outputMutex.Lock()
						clearInputLine()
						timestamp := time.Now().Format("2006-01-02 15:04:05.000")
						messageBytes := []byte(message)
						hexData := fmt.Sprintf("%x", messageBytes)
						if colorEnabled {
							fmt.Printf("%s[Recv]%s %s%s%s | %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
								colorGreen, colorReset,
								colorYellow, timestamp, colorReset,
								message,
								colorCyan, len(messageBytes), colorReset,
								colorPurple, hexData, colorReset)
						} else {
							fmt.Printf("[Recv] %s | %s (Bytes: %d, HEX: %s)\n",
								timestamp, message, len(messageBytes), hexData)
						}
						outputMutex.Unlock()
					}
				}
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
		message := processedText + string(sendTerminatorBytes)
		_, err := conn.Write([]byte(message))
		if err != nil {
			fmt.Println("Send error:", err)
			break
		}

		outputMutex.Lock()
		timestamp := time.Now().Format("2006-01-02 15:04:05.000")
		messageBytes := []byte(message)
		hexData := fmt.Sprintf("%x", messageBytes)
		if colorEnabled {
			fmt.Printf("%s[Send]%s %s%s%s | %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
				colorCyan, colorReset,
				colorYellow, timestamp, colorReset,
				text,
				colorCyan, len(messageBytes), colorReset,
				colorPurple, hexData, colorReset)
		} else {
			fmt.Printf("[Send] %s | %s (Bytes: %d, HEX: %s)\n",
				timestamp, text, len(messageBytes), hexData)
		}
		printPrompt("Send> ")
		outputMutex.Unlock()
	}
	if err := scanner.Err(); err != nil {
		fmt.Println("Input error:", err)
	}
	clientClosing.Store(true)
	conn.Close()

	wg.Wait()
}
