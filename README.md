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

The name comes from *koe* (声), Japanese for "voice" — the tool is there to let two endpoints talk to each other.

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
- Receive frame size limit (`--max-frame-size`) and server connection limit (`--max-clients`)
- Per-write socket deadline (`--write-timeout`) so a stalled peer cannot freeze the server

## Installation

```bash
git clone https://github.com/yutat23/coe.git
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
- `--buffer-size <size>`: `Read` chunk size in bytes, default `1024`. This is not a frame size limit.
- `--max-frame-size <size>`: maximum assembled receive frame in bytes, including the terminator; `0` is unlimited. Default `1048576`. Connections that exceed it are closed.
- `--max-clients <n>`: maximum concurrent server clients; `0` is unlimited. Default `64`. Extra connections are rejected.
- `--write-timeout <duration>`: deadline for each socket write, for example `5s`; `0`/`off` disables it. Default `5s`. Timed-out clients are dropped.
- `--flush-timeout <duration>`: display incomplete buffered data after inactivity, for example `100ms`; default off
- `-m <message>`, `--message <message>`: send one message and exit without starting the interactive prompt
- `--wait <duration>`: after a one-shot send, receive frames for this duration; omitted means fire-and-forget
- `--quiet`: in one-shot mode, write response payloads (one line per frame) to stdout and connection/log output to stderr
- `--color`: enable color output, default on
- `--no-color`: disable color output
- `--no-echo`: disable server echo-back, server mode only

## Server Commands

- `#send <clientAddr> <message>`: send to one client. Use the `IP:port` value shown by `#list`. The message keeps repeated and trailing spaces.
- `#broadcast <message>`: send to all clients. The message keeps repeated and trailing spaces.
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
coe -s 8080 --max-frame-size 4096 --max-clients 8 --write-timeout 2s

coe -c 127.0.0.1 8080
coe -c ::1 8080
coe -c 127.0.0.1 8080 LF
coe -c 127.0.0.1 8080 -tt CR -rt CRLF
coe -c 192.168.1.100 8080 CR --buffer-size 512 --no-color
coe -c 127.0.0.1 8080 -m "STATUS\\r" --wait 1s --quiet
```

Exit status:

- `0`: success, including `--help` and no-argument usage
- `1`: connection or bind/listen failure, receive-side I/O error (including oversized frame), or interactive send/input failure
- `2`: usage/argument error; in one-shot mode, also a send failure
- `3`: `--wait` elapsed with no response frame. Unexpected receive I/O errors during `--wait` return `1`, even if some frames already arrived. A clean EOF after receiving frames is still `0`.

## Message Processing

By default, received data is buffered until the configured receive terminator is found. This avoids splitting TCP data just because packets arrive in multiple chunks. `--buffer-size` only controls how much is read from the socket at once; `--max-frame-size` caps the assembled frame and closes the connection if it is exceeded.

For devices or protocols that do not send a terminator, use `--flush-timeout 100ms` or another duration. That mode displays incomplete buffered data after the connection has been idle for the requested time.

Sent messages are processed for escape sequences before the configured send terminator is appended. For example, `Hello\r\nWorld` sends `Hello` followed by CRLF and `World`.

## Requirements

- Go 1.24.4 or later
- Network connectivity for client/server communication

## Dependencies

coe uses only the Go standard library.
