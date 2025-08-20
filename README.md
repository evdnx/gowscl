# gowscl

gowscl is a Go package built on top of [github.com/coder/websocket](https://github.com/coder/websocket), providing a robust WebSocket client with advanced features like auto-reconnection with exponential backoff, heartbeat (ping/pong), message queuing during disconnections, event callbacks, and thread-safety. It simplifies building reliable WebSocket applications in idiomatic Go.

## Features

- **Auto-Reconnect**: Automatically reconnects with exponential backoff and configurable jitter to handle network interruptions gracefully.
- **Heartbeat**: Sends periodic pings and monitors pongs to detect connection issues.
- **Message Queuing**: Queues messages during disconnections and sends them once reconnected, preventing message loss.
- **Event Callbacks**: Supports callbacks for connection open, message receipt, errors, and connection close.
- **Thread-Safe**: Uses mutexes to ensure safe concurrent access.
- **Configurable Options**: Customize reconnect intervals, timeouts, subprotocols, headers, and more.
- **JSON Support**: Simplifies sending JSON messages with a dedicated `SendJSON` method.
- **Graceful Shutdown**: Ensures clean closure of connections with a configurable grace period.

## Installation

Install the package using:

```bash
go get github.com/evdnx/gowscl
```

*Note: Replace `github.com/evdnx/gowscl` with the actual repository path once published.*

Ensure you have the dependency `github.com/coder/websocket` installed:

```bash
go get github.com/coder/websocket
```

## Usage

Below is a basic example of using the gowscl client:

```go
package main

import (
	"log"
	"github.com/evdnx/gowscl"
	"github.com/coder/websocket"
)

func main() {
	client := gowscl.NewClient("ws://example.com/ws",
		gowscl.WithOnOpen(func() { log.Println("Connected to WebSocket server") }),
		gowscl.WithOnMessage(func(data []byte, typ websocket.MessageType) {
			log.Printf("Received message: %s (Type: %v)", string(data), typ)
		}),
		gowscl.WithOnError(func(err error) { log.Printf("Error: %v", err) }),
		gowscl.WithOnClose(func() { log.Println("Connection closed") }),
		gowscl.WithInitialReconnect(2*time.Second),
		gowscl.WithMaxReconnect(60*time.Second),
		gowscl.WithPingInterval(15*time.Second),
	)

	err := client.Connect()
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer client.Close()

	// Send a text message
	err = client.Send([]byte("Hello, WebSocket!"), websocket.MessageText)
	if err != nil {
		log.Printf("Failed to send message: %v", err)
	}

	// Send a JSON message
	err = client.SendJSON(map[string]string{"message": "Hello, JSON!"})
	if err != nil {
		log.Printf("Failed to send JSON: %v", err)
	}

	// Keep the program running
	select {}
}
```

## Configuration Options

Customize the client using the following options:

- `WithOnOpen(func())`: Callback when the connection opens.
- `WithOnMessage(func([]byte, websocket.MessageType))`: Callback for incoming messages.
- `WithOnError(func(error))`: Callback for errors.
- `WithOnClose(func())`: Callback when the connection closes.
- `WithInitialReconnect(time.Duration)`: Initial delay before reconnect attempts (default: 1s).
- `WithMaxReconnect(time.Duration)`: Maximum reconnect delay (default: 30s).
- `WithReconnectFactor(float64)`: Exponential backoff factor (default: 2.0).
- `WithReconnectJitter(float64)`: Random jitter fraction for reconnect delays (default: 0.5).
- `WithPingInterval(time.Duration)`: Interval for sending pings (default: 30s).
- `WithPongTimeout(time.Duration)`: Timeout for receiving pongs (default: 10s).
- `WithWriteTimeout(time.Duration)`: Timeout for write operations (default: 10s).
- `WithReadTimeout(time.Duration)`: Timeout for read operations (default: 10s).
- `WithHandshakeTimeout(time.Duration)`: Timeout for WebSocket handshake (default: 5s).
- `WithMessageQueueSize(int)`: Size of the message queue for disconnections (default: 100).
- `WithCloseGracePeriod(time.Duration)`: Grace period for closing connections (default: 5s).
- `WithLogger(func(string, ...interface{}))`: Custom logger function (default: `log.Printf`).
- `WithSubprotocols(...string)`: Subprotocols for the WebSocket handshake.
- `WithHeaders(map[string][]string)`: Custom headers for the WebSocket handshake.
- `WithCompression(bool)`: Enable compression (placeholder for future support).

Example with custom options:

```go
client := gowscl.NewClient("ws://example.com/ws",
	gowscl.WithInitialReconnect(5*time.Second),
	gowscl.WithMaxReconnect(120*time.Second),
	gowscl.WithSubprotocols("chat", "superchat"),
	gowscl.WithHeaders(map[string][]string{
		"Authorization": {"Bearer your-token"},
	}),
	gowscl.WithLogger(func(format string, args ...interface{}) {
		log.Printf("[CUSTOM LOG] "+format, args...)
	}),
)
```

## Notes

- **Ping/Pong Handling**: The client sends pings at the configured interval and monitors connection health. If no activity (pong or message) is detected within twice the pong timeout, the connection is considered dead and reconnection is attempted.
- **Message Queuing**: Messages sent during disconnections are queued (up to the configured queue size) and sent upon reconnection.
- **Compression**: Currently a placeholder; compression support depends on the underlying `github.com/coder/websocket` library. Check its roadmap for updates.
- **Thread-Safety**: The client is safe for concurrent use, with internal mutexes protecting connection state and message queuing.
- **Error Handling**: Errors during connection, read, write, or ping/pong are reported via the `OnError` callback, and the client attempts to reconnect automatically.

## Comparison with Other Libraries

- **gorilla/websocket**: Mature and widely used but lacks built-in auto-reconnect and heartbeat. gowscl adds these features while maintaining idiomatic Go.
- **golang.org/x/net/websocket**: Deprecated; gowscl uses the modern `github.com/coder/websocket` for better performance and API.
- **gobwas/ws** and **lesismal/nbio**: Event-driven for performance but complex. gowscl prioritizes simplicity and idiomatic Go while offering advanced features.

## Roadmap

- Add server-side wrapper for robust WebSocket servers.
- Implement compression when supported by `github.com/coder/websocket`.
- Add metrics for connection state, message throughput, and reconnect attempts.
- Support TLS configuration and proxy settings.
- Enhance testing with in-memory WebSocket testing (inspired by `wstest.Pipe` from `github.com/coder/websocket` roadmap).

## Contributing

Contributions are welcome! Please submit issues or pull requests to the [repository](https://github.com/evdnx/gowscl). Ensure code follows Go idioms and includes tests.

## License

MIT License. See [LICENSE](LICENSE) for details.

## Acknowledgments

- Built on top of [github.com/coder/websocket](https://github.com/coder/websocket).
- Thanks to the original author, nhooyr, and the current maintainers at Coder for their work on the underlying WebSocket library.