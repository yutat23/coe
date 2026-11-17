# coe

```
 ██████╗ ██████╗ ███████╗
██╔════╝██╔═══██╗██╔════╝
██║     ██║   ██║█████╗
██║     ██║   ██║██╔══╝
╚██████╗╚██████╔╝███████╗
 ╚═════╝ ╚═════╝ ╚══════╝
```
<img width="1168" height="795" alt="image" src="https://github.com/user-attachments/assets/51aabeeb-ff85-4aee-ba52-a415b144f06e" />

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
- Optional interactive TUI with `--tui` (client list, frame log, hex dump)
- Time-based response waiting with `--wait` and script-friendly output with `--quiet`
- Receive frame size limit (`--max-frame-size`) and server connection limit (`--max-clients`)
- Per-write socket deadline (`--write-timeout`) so a stalled peer cannot freeze the server
- Connect timeout (`--connect-timeout`) and a listen address (`-s host:port` or `--bind`)
- Session log file (`--log`) with full frames and connection events

## Installation

Homebrew (macOS, Linux):

```bash
brew install yutat23/tap/coe
```

Prebuilt binaries for Windows (x64, arm64, x86), Linux, and macOS are attached to each [release](https://github.com/yutat23/coe/releases).

From source:

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
coe -s [host:]<port> [terminator] [options]
coe -c <host> <port> [terminator] [options]
coe -c <host>:<port> [terminator] [options]
coe --version
```

`terminator` can be `LF`, `CR`, or `CRLF`. If omitted, both send and receive default to `LF` unless overridden.

The server listens on every interface unless a host is given (`-s 127.0.0.1:8080` or `--bind 127.0.0.1`). `-s 0` lets the system pick a free port and prints the one in use. IPv6 addresses take brackets in the combined form (`[::1]:8080`); with a separate port, `coe -c ::1 8080` also works. Ports must be numbers from 1 to 65535 (0 is allowed for the server only).

## Options

- `-tt T`, `--tx-term T`, `--send-terminator T`: outgoing terminator
- `-rt T`, `--rx-term T`, `--recv-terminator T`: incoming frame terminator
- `--buffer-size <size>`: `Read` chunk size in bytes, default `1024`. This is not a frame size limit.
- `--max-frame-size <size>`: maximum assembled receive frame in bytes, including the terminator; `0` is unlimited. Default `1048576`. Connections that exceed it are closed.
- `--max-clients <n>`: maximum concurrent server clients; `0` is unlimited. Default `64`. Extra connections are rejected.
- `--write-timeout <duration>`: deadline for each socket write, for example `5s`; `0`/`off` disables it. Default `5s`. Timed-out clients are dropped.
- `--connect-timeout <duration>`: give up connecting after this long, client mode only; `0`/`off` waits for the OS. Default `10s`.
- `--flush-timeout <duration>`: display incomplete buffered data after inactivity, for example `100ms` (at least `1ms`); default off
- `-m <message>`, `--message <message>`: send one message and exit without starting the interactive prompt
- `--wait <duration>`: with `-m`, receive frames for this duration after sending; omitted means fire-and-forget. With piped input (no `-m`), wait at most this long for replies after the input ends.
- `--quiet`: with `-m`, write response payloads (one line per frame) to stdout and connection/log output to stderr
- `--log <file>`: append frames and connection events to a file, without colors. Frames are logged in full, also in the TUI.
- `--bind <host>`: listen on one address only, server mode only. Cannot be combined with `-s host:port`.
- `--color`: always color the output
- `--no-color`: never color the output. Without either flag, output is colored only on a terminal and only when the `NO_COLOR` environment variable is unset or empty. The flags apply to the TUI as well.
- `--no-echo`: disable server echo-back, server mode only
- `--tui`: interactive TUI for server and client sessions. Requires a terminal. Incompatible with `-m`.
- `-v`, `--version`: print the version

Errors (bad arguments, connection failures) are written to stderr.

## Server Commands

- `#send <clientAddr> <message>`: send to one client. Use the `IP:port` value shown by `#list`. The message keeps repeated and trailing spaces.
- `#broadcast <message>`: send to all clients. The message keeps repeated and trailing spaces.
- `#list`: show connected clients
- `#help`: show server command help
- `#help program`: show full program help
- `#quit`, `#exit`: shut down the server

Asynchronous log lines erase the `Command>` prompt, print, and draw it again, so they do not run into a half-typed command. Characters typed before the redraw are still in the input line even though they are no longer shown.

Ctrl+D on a terminal shuts the server down. When standard input is not a terminal (a background job, `nohup`, a service, a container without `-i`) and it ends or cannot be read, the server keeps running without commands; stop it with Ctrl+C or SIGTERM.

## Client Input

Each line typed in the interactive client is sent with the send terminator; an empty line sends just the terminator (an empty frame). When standard input is a pipe or file, the client sends every line, closes its sending side when the input ends, and keeps printing replies until the peer closes the connection:

```bash
printf 'STATUS\nVERSION\n' | coe -c 127.0.0.1:8080
printf 'STATUS\n' | coe -c 127.0.0.1:8080 --wait 2s
```

A peer that keeps the connection open after the input ends would keep the client waiting; add `--wait` to stop after that long, or press Ctrl+C.

## Examples

```bash
coe -s 8080
coe -s 8080 CR --no-echo
coe -s 8080 -tt CR -rt CRLF
coe -s 8080 --flush-timeout 100ms
coe -s 8080 --max-frame-size 4096 --max-clients 8 --write-timeout 2s
coe -s 8080 --tui
coe -s 127.0.0.1:8080 --log session.log

coe -c 127.0.0.1 8080
coe -c 127.0.0.1:8080
coe -c ::1 8080
coe -c 127.0.0.1 8080 LF
coe -c 127.0.0.1 8080 -tt CR -rt CRLF
coe -c 192.168.1.100 8080 CR --buffer-size 512 --no-color --connect-timeout 3s
coe -c 127.0.0.1 8080 -m "STATUS\r" --wait 1s --quiet
coe -c 127.0.0.1 8080 --tui
```

`"STATUS\r"` reaches coe as the eight characters `STATUS\r` in bash, zsh, PowerShell, and cmd.exe, and coe turns `\r` into a CR byte. Do not double the backslash: in PowerShell and cmd.exe `"STATUS\\r"` stays two backslashes, which coe sends as a literal `\` followed by `r`.

Exit status:

- `0`: success, including `--help` and no-argument usage
- `0` also when the server or TUI is stopped with Ctrl+C, SIGINT, or SIGTERM
- `1`: connection or bind/listen failure (including a connect timeout), receive-side I/O error (including oversized frame), interactive send/input failure, or a log file that cannot be opened
- `2`: usage/argument error, including an invalid port; in one-shot mode, also a send failure
- `3`: `--wait` elapsed with no response frame. Unexpected receive I/O errors during `--wait` return `1`, even if some frames already arrived. A clean EOF after receiving frames is still `0`.

## Message Processing

By default, received data is buffered until the configured receive terminator is found. This avoids splitting TCP data just because packets arrive in multiple chunks. `--buffer-size` only controls how much is read from the socket at once; `--max-frame-size` caps the assembled frame and closes the connection if it is exceeded.

For devices or protocols that do not send a terminator, use `--flush-timeout 100ms` or another duration. That mode displays incomplete buffered data after the connection has been idle for the requested time.

Sent messages are processed for escape sequences before the configured send terminator is appended. For example, `Hello\r\nWorld` sends `Hello` followed by CRLF and `World`.

Logged payloads use the same notation for control bytes, so a stray CR shows up as `hello\r` and a peer's escape sequences appear as `\x1B[2J` instead of acting on your terminal. The `HEX:` column shows the bytes of the frame, including its terminator. With `-rt CR`, an LF that directly follows the CR is dropped while framing (so peer CRLF does not produce empty frames); that LF appears in neither the message nor the `HEX:` column. `--quiet` one-shot output on stdout stays raw for scripts.

## Requirements

- Go 1.26 or later to build (the TUI libraries require it)
- Network connectivity for client/server communication

## TUI

`--tui` replaces the line prompt with a full-screen session. One-shot (`-m`) stays on the original CLI.

Server layout: client list on the left, frames on the right, input at the bottom. `Tab` / `Shift+Tab` selects the target (`*` is broadcast, the default). `Enter` sends to the target. The target never changes on its own: if the selected client disconnects it becomes `(none)` and the typed line is kept until you pick another one. `#send`, `#broadcast`, `#list`, `#help`, and `#quit` still work. `F2` toggles a hex dump under each frame. `F1` or `#help` shows help.

Client layout: conversation log plus input, with the same hex and history keys. When the peer closes the connection the log stays on screen; press `Ctrl+C` to exit, and the reason is printed after the screen is restored.

Long frames wrap to the window width, and the hex dump uses 16, 8, or 4 bytes per row depending on the space available. The byte count always shows the full frame size, but the TUI keeps only the first 2 KiB of each frame for display, and the history is capped at 2000 entries and about 4 MiB of kept frame data, dropping the oldest entries first. Use `--log` to keep every frame in full. Toggling the hex dump or resizing never drops history. An empty line sends just the terminator to the selected client; with the `*` broadcast target it is ignored, so a stray Enter does not reach every client (`#broadcast` with no text sends an empty frame to all). Lines typed quickly are sent in order, and a multi-line paste is sent one line at a time; text after the last newline stays in the input line.

## Dependencies

Interactive `--tui` uses Charm's Bubble Tea, Bubbles, and Lip Gloss (v2). Line-oriented and one-shot modes keep the previous CLI behavior and do not query the terminal at startup.

## Releases

`build.ps1` (PowerShell 7) builds Windows, Linux, and macOS archives into `release/`. The version comes from `version` in `main.go`. Archives are reproducible for a given commit, and the Linux and macOS binaries keep their execute permission when unzipped.

## License

MIT. See [LICENSE](LICENSE).
