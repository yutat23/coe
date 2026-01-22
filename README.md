# coe

```
 ██████╗ ██████╗ ███████╗
██╔════╝██╔═══██╗██╔═══█║
██║     ██║   ██║███████║
██║     ██║   ██║██╔════╝
╚██████╗╚██████╔╝███████╗
 ╚═════╝ ╚═════╝ ╚══════╝
```

A simple TCP socket communication tool written in Go for basic server-client messaging with configurable terminators and optional echo functionality.

## Features

- **Server Mode**: Multi-client TCP server with interactive command interface
- **Client Mode**: TCP client for connecting to servers
- **Configurable Terminators**: Support for LF (0x0A) and CR (0x0D) terminators
- **Echo Functionality**: Optional echo-back feature for server responses
- **Colored Output**: Enhanced readability with color-coded messages and data
- **Interactive Commands**: Server-side commands for client management
- **Real-time Monitoring**: Live display of sent/received messages with timestamps
- **Hexadecimal Data Display**: Raw data inspection with hex representation
- **Buffer Size Configuration**: Customizable buffer sizes for different use cases

## Installation

### From Source

```bash
git clone <repository-url>
cd tcil
go build -o coe coe.go
```

### Windows

```bash
go build -o coe.exe coe.go
```

## Usage

```
coe <mode> [options]
```

### Modes

- `-s`, `--server`: Run in server mode
- `-c`, `--client`: Run in client mode
- `-h`, `--help`, `help`: Show help message

## Server Mode

Start a TCP server that can handle multiple client connections:

```bash
coe -s <port> [terminator] [options]
```

### Server Options

- `<port>`: Port number to listen on (required)
- `[terminator]`: Message terminator - LF (0x0A) or CR (0x0D) - Default: LF
- `--no-echo`: Disable echo-back functionality
- `--buffer-size <size>`: Specify buffer size in bytes - Default: 1024
- `--color`: Enable colored output

### Server Commands

Once the server is running, you can use interactive commands:

- `#send <clientIP> <message>`: Send a message to a specific client
- `#broadcast <message>`: Send a message to all connected clients
- `#list`: Show all connected clients
- `#help`: Show server command help
- `#quit`, `#exit`: Shut down the server

### Server Examples

```bash
# Start server on port 8080 with default settings
coe -s 8080

# Start server with CR terminator
coe -s 8080 CR

# Start server with disabled echo
coe -s 8080 LF --no-echo

# Start server with custom buffer size
coe -s 8080 --buffer-size 2048

# Start server with colored output
coe -s 8080 --color

# Combine multiple options
coe -s 8080 CR --no-echo --buffer-size 512 --color
```

## Client Mode

Connect to a TCP server as a client:

```bash
coe -c <IP> <port> <terminator> [options]
```

### Client Options

- `<IP>`: Server IP address (required)
- `<port>`: Server port number (required)
- `<terminator>`: Message terminator - LF (0x0A) or CR (0x0D) (required)
- `--buffer-size <size>`: Specify buffer size in bytes - Default: 1024
- `--color`: Enable colored output

### Client Examples

```bash
# Connect to localhost server
coe -c 127.0.0.1 8080 LF

# Connect to remote server with CR terminator
coe -c 192.168.1.100 8080 CR

# Connect with custom buffer size
coe -c 127.0.0.1 8080 LF --buffer-size 512

# Connect with colored output
coe -c 127.0.0.1 8080 LF --color

# Combine options
coe -c 192.168.1.100 8080 CR --buffer-size 2048 --color
```

## Color Coding

When `--color` is enabled, the output uses the following color scheme:

- **Blue**: Client IP addresses
- **Green**: Received messages
- **Red**: Sent messages
- **Yellow**: Timestamps
- **Cyan**: Byte counts
- **Purple**: Hexadecimal data

## Escape Sequences

Messages support escape sequences: `\r` (CR), `\n` (LF), `\t` (TAB), `\\` (backslash), `\xHH` (hex byte).

Example: `Hello\r\nWorld` sends "Hello" + CR + LF + "World"

## How It Works

### Server Mode
- Listens on the specified port for incoming TCP connections
- Handles multiple clients concurrently using goroutines
- Processes messages based on the configured terminator (LF or CR)
- Provides interactive command interface for client management
- Supports optional echo-back functionality
- Displays real-time message logs with timestamps and metadata

### Client Mode
- Connects to a TCP server at the specified IP and port
- Sends messages with the configured terminator
- Receives and displays messages from the server
- Shows detailed message information including byte counts and hex data
- Runs send and receive operations concurrently

### Message Processing
- Messages are buffered until the terminator character is received
- Supports both LF (Line Feed, 0x0A) and CR (Carriage Return, 0x0D) terminators
- Displays message metadata including timestamps, byte counts, and hexadecimal representation
- Configurable buffer sizes for different network conditions

## Use Cases

- **Network Protocol Testing**: Test custom protocols with different terminators
- **Debugging Network Applications**: Monitor message flow with detailed logging
- **IoT Device Communication**: Communicate with devices using specific terminators
- **Load Testing**: Multiple clients can connect to test server performance
- **Educational Purposes**: Learn about TCP socket programming and message framing

## Requirements

- Go 1.24.4 or later
- Network connectivity for client-server communication

## Dependencies

This application uses only Go standard library packages:
- `net`: TCP socket communication
- `bufio`: Buffered I/O operations
- `fmt`: Formatted I/O
- `os`: Operating system interface
- `strings`: String manipulation
- `sync`: Synchronization primitives
- `time`: Time operations