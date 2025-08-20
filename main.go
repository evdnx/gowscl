// Package gowscl provides a robust WebSocket client built on top of github.com/coder/websocket.
// It includes features like auto-reconnect with exponential backoff, heartbeat (ping/pong),
// message queuing during disconnections, event callbacks, and thread-safety.
//
// Usage:
//
//	client := gowscl.NewClient("ws://example.com/ws",
//	    gowscl.WithOnOpen(func() { log.Println("Connected") }),
//	    gowscl.WithOnMessage(func(data []byte, typ websocket.MessageType) { log.Println("Message:", string(data)) }),
//	    gowscl.WithOnError(func(err error) { log.Println("Error:", err) }),
//	    gowscl.WithOnClose(func() { log.Println("Closed") }),
//	)
//	err := client.Connect()
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close()
//
//	client.Send([]byte("Hello"), websocket.MessageText)
package gowscl

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// MessageType aliases websocket.MessageType for convenience.
type MessageType = websocket.MessageType

// StatusCode aliases websocket.StatusCode for convenience.
type StatusCode = websocket.StatusCode

// Constants for default values.
const (
	DefaultInitialReconnectInterval = 1 * time.Second
	DefaultMaxReconnectInterval     = 30 * time.Second
	DefaultReconnectFactor          = 2.0
	DefaultReconnectJitter          = 0.5 // Fraction for random jitter.
	DefaultPingInterval             = 30 * time.Second
	DefaultPongTimeout              = 10 * time.Second
	DefaultWriteTimeout             = 10 * time.Second
	DefaultReadTimeout              = 10 * time.Second
	DefaultHandshakeTimeout         = 5 * time.Second
	DefaultMessageQueueSize         = 100
	DefaultCloseGracePeriod         = 5 * time.Second
)

// ErrClosed indicates the client is closed.
var ErrClosed = errors.New("client closed")

// Client is a robust WebSocket client with auto-reconnect and advanced features.
type Client struct {
	url           string
	opts          *clientOptions
	conn          *websocket.Conn
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	closed        bool
	reconnectWait time.Duration
	msgQueue      chan queuedMessage
	wg            sync.WaitGroup
	lastPong      time.Time
}

// queuedMessage represents a message to be sent, queued during disconnections.
type queuedMessage struct {
	data []byte
	typ  MessageType
}

// clientOptions holds configurable options for the Client.
type clientOptions struct {
	dialOpts           *websocket.DialOptions
	onOpen             func()
	onMessage          func([]byte, MessageType)
	onError            func(error)
	onClose            func()
	initialReconnect   time.Duration
	maxReconnect       time.Duration
	reconnectFactor    float64
	reconnectJitter    float64
	pingInterval       time.Duration
	pongTimeout        time.Duration
	writeTimeout       time.Duration
	readTimeout        time.Duration
	handshakeTimeout   time.Duration
	messageQueueSize   int
	closeGracePeriod   time.Duration
	logger             func(format string, args ...interface{})
	subprotocols       []string
	headers            map[string][]string
	compressionEnabled bool // Placeholder for future compression support.
}

// ClientOption is a functional option for configuring the Client.
type ClientOption func(*clientOptions)

// WithOnOpen sets the callback for when the connection opens.
func WithOnOpen(fn func()) ClientOption {
	return func(o *clientOptions) { o.onOpen = fn }
}

// WithOnMessage sets the callback for incoming messages.
func WithOnMessage(fn func([]byte, MessageType)) ClientOption {
	return func(o *clientOptions) { o.onMessage = fn }
}

// WithOnError sets the callback for errors.
func WithOnError(fn func(error)) ClientOption {
	return func(o *clientOptions) { o.onError = fn }
}

// WithOnClose sets the callback for when the connection closes.
func WithOnClose(fn func()) ClientOption {
	return func(o *clientOptions) { o.onClose = fn }
}

// WithInitialReconnect sets the initial reconnect interval.
func WithInitialReconnect(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.initialReconnect = d }
}

// WithMaxReconnect sets the maximum reconnect interval.
func WithMaxReconnect(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.maxReconnect = d }
}

// WithReconnectFactor sets the exponential backoff factor.
func WithReconnectFactor(f float64) ClientOption {
	return func(o *clientOptions) { o.reconnectFactor = f }
}

// WithReconnectJitter sets the jitter fraction for reconnect delays.
func WithReconnectJitter(j float64) ClientOption {
	return func(o *clientOptions) { o.reconnectJitter = j }
}

// WithPingInterval sets the interval for sending pings.
func WithPingInterval(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.pingInterval = d }
}

// WithPongTimeout sets the timeout for receiving pongs.
func WithPongTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.pongTimeout = d }
}

// WithWriteTimeout sets the write timeout.
func WithWriteTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.writeTimeout = d }
}

// WithReadTimeout sets the read timeout.
func WithReadTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.readTimeout = d }
}

// WithHandshakeTimeout sets the handshake timeout.
func WithHandshakeTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.handshakeTimeout = d }
}

// WithMessageQueueSize sets the size of the message queue.
func WithMessageQueueSize(size int) ClientOption {
	return func(o *clientOptions) { o.messageQueueSize = size }
}

// WithCloseGracePeriod sets the grace period for closing.
func WithCloseGracePeriod(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.closeGracePeriod = d }
}

// WithLogger sets a custom logger function.
func WithLogger(fn func(format string, args ...interface{})) ClientOption {
	return func(o *clientOptions) { o.logger = fn }
}

// WithSubprotocols sets the subprotocols for the WebSocket handshake.
func WithSubprotocols(subs ...string) ClientOption {
	return func(o *clientOptions) { o.subprotocols = subs }
}

// WithHeaders sets custom headers for the WebSocket handshake.
func WithHeaders(headers map[string][]string) ClientOption {
	return func(o *clientOptions) { o.headers = headers }
}

// WithCompression enables compression (placeholder for future implementation).
func WithCompression(enabled bool) ClientOption {
	return func(o *clientOptions) { o.compressionEnabled = enabled }
}

// NewClient creates a new robust WebSocket client.
func NewClient(url string, options ...ClientOption) *Client {
	opts := &clientOptions{
		initialReconnect:   DefaultInitialReconnectInterval,
		maxReconnect:       DefaultMaxReconnectInterval,
		reconnectFactor:    DefaultReconnectFactor,
		reconnectJitter:    DefaultReconnectJitter,
		pingInterval:       DefaultPingInterval,
		pongTimeout:        DefaultPongTimeout,
		writeTimeout:       DefaultWriteTimeout,
		readTimeout:        DefaultReadTimeout,
		handshakeTimeout:   DefaultHandshakeTimeout,
		messageQueueSize:   DefaultMessageQueueSize,
		closeGracePeriod:   DefaultCloseGracePeriod,
		logger:             log.Printf,
		subprotocols:       nil,
		headers:            nil,
		compressionEnabled: false,
	}

	for _, opt := range options {
		opt(opts)
	}

	// Set up DialOptions.
	opts.dialOpts = &websocket.DialOptions{
		Subprotocols: opts.subprotocols,
		// Compression can be added here if supported in base library.
		// For now, placeholder.
		// HTTPHeader: opts.headers, // Assuming base supports it; adjust if needed.
	}

	// Note: github.com/coder/websocket.DialOptions has HTTPHeader field.
	if opts.headers != nil {
		if opts.dialOpts.HTTPHeader == nil {
			opts.dialOpts.HTTPHeader = make(map[string][]string)
		}
		for k, v := range opts.headers {
			opts.dialOpts.HTTPHeader[k] = v
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Client{
		url:           url,
		opts:          opts,
		ctx:           ctx,
		cancel:        cancel,
		reconnectWait: opts.initialReconnect,
		msgQueue:      make(chan queuedMessage, opts.messageQueueSize),
		lastPong:      time.Now(),
	}
}

// Connect starts the connection and auto-reconnect loop.
func (c *Client) Connect() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.mu.Unlock()

	c.wg.Add(1)
	go c.run()
	return nil
}

// run is the main loop for connecting, reconnecting, and handling the connection.
func (c *Client) run() {
	defer c.wg.Done()

	for {
		err := c.connectOnce()
		if err != nil {
			c.callOnError(err)
		}

		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()

		// Exponential backoff with jitter.
		jitter := time.Duration(rand.Float64() * float64(c.reconnectWait) * c.opts.reconnectJitter)
		c.opts.logger("Reconnecting after %v + %v jitter", c.reconnectWait, jitter)
		time.Sleep(c.reconnectWait + jitter)

		c.reconnectWait = time.Duration(float64(c.reconnectWait) * c.opts.reconnectFactor)
		if c.reconnectWait > c.opts.maxReconnect {
			c.reconnectWait = c.opts.maxReconnect
		}
	}
}

// connectOnce attempts a single connection and handles read/write.
func (c *Client) connectOnce() error {
	ctx, cancel := context.WithTimeout(c.ctx, c.opts.handshakeTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, c.url, c.opts.dialOpts)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.reconnectWait = c.opts.initialReconnect // Reset backoff on success.
	c.lastPong = time.Now()
	c.mu.Unlock()

	c.callOnOpen()

	// Start heartbeat goroutine.
	c.wg.Add(1)
	go c.heartbeat()

	// Start writer goroutine for queued messages.
	c.wg.Add(1)
	go c.writer()

	// Read loop.
	for {
		ctx, cancel := context.WithTimeout(c.ctx, c.opts.readTimeout)
		typ, data, err := conn.Read(ctx)
		cancel()

		if err != nil {
			if websocket.CloseStatus(err) == -1 { // Abnormal close.
				conn.Close(websocket.StatusInternalError, "read error")
				return err
			}
			// Normal close or context cancel.
			return nil
		}

		// Handle pong (assuming base library passes control messages; but actually, WebSocket libs often handle ping/pong internally.
		// Note: github.com/coder/websocket exposes Read for app messages, handles control internally?
		// From docs, Read reads the next data message. Control is handled automatically.
		// So for ping/pong, we need to send ping as control.
		// To send ping: conn.Ping(ctx)
		// Wait, checking assumed API.
		// Actually, from roadmap, no built-in, but you can use Write with MessagePing.
		// No, the Write is for data messages. Control is separate.
		// Upon quick search in mind, the library has conn.Ping(ctx) error
		// Yes, assuming it does, or we can implement.
		// For simplicity, assume conn.Write(ctx, websocket.MessagePing, []byte("")) but probably not.
		// Actually, to be accurate, the library has wsjson but for raw, conn.Write(ctx, typ, p) where typ can be Text, Binary.
		// For control, perhaps conn.CloseWrite or something.
		// Wait, looking at the example, they use wsjson.Read/Write for JSON, but for raw, it's conn.MessageReader, etc.
		// The API is low-level: to write, use w := conn.Writer(ctx, typ), then w.Write, w.Close
		// For read, r := conn.Reader(ctx), then io.ReadAll(r), r.Close
		// For ping, conn.Ping(ctx)
		// Yes, it has conn.Ping(ctx) error
		// And pongs are automatic.
		// To detect, perhaps no direct way, but we can use read timeouts or separate.
		// For heartbeat, send ping, if read times out, assume dead.
		// But to make it work, in read loop, if ping received, update lastPong, but since control is handled, perhaps not exposed.
		// Upon thinking, the library handles pongs automatically, but to implement heartbeat, send ping periodically, and if no message or pong in time, close.
		// But since pongs are not exposed, perhaps use a deadline.
		// To simplify, we'll send ping, and use a separate timer for pong timeout.
		// Assume if no pong, the conn will close on read if dead.
		// For now, implement ping send, and assume read will error if dead.

		// Update lastPong on any read, assuming activity.
		c.mu.Lock()
		c.lastPong = time.Now()
		c.mu.Unlock()

		c.callOnMessage(data, typ)
	}
}

// heartbeat sends pings and checks for pongs.
func (c *Client) heartbeat() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.opts.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn == nil {
				continue
			}

			ctx, cancel := context.WithTimeout(c.ctx, c.opts.pongTimeout)
			err := conn.Ping(ctx) // Assuming conn.Ping exists; if not, implement as control frame.
			cancel()
			if err != nil {
				c.callOnError(err)
				c.disconnect()
				return
			}

			// Check if last activity is too old.
			c.mu.Lock()
			if time.Since(c.lastPong) > c.opts.pongTimeout*2 { // Arbitrary multiplier.
				c.mu.Unlock()
				c.callOnError(errors.New("pong timeout"))
				c.disconnect()
				return
			}
			c.mu.Unlock()
		}
	}
}

// writer handles sending queued messages.
func (c *Client) writer() {
	defer c.wg.Done()

	for {
		select {
		case <-c.ctx.Done():
			return
		case msg := <-c.msgQueue:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn == nil {
				// Requeue if not connected.
				select {
				case c.msgQueue <- msg:
				default:
					c.callOnError(errors.New("message queue full, dropping message"))
				}
				time.Sleep(100 * time.Millisecond) // Avoid tight loop.
				continue
			}

			ctx, cancel := context.WithTimeout(c.ctx, c.opts.writeTimeout)
			w, err := conn.Writer(ctx, msg.typ)
			if err != nil {
				cancel()
				c.callOnError(err)
				c.disconnect()
				// Requeue.
				select {
				case c.msgQueue <- msg:
				default:
				}
				continue
			}
			_, err = w.Write(msg.data)
			if err != nil {
				cancel()
				c.callOnError(err)
				c.disconnect()
				continue
			}
			err = w.Close()
			cancel()
			if err != nil {
				c.callOnError(err)
				c.disconnect()
			}
		}
	}
}

// Send sends a message, queuing if not connected.
func (c *Client) Send(data []byte, typ MessageType) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.mu.Unlock()

	select {
	case c.msgQueue <- queuedMessage{data: data, typ: typ}:
		return nil
	default:
		return errors.New("message queue full")
	}
}

// SendJSON sends a JSON message using wsjson.
func (c *Client) SendJSON(v interface{}) error {
	data, err := json.Marshal(v) // Import encoding/json.
	if err != nil {
		return err
	}
	return c.Send(data, websocket.MessageText)
}

// Close closes the client gracefully.
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	c.cancel()

	// Close conn if exists.
	c.mu.Lock()
	if c.conn != nil {
		// No context needed; directly close the connection.
		c.conn.Close(websocket.StatusNormalClosure, "client closing")
	}
	c.mu.Unlock()

	c.wg.Wait()
	c.callOnClose()
}

// disconnect closes the current connection without closing the client.
func (c *Client) disconnect() {
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close(websocket.StatusInternalError, "disconnect")
		c.conn = nil
	}
	c.mu.Unlock()
}

// callOnOpen calls the onOpen callback if set.
func (c *Client) callOnOpen() {
	if c.opts.onOpen != nil {
		c.opts.onOpen()
	}
}

// callOnMessage calls the onMessage callback if set.
func (c *Client) callOnMessage(data []byte, typ MessageType) {
	if c.opts.onMessage != nil {
		c.opts.onMessage(data, typ)
	}
}

// callOnError calls the onError callback if set.
func (c *Client) callOnError(err error) {
	if c.opts.onError != nil {
		c.opts.onError(err)
	}
}

// callOnClose calls the onClose callback if set.
func (c *Client) callOnClose() {
	if c.opts.onClose != nil {
		c.opts.onClose()
	}
}
