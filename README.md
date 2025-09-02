# gowscl

`gowscl` is a Go package built on top of **github.com/coder/websocket**, providing a robust WebSocket client with advanced features such as auto‑reconnection (exponential back‑off with jitter), heartbeat (ping/pong), message queuing during disconnections, event callbacks, and thread‑safety. It simplifies building reliable WebSocket applications in idiomatic Go.

## Features

- **Auto‑Reconnect** – Automatically reconnects with exponential back‑off, configurable jitter and a maximum‑failure limit.  
- **Heartbeat** – Periodic pings with configurable interval and pong timeout; failures trigger reconnection.  
- **Message Queuing** – Outgoing messages are queued while the connection is down and flushed once a new connection is established (queue size is configurable).  
- **Event Callbacks** – Hooks for connection open, incoming messages, errors, and connection close.  
- **Thread‑Safe** – All mutable state is protected by a mutex, allowing concurrent use of the client.  
- **Configurable Options** – Fine‑grained control over reconnect timings, timeouts, sub‑protocols, custom headers, logger, etc.  
- **JSON Helper** – `SendJSON` marshals a value to JSON and enqueues it as a text frame.  
- **Graceful Shutdown** – `Close` blocks until all goroutines finish; `CloseWithTimeout` lets you bound the wait time.

## Installation
```bash
    go get github.com/evdnx/gowscl
```

## Quick Start

```go
package main

import (
	"log"
	"time"

	"github.com/evdnx/gowscl"
	"github.com/coder/websocket"
)

func main() {
	client := gowscl.NewClient(
		"ws://example.com/ws",
		gowscl.WithOnOpen(func() { log.Println("Connected") }),
		gowscl.WithOnMessage(func(data []byte, typ websocket.MessageType) {
			log.Printf("← %s (type=%v)", string(data), typ)
		}),
		gowscl.WithOnError(func(err error) { log.Printf("error: %v", err) }),
		gowscl.WithOnClose(func() { log.Println("Closed") }),

		// Example customizations
		gowscl.WithInitialReconnect(2*time.Second),
		gowscl.WithMaxReconnect(60*time.Second),
		gowscl.WithPingInterval(15*time.Second),
	)

	if err := client.Connect(); err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer client.Close()

	// Send a plain‑text message
	if err := client.Send([]byte("Hello, WebSocket!"), websocket.MessageText); err != nil {
		log.Printf("send failed: %v", err)
	}

	// Send a JSON payload
	if err := client.SendJSON(map[string]string{"msg": "Hello, JSON!"}); err != nil {
		log.Printf("sendJSON failed: %v", err)
	}

	// Keep the program running (or do other work)
	select {}
}
```

## Configuration Options

| Option | Description |
|--------|-------------|
| `WithOnOpen(func())` | Called when a connection is successfully opened. |
| `WithOnMessage(func([]byte, websocket.MessageType))` | Called for each incoming text or binary frame. |
| `WithOnError(func(error))` | Called whenever the client encounters an error (dial, read, write, ping, etc.). |
| `WithOnClose(func())` | Called when the client is shut down. |
| `WithInitialReconnect(time.Duration)` | First back‑off delay after a failed handshake (default = 1 s). |
| `WithMaxReconnect(time.Duration)` | Upper bound for the back‑off delay (default = 30 s). |
| `WithReconnectFactor(float64)` | Multiplicative factor for exponential growth (default = 2.0). |
| `WithReconnectJitter(float64)` | Fraction of the current back‑off to add as random jitter (default = 0.5). |
| `WithPingInterval(time.Duration)` | How often to send a ping frame (default = 30 s). |
| `WithPongTimeout(time.Duration)` | Time allowed to receive a pong after a ping (default = 10 s). |
| `WithWriteTimeout(time.Duration)` | Per‑message write deadline (default = 10 s). |
| `WithReadTimeout(time.Duration)` | Read deadline for the underlying connection (default = 10 s). |
| `WithHandshakeTimeout(time.Duration)` | Timeout for the initial WebSocket handshake (default = 5 s). |
| `WithMessageQueueSize(int)` | Capacity of the outbound queue (default = 100). |
| `WithCloseGracePeriod(time.Duration)` | Grace period before force‑closing the connection (default = 5 s). |
| `WithLogger(*golog.Logger)` | Inject a custom logger; defaults to a JSON logger writing to stdout. |
| `WithSubprotocols(...string)` | List of sub‑protocols advertised during the handshake. |
| `WithHeaders(map[string][]string)` | Additional HTTP headers for the handshake. |
| `WithCompression(bool)` | Placeholder for future compression support. |
| `WithMetrics(*Metrics)` | Supply callbacks for instrumentation (reconnect, queue drops, ping failures, permanent errors). |
| `WithPinger(Pinger)` | Override the default ping implementation. |
| `WithMaxConsecutiveFailures(int)` | Stop retrying after this many consecutive failures (default = 10). |
| `WithDialer(func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error))` | Inject a custom dial function (useful for tests). |

### Metrics Callbacks

```go
type Metrics struct {
    OnReconnect      func(wait time.Duration)          // each back‑off wait
    OnQueueDrop      func(msg queuedMessage)           // when the queue overflows
    OnPingFailure    func(err error)                   // ping returned an error
    OnPermanentError func(err error)                   // client gave up reconnecting
}
```

## Internals Worth Knowing

- **Back‑off algorithm** – After each failed handshake the client waits `reconnectWait + jitter`, then multiplies `reconnectWait` by `reconnectFactor` (capped at `maxReconnect`). Jitter is a random fraction (`reconnectJitter`) of the current wait.  
- **Maximum failure limit** – When `consecFails` reaches `maxConsecutiveFails`, the client reports a permanent error via `Metrics.OnPermanentError` and stops further retries.  
- **Heartbeat** – Implemented by a ticker that calls the injected `Pinger`. If a ping fails, the connection is closed, the error is forwarded through `OnError` and `Metrics.OnPingFailure`, and the reconnect loop takes over.  
- **Message queuing** – `Send` enqueues directly into `msgQueue`. If the queue is full, `Metrics.OnQueueDrop` (if set) is invoked and an error is returned. `writeLoop` drains the queue, re‑queues messages on transient write errors, and respects `writeTimeout`.  
- **Graceful shutdown** – `Close` cancels the root context, closes the underlying WebSocket, and waits for all worker goroutines (`readLoop`, `writeLoop`, `heartbeat`, and the reconnect manager) to finish. `CloseWithTimeout` adds a deadline to that wait.

## Testing

The repository ships with a comprehensive test suite (`main_test.go`). Highlights:

- **Default construction** – verifies that all default values match the constants defined in the source.  
- **Custom options** – ensures functional options correctly override defaults.  
- **Queue behavior** – tests normal queuing, overflow handling, and `SendJSON` encoding.  
- **Successful handshake & callbacks** – spins up an in‑process echo server and validates that `OnOpen`, `OnMessage`, and `OnClose` fire as expected.  
- **Reconnect back‑off** – injects a failing dialer and asserts that the back‑off sequence respects the configured factor, jitter, and max‑failure limit.  
- **Heartbeat failure** – uses a flaky `Pinger` to trigger a ping error, confirming metric callbacks and connection teardown.  
- **Graceful shutdown with timeout** – checks that `CloseWithTimeout` respects a provided context deadline.

Run the tests with:

```bash
go test ./...
```

## Roadmap

- **Compression support** – enable per‑message compression when the underlying library adds it.  
- **Extended metrics** – expose counters for messages sent/received, throughput, and connection uptime.  
- **TLS & Proxy configuration** – allow custom transport settings.

## License

MIT-0 License – see the `LICENSE` file for details.

## Acknowledgments

- Built on top of **github.com/coder/websocket**.  
- Thanks to the original author *nhooyr* and the Coder team for their solid WebSocket implementation.
