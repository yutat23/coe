# coe

```
 ██████╗ ██████╗ ███████╗
██╔════╝██╔═══██╗██╔════╝
██║     ██║   ██║█████╗
██║     ██║   ██║██╔══╝
╚██████╗╚██████╔╝███████╗
 ╚═════╝ ╚═════╝ ╚══════╝
```

coe is a small TCP socket communication tool for interactive server/client testing with configurable send and receive terminators.

## Features

- Server mode with multiple concurrent clients
- Client mode for connecting to TCP servers
- LF, CR, and CRLF terminators
- Separate send and receive terminators with `-tt` and `-rt`
- Optional server echo-back
- Hex and byte-count logging
- Escape sequences in sent messages: `\r`, `\n`, `\t`, `\\`, `\xHH`
- Optional incomplete-frame flushing with `--flush-timeout`
- One-shot client sends with `-m` / `--message`
- Time-based response waiting with `--wait` and script-friendly output with `--quiet`

## Installation

```bash
git clone <repository-url>
cd coe
go build -o coe .
```

Windows:

```bash
go build -o coe.exe .
```

## Usage

```text
coe -s <port> [terminator] [options]
coe -c <IP> <port> [terminator] [options]
```

`terminator` can be `LF`, `CR`, or `CRLF`. If omitted, both send and receive default to `LF` unless overridden.

## Options

- `-tt T`, `--tx-term T`, `--send-terminator T`: outgoing terminator
- `-rt T`, `--rx-term T`, `--recv-terminator T`: incoming frame terminator
- `--buffer-size <size>`: receive buffer size in bytes, default `1024`
- `--flush-timeout <duration>`: display incomplete buffered data after inactivity, for example `100ms`; default off
- `-m <message>`, `--message <message>`: send one message and exit without starting the interactive prompt
- `--wait <duration>`: after a one-shot send, receive frames for this duration; omitted means fire-and-forget
- `--quiet`: in one-shot mode, write response payloads (one line per frame) to stdout and connection/log output to stderr
- `--color`: enable color output, default on
- `--no-color`: disable color output
- `--no-echo`: disable server echo-back, server mode only

## Server Commands

- `#send <clientAddr> <message>`: send to one client. Use the `IP:port` value shown by `#list`.
- `#broadcast <message>`: send to all clients
- `#list`: show connected clients
- `#help`: show server command help
- `#help program`: show full program help
- `#quit`, `#exit`: shut down the server

## Examples

```bash
coe -s 8080
coe -s 8080 CR --no-echo
coe -s 8080 -tt CR -rt CRLF
coe -s 8080 --flush-timeout 100ms

coe -c 127.0.0.1 8080
coe -c 127.0.0.1 8080 LF
coe -c 127.0.0.1 8080 -tt CR -rt CRLF
coe -c 192.168.1.100 8080 CR --buffer-size 512 --no-color
coe -c 127.0.0.1 8080 -m "STATUS\\r" --wait 1s --quiet
```

When `--wait` is specified, exit status `3` means that no response frame arrived before the wait period ended. A successful connection and send returns `0`; connection failures return `1`, and send failures return `2`.

## Message Processing

By default, received data is buffered until the configured receive terminator is found. This avoids splitting TCP data just because packets arrive in multiple chunks.

For devices or protocols that do not send a terminator, use `--flush-timeout 100ms` or another duration. That mode displays incomplete buffered data after the connection has been idle for the requested time.

Sent messages are processed for escape sequences before the configured send terminator is appended. For example, `Hello\r\nWorld` sends `Hello` followed by CRLF and `World`.

## Requirements

- Go 1.24.4 or later
- Network connectivity for client/server communication

## Dependencies

coe uses only the Go standard library.
