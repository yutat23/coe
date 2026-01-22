package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "0.1.1"

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

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}

	mode := os.Args[1]

	switch mode {
	case "-s", "--server":
		runServer()
	case "-c", "--client":
		runClient()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Println("Error: Mode must be '-s'/'--server' or '-c'/'--client'")
		usage()
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

func usage() {
	showLogo()
	fmt.Println("")
	fmt.Println("USAGE")
	fmt.Println("  Server mode:   coe -s, --server <port> [terminator] [--no-echo] [--buffer-size <size>] [--color] [--no-color]")
	fmt.Println("  Client mode    coe -c, --client <IP> <port> <terminator> [--buffer-size <size>] [--color] [--no-color]")
	fmt.Println("")
	fmt.Println("OPTIONS")
	fmt.Println("Terminator: LF (0A) or CR (0D) - Default is LF")
	fmt.Println("--no-echo        Disable echo back (Server mode only)")
	fmt.Println("--buffer-size    Specify buffer size (bytes) - Default is 1024")
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
	fmt.Println("  coe -s 8080 --color")
	fmt.Println("  coe -s 8080 --no-color")
	fmt.Println("  coe -c 127.0.0.1 8080 LF")
	fmt.Println("  coe --client 192.168.1.100 8080 CR --buffer-size 512 --color")
	fmt.Println("  coe --client 192.168.1.100 8080 CR --no-color")
}

func runServer() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: -s, --server <port> [terminator] [--no-echo] [--buffer-size <size>] [--color] [--no-color]")
		return
	}

	port := os.Args[2]
	terminator := "LF"  // Default
	echoEnabled := true // Default echo enabled
	bufferSize := 1024  // Default buffer size
	colorEnabled = true // Default color enabled

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
		} else if arg == "--color" {
			colorEnabled = true
		} else if arg == "--no-color" {
			colorEnabled = false
		} else if terminator == "LF" && (arg == "LF" || arg == "CR") {
			terminator = arg
		}
	}

	// Set terminator
	var terminatorBytes []byte
	switch strings.ToUpper(terminator) {
	case "LF":
		terminatorBytes = []byte{0x0A} // LF
	case "CR":
		terminatorBytes = []byte{0x0D} // CR
	default:
		fmt.Println("Error: Terminator must be 'LF' or 'CR'")
		return
	}

	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Println("Server startup error:", err)
		return
	}
	defer listener.Close()

	fmt.Printf("Server started on port: %s\n", port)
	fmt.Printf("Terminator: %s (0x%02X)\n", terminator, terminatorBytes[0])
	fmt.Printf("Buffer size: %d bytes\n", bufferSize)
	if echoEnabled {
		fmt.Println("Echo back: Enabled")
	} else {
		fmt.Println("Echo back: Disabled")
	}
	fmt.Println("Waiting for client connections...")
	fmt.Println("Commands: '#send <clientIP> <message>' to send to specific client")
	fmt.Println("Commands: '#broadcast <message>' to send to all clients")
	fmt.Println("Commands: '#list' to show connected clients")
	fmt.Println("Commands: '#help' to show available commands")
	fmt.Println("Commands: '#quit, #exit: Shut down the server")
	fmt.Println("----------------------------------------")

	// Manage connected clients
	var clients sync.Map
	var clientsMutex sync.RWMutex

	// Handle Ctrl-C (SIGINT) signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nShutting down server...")
		// Close all client connections
		clientsMutex.Lock()
		clients.Range(func(key, value interface{}) bool {
			conn := value.(net.Conn)
			conn.Close()
			fmt.Printf("Disconnected client: %s\n", key)
			return true
		})
		clientsMutex.Unlock()
		listener.Close()
		os.Exit(0)
	}()

	// Client connection handling
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
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
				handleClient(conn, terminatorBytes, echoEnabled, &clients, &clientsMutex, bufferSize)

				// Remove from client list when disconnected
				clientsMutex.Lock()
				clients.Delete(clientAddr)
				clientsMutex.Unlock()
			}()
		}
	}()

	// Command input handling
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("Command> ")
	for scanner.Scan() {
		command := scanner.Text()
		if command == "" {
			fmt.Print("Command> ")
			continue
		}

		parts := strings.Fields(command)
		if len(parts) == 0 {
			fmt.Print("Command> ")
			continue
		}

		switch parts[0] {
		case "#send":
			if len(parts) < 3 {
				fmt.Println("Usage: send <clientIP> <message>")
			} else {
				clientIP := parts[1]
				message := strings.Join(parts[2:], " ")
				sendToClient(&clients, &clientsMutex, clientIP, message, terminatorBytes)
			}
		case "#broadcast":
			if len(parts) < 2 {
				fmt.Println("Usage: broadcast <message>")
			} else {
				message := strings.Join(parts[1:], " ")
				broadcastToAll(&clients, &clientsMutex, message, terminatorBytes)
			}
		case "#list":
			liscoeents(&clients, &clientsMutex)
		case "#help":
			if len(parts) > 1 && parts[1] == "program" {
				usage()
			} else {
				printServerHelp()
			}
		case "#quit", "#exit":
			fmt.Println("Shutting down server...")
			return
		default:
			fmt.Printf("Unknown command: %s\n", parts[0])
			fmt.Println("Available commands: send, broadcast, list, help, quit")
		}

		fmt.Print("Command> ")
	}
}

func handleClient(conn net.Conn, terminatorBytes []byte, echoEnabled bool, clients *sync.Map, clientsMutex *sync.RWMutex, bufferSize int) {
	defer conn.Close()
	defer fmt.Printf("Client disconnected: %s\n", conn.RemoteAddr().String())

	// Receive with specified buffer size
	buffer := make([]byte, bufferSize)
	var messageBuffer strings.Builder
	const timeoutDuration = 100 * time.Millisecond // Timeout for incomplete messages

	for {
		// Set read deadline to detect when data stops coming
		conn.SetReadDeadline(time.Now().Add(timeoutDuration))
		n, err := conn.Read(buffer)

		// Check if it's a timeout error
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			// Timeout occurred - display buffered data if any
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
			fmt.Printf("[%s] Receive error: %v\n", conn.RemoteAddr().String(), err)
			break
		}

		if n == 0 {
			// Display any remaining buffered data when connection is closed gracefully
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
			continue
		}

		// Debug: Show received data details
		// fmt.Printf("[%s] Debug: Received bytes=%d, data=%x\n", conn.RemoteAddr().String(), n, buffer[:n])

		// Process received data
		data := buffer[:n]
		for _, b := range data {
			if b == terminatorBytes[0] {
				// Display message when terminator is found
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

					// Echo back functionality (optional)
					if echoEnabled {
						response := message + string(terminatorBytes)
						_, err := conn.Write([]byte(response))
						if err != nil {
							fmt.Printf("[%s] Send error: %v\n", conn.RemoteAddr().String(), err)
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
				}
				messageBuffer.Reset()
			} else {
				messageBuffer.WriteByte(b)
			}
		}
	}
}

func sendToClient(clients *sync.Map, clientsMutex *sync.RWMutex, clientIP string, message string, terminatorBytes []byte) {
	clientsMutex.RLock()
	defer clientsMutex.RUnlock()

	// Process escape sequences in message
	processedMessage := processEscapeSequences(message)

	if conn, ok := clients.Load(clientIP); ok {
		response := processedMessage + string(terminatorBytes)
		_, err := conn.(net.Conn).Write([]byte(response))
		if err != nil {
			fmt.Printf("Send error [%s]: %v\n", clientIP, err)
		} else {
			timestamp := time.Now().Format("2006-01-02 15:04:05.000")
			responseBytes := []byte(response)
			hexData := fmt.Sprintf("%x", responseBytes)
			// Display original message (with escape sequences) for readability
			if colorEnabled {
				fmt.Printf("%s[%s]%s %s%s%s | %sSent:%s %s (Bytes: %s%d%s, HEX: %s%s%s)\n",
					colorBlue, clientIP, colorReset,
					colorYellow, timestamp, colorReset,
					colorRed, colorReset, message,
					colorCyan, len(responseBytes), colorReset,
					colorPurple, hexData, colorReset)
			} else {
				fmt.Printf("[%s] %s | Sent: %s (Bytes: %d, HEX: %s)\n",
					clientIP, timestamp, message, len(responseBytes), hexData)
			}
		}
	} else {
		fmt.Printf("Client not found: %s\n", clientIP)
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

func liscoeents(clients *sync.Map, clientsMutex *sync.RWMutex) {
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
	fmt.Println("  #send <clientIP> <message>: Send a message to a specific client")
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
	fmt.Println("Program help: Type 'help program' for full program usage")
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
	if len(os.Args) < 5 {
		fmt.Println("Usage: -c, --client <IP> <port> <terminator> [--buffer-size <size>] [--color] [--no-color]")
		fmt.Println("Terminator: LF (0A) or CR (0D)")
		return
	}

	address := os.Args[2] + ":" + os.Args[3]
	terminator := os.Args[4]
	bufferSize := 1024  // Default buffer size
	colorEnabled = true // Default color enabled

	// Parse arguments
	for i := 5; i < len(os.Args); i++ {
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
		} else if arg == "--color" {
			colorEnabled = true
		} else if arg == "--no-color" {
			colorEnabled = false
		}
	}

	// Set terminator
	var terminatorBytes []byte
	switch strings.ToUpper(terminator) {
	case "LF":
		terminatorBytes = []byte{0x0A} // LF
	case "CR":
		terminatorBytes = []byte{0x0D} // CR
	default:
		fmt.Println("Error: Terminator must be 'LF' or 'CR'")
		return
	}

	conn, err := net.Dial("tcp", address)
	if err != nil {
		fmt.Println("Connection error:", err)
		return
	}
	defer conn.Close()

	fmt.Println("Connection successful:", address)
	fmt.Printf("Terminator: %s (0x%02X)\n", terminator, terminatorBytes[0])
	fmt.Printf("Buffer size: %d bytes\n", bufferSize)
	fmt.Println("Chat started. Enter messages:")
	fmt.Println("----------------------------------------")

	// Handle Ctrl-C (SIGINT) signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nDisconnecting...")
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
		var messageBuffer strings.Builder
		const timeoutDuration = 100 * time.Millisecond // Timeout for incomplete messages

		for {
			// Set read deadline to detect when data stops coming
			conn.SetReadDeadline(time.Now().Add(timeoutDuration))
			n, err := conn.Read(buffer)

			// Check if it's a timeout error
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Timeout occurred - display buffered data if any
				message := messageBuffer.String()
				if message != "" {
					outputMutex.Lock()
					fmt.Print("\r\033[K") // Clear current line
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
					fmt.Print("Send> ") // Re-display prompt
					outputMutex.Unlock()
					messageBuffer.Reset()
				}
				continue // Continue reading
			}

			if err != nil {
				// Display any remaining buffered data before returning
				message := messageBuffer.String()
				if message != "" {
					outputMutex.Lock()
					fmt.Print("\r\033[K") // Clear current line
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
				outputMutex.Lock()
				fmt.Print("\r\033[K") // Clear current line before error message
				fmt.Println("Receive error:", err)
				outputMutex.Unlock()
				return
			}

			if n == 0 {
				// Display any remaining buffered data when connection is closed gracefully
				message := messageBuffer.String()
				if message != "" {
					outputMutex.Lock()
					fmt.Print("\r\033[K") // Clear current line
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
					fmt.Print("Send> ") // Re-display prompt
					outputMutex.Unlock()
					messageBuffer.Reset()
				}
				continue
			}

			// Process received data - wait for terminator like server side
			data := buffer[:n]
			for _, b := range data {
				if b == terminatorBytes[0] {
					// Display message when terminator is found
					message := messageBuffer.String()
					if message != "" {
						outputMutex.Lock()
						fmt.Print("\r\033[K") // Clear current line
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
						fmt.Print("Send> ") // Re-display prompt
						outputMutex.Unlock()
					}
					messageBuffer.Reset()
				} else {
					messageBuffer.WriteByte(b)
				}
			}
		}
	}()

	// Send processing
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("Send> ")
	for scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			fmt.Print("Send> ")
			continue
		}

		// Process escape sequences and send with specified terminator
		processedText := processEscapeSequences(text)
		message := processedText + string(terminatorBytes)
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
		fmt.Print("Send> ")
		outputMutex.Unlock()
	}

	wg.Wait()
}
