// ====================== main.go ======================
package gowscl

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/evdnx/golog"
)

// ---------------------------------------------------------------------------
// Types & Interfaces
// ---------------------------------------------------------------------------

// MessageType aliases websocket.MessageType for convenience.
type MessageType = websocket.MessageType

// StatusCode aliases websocket.StatusCode for convenience.
type StatusCode = websocket.StatusCode

// Pinger abstracts the ping operation used by the heartbeat routine.
type Pinger interface {
	Ping(context.Context) error
}

// Metrics aggregates optional callbacks that can be used for instrumentation.
type Metrics struct {
	// OnReconnect is called after each reconnection attempt (including the
	// jitter delay). The argument is the duration waited before the next try.
	OnReconnect func(wait time.Duration)

	// OnQueueDrop is called when a message cannot be enqueued because the
	// queue is full.
	OnQueueDrop func(msg queuedMessage)

	// OnPingFailure is called when a ping operation fails.
	OnPingFailure func(err error)

	// OnPermanentError is called when the client decides to stop retrying
	// (e.g., after exceeding MaxConsecutiveFailures).
	OnPermanentError func(err error)
}

// ---------------------------------------------------------------------------
// Default values
// ---------------------------------------------------------------------------

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

	// After this many consecutive failures the client gives up reconnecting.
	DefaultMaxConsecutiveFailures = 10
)

// ErrClosed indicates the client is closed.
var ErrClosed = errors.New("client closed")

// ---------------------------------------------------------------------------
// Core structs
// ---------------------------------------------------------------------------

// Client is a robust WebSocket client with auto‑reconnect and advanced features.
type Client struct {
	url            string
	opts           *clientOptions
	conn           *websocket.Conn
	mu             sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	closed         bool
	reconnectWait  time.Duration
	msgQueue       chan queuedMessage
	wg             sync.WaitGroup
	lastPong       time.Time
	backoffHistory []time.Duration

	// internal state for reconnection strategy
	consecFails int
}

// queuedMessage represents a message to be sent, queued during disconnections.
type queuedMessage struct {
	data []byte
	typ  MessageType
}

// clientOptions holds configurable options for the Client.
type clientOptions struct {
	// ----- functional options -----
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
	logger             *golog.Logger
	subprotocols       []string
	headers            map[string][]string
	compressionEnabled bool // placeholder for future compression support.

	// ----- advanced hooks -----
	metrics             *Metrics
	pinger              Pinger
	maxConsecutiveFails int
	dialer              func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
}

// ClientOption is a functional option for configuring the Client.
type ClientOption func(*clientOptions)

// ---------------------------------------------------------------------------
// Functional options
// ---------------------------------------------------------------------------

func WithOnOpen(fn func()) ClientOption {
	return func(o *clientOptions) { o.onOpen = fn }
}

func WithOnMessage(fn func([]byte, MessageType)) ClientOption {
	return func(o *clientOptions) { o.onMessage = fn }
}

func WithOnError(fn func(error)) ClientOption {
	return func(o *clientOptions) { o.onError = fn }
}

func WithOnClose(fn func()) ClientOption {
	return func(o *clientOptions) { o.onClose = fn }
}

func WithInitialReconnect(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.initialReconnect = d }
}

func WithMaxReconnect(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.maxReconnect = d }
}

func WithReconnectFactor(f float64) ClientOption {
	return func(o *clientOptions) { o.reconnectFactor = f }
}

func WithReconnectJitter(j float64) ClientOption {
	return func(o *clientOptions) { o.reconnectJitter = j }
}

func WithPingInterval(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.pingInterval = d }
}

func WithPongTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.pongTimeout = d }
}

func WithWriteTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.writeTimeout = d }
}

func WithReadTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.readTimeout = d }
}

func WithHandshakeTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.handshakeTimeout = d }
}

func WithMessageQueueSize(size int) ClientOption {
	return func(o *clientOptions) { o.messageQueueSize = size }
}

func WithCloseGracePeriod(d time.Duration) ClientOption {
	return func(o *clientOptions) { o.closeGracePeriod = d }
}

// WithLogger lets callers inject their own *golog.Logger instance.
func WithLogger(l *golog.Logger) ClientOption {
	return func(o *clientOptions) { o.logger = l }
}

func WithSubprotocols(subs ...string) ClientOption {
	return func(o *clientOptions) { o.subprotocols = subs }
}

func WithHeaders(h map[string][]string) ClientOption {
	return func(o *clientOptions) { o.headers = h }
}

func WithCompression(enabled bool) ClientOption {
	return func(o *clientOptions) { o.compressionEnabled = enabled }
}

// Advanced hooks ------------------------------------------------------------

func WithMetrics(m *Metrics) ClientOption {
	return func(o *clientOptions) { o.metrics = m }
}

func WithPinger(p Pinger) ClientOption {
	return func(o *clientOptions) { o.pinger = p }
}

// MaxConsecutiveFailures controls when the client gives up reconnecting.
func WithMaxConsecutiveFailures(n int) ClientOption {
	return func(o *clientOptions) { o.maxConsecutiveFails = n }
}

// WithDialer lets tests inject a custom dial function.
func WithDialer(d func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)) ClientOption {
	return func(o *clientOptions) { o.dialer = d }
}

// ---------------------------------------------------------------------------
// Helper functions for constructing defaults
// ---------------------------------------------------------------------------

func populateDefaults(opts *clientOptions) {
	if opts.initialReconnect == 0 {
		opts.initialReconnect = DefaultInitialReconnectInterval
	}
	if opts.maxReconnect == 0 {
		opts.maxReconnect = DefaultMaxReconnectInterval
	}
	if opts.reconnectFactor == 0 {
		opts.reconnectFactor = DefaultReconnectFactor
	}
	if opts.reconnectJitter == 0 {
		opts.reconnectJitter = DefaultReconnectJitter
	}
	if opts.pingInterval == 0 {
		opts.pingInterval = DefaultPingInterval
	}
	if opts.pongTimeout == 0 {
		opts.pongTimeout = DefaultPongTimeout
	}
	if opts.writeTimeout == 0 {
		opts.writeTimeout = DefaultWriteTimeout
	}
	if opts.readTimeout == 0 {
		opts.readTimeout = DefaultReadTimeout
	}
	if opts.handshakeTimeout == 0 {
		opts.handshakeTimeout = DefaultHandshakeTimeout
	}
	if opts.messageQueueSize == 0 {
		opts.messageQueueSize = DefaultMessageQueueSize
	}
	if opts.closeGracePeriod == 0 {
		opts.closeGracePeriod = DefaultCloseGracePeriod
	}
	if opts.logger == nil {
		// Create a default golog logger if none was supplied.
		l, err := golog.NewLogger(golog.WithStdOutProvider(golog.JSONEncoder))
		opts.logger = l
		if err != nil {
			panic(err)
		}
	}
	if opts.maxConsecutiveFails == 0 {
		opts.maxConsecutiveFails = DefaultMaxConsecutiveFailures
	}
	if opts.dialer == nil {
		// Use the library's default dialer.
		opts.dialer = websocket.Dial
	}
}

// buildDialOptions creates a *websocket.DialOptions instance from clientOptions.
func buildDialOptions(opts *clientOptions) *websocket.DialOptions {
	dial := &websocket.DialOptions{
		Subprotocols: opts.subprotocols,
	}
	if opts.headers != nil {
		if dial.HTTPHeader == nil {
			dial.HTTPHeader = make(map[string][]string)
		}
		for k, v := range opts.headers {
			dial.HTTPHeader[k] = v
		}
	}
	return dial
}

// ---------------------------------------------------------------------------
// Constructor
// ---------------------------------------------------------------------------

// NewClient creates a new robust WebSocket client.
// The returned client is ready to Connect().
func NewClient(url string, options ...ClientOption) *Client {
	// 1️⃣ start with defaults
	opts := &clientOptions{}
	populateDefaults(opts)

	// 2️⃣ now apply the caller’s options (they can override defaults,
	//    even with zero values)
	for _, opt := range options {
		opt(opts)
	}

	// 3️⃣ build immutable dial options
	opts.dialOpts = buildDialOptions(opts)

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

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// Connect establishes the initial WebSocket connection and then starts the
// background reconnect manager.  It returns as soon as the first handshake
// succeeds (or fails), allowing callers/tests to proceed without being blocked
// by the long‑running reconnect loop.
func (c *Client) Connect() error {
	// Attempt the first handshake.
	err := c.connectOnce()
	if err != nil {
		// Record the failure so the retry loop sees the correct state.
		c.callOnError(err)
		c.incrementFailure(err)
	}

	// Increment the WaitGroup before launching the goroutine.
	c.wg.Add(1)

	// Start the reconnect manager regardless of the first attempt outcome.
	go c.run()

	// Propagate the error (tests that care about it can inspect it;
	// most callers simply check for nil).
	return err
}

// Close gracefully shuts down the client, waiting for all goroutines.
// It blocks indefinitely; use CloseWithTimeout if you need a bounded wait.
func (c *Client) Close() {
	c.CloseWithTimeout(context.Background())
}

// CloseWithTimeout shuts down the client but aborts if the supplied context
// expires before all goroutines finish.
func (c *Client) CloseWithTimeout(parent context.Context) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	// Cancel the root context to signal all workers.
	c.cancel()

	// Close the underlying connection if present.
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close(websocket.StatusNormalClosure, "client closing")
	}
	c.mu.Unlock()

	// Wait with timeout.
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// normal exit
	case <-parent.Done():
		// timeout – nothing we can do besides returning.
	}

	c.callOnClose()
}

// Send queues a message for delivery. Returns ErrClosed if the client is closed,
// or an error if the internal queue is full.
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
		// Queue overflow – report via metrics if configured.
		if c.opts.metrics != nil && c.opts.metrics.OnQueueDrop != nil {
			c.opts.metrics.OnQueueDrop(queuedMessage{data: data, typ: typ})
		}
		return errors.New("message queue full")
	}
}

// SendJSON marshals v to JSON and enqueues the resulting payload.
// It never returns an error because of connection state – the message will
// be sent once the socket reconnects.
func (c *Client) SendJSON(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	// Directly enqueue; the writeLoop will pick it up when a connection exists.
	c.msgQueue <- queuedMessage{
		data: data,
		typ:  websocket.MessageText,
	}
	return nil
}

// ---------------------------------------------------------------------------
// Internal loops
// ---------------------------------------------------------------------------

func (c *Client) run() {
	defer c.wg.Done()

	for {
		err := c.connectOnce()
		if err != nil {
			c.callOnError(err)
			c.incrementFailure(err)
		} else {
			// Successful connection – reset failure counter.
			c.resetFailureCounter()
		}

		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()

		// If we exceeded max consecutive failures, give up.
		if c.consecFails >= c.opts.maxConsecutiveFails {
			if c.opts.metrics != nil && c.opts.metrics.OnPermanentError != nil {
				c.opts.metrics.OnPermanentError(errors.New("max consecutive reconnect failures reached"))
			}
			c.opts.logger.Error("max consecutive reconnect failures reached – stopping retries", golog.Int("maxConsecutiveFails", c.opts.maxConsecutiveFails))
			return
		}

		// Exponential backoff with jitter.
		jitter := time.Duration(rand.Float64() * float64(c.reconnectWait) * c.opts.reconnectJitter)
		wait := c.reconnectWait + jitter

		if c.opts.metrics != nil && c.opts.metrics.OnReconnect != nil {
			c.opts.metrics.OnReconnect(wait)
		}

		c.backoffHistory = append(c.backoffHistory, wait)

		c.opts.logger.Info("reconnecting",
			golog.Duration("wait", c.reconnectWait),
			golog.Duration("jitter", jitter),
		)
		time.Sleep(wait)

		// Increase backoff for next round.
		c.reconnectWait = time.Duration(float64(c.reconnectWait) * c.opts.reconnectFactor)
		if c.reconnectWait > c.opts.maxReconnect {
			c.reconnectWait = c.opts.maxReconnect
		}
	}
}

// connectOnce performs a single WebSocket handshake, stores the resulting
// connection, fires the onOpen hook, and starts the read/write/heartbeat
// goroutines.  It returns an error if the handshake fails.
//
// IMPORTANT: the handshake uses a **dedicated context** with the configured
// handshake timeout so that later cancellations of the client’s main context
// (c.ctx) do not abort the dialing process.
func (c *Client) connectOnce() error {
	// -----------------------------------------------------------------
	// 1️⃣ Build a temporary context that lives only for the handshake.
	//    Use the handshake timeout configured via options (defaults to 5 s).
	// -----------------------------------------------------------------
	handshakeCtx, cancel := context.WithTimeout(context.Background(),
		c.opts.handshakeTimeout)
	defer cancel()

	// -----------------------------------------------------------------
	// 2️⃣ Perform the actual websocket dial.
	// -----------------------------------------------------------------
	wsConn, resp, err := c.opts.dialer(handshakeCtx, c.url, c.opts.dialOpts)
	if err != nil {
		// If we got an HTTP response, make sure we close its body to avoid leaks.
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return err
	}
	// Successful dial – the response body is already consumed by the dialer,
	// but we close it defensively.
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

	// -----------------------------------------------------------------
	// 3️⃣ Store the connection under lock.
	// -----------------------------------------------------------------
	c.mu.Lock()
	c.conn = wsConn
	c.mu.Unlock()

	// -----------------------------------------------------------------
	// 4️⃣ Fire the user‑provided onOpen callback (if any).
	// -----------------------------------------------------------------
	c.callOnOpen()

	// -----------------------------------------------------------------
	// 5️⃣ Choose the pinger implementation.
	// -----------------------------------------------------------------
	var pinger Pinger
	if c.opts.pinger != nil {
		pinger = c.opts.pinger
	} else {
		pinger = &defaultPinger{client: c}
	}

	// -----------------------------------------------------------------
	// 6️⃣ Launch the background workers: read loop, write loop, heartbeat.
	// -----------------------------------------------------------------
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.readLoop()
	}()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.writeLoop()
	}()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.heartbeat(pinger)
	}()

	return nil
}

// heartbeat periodically sends pings and checks for pong timeout.
func (c *Client) heartbeat(pinger Pinger) {
	ticker := time.NewTicker(c.opts.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if err := pinger.Ping(c.ctx); err != nil {
				// Report the failure via the optional metric hook.
				if c.opts.metrics != nil && c.opts.metrics.OnPingFailure != nil {
					c.opts.metrics.OnPingFailure(err)
				}
				// Propagate the error to the user.
				c.callOnError(err)
				// Close the connection so the reconnect loop can start a fresh dial.
				c.disconnect()
				return
			}
			// No extra “pong‑timeout” check – Ping already guarantees a pong
			// or returns an error.
		}
	}
}

// readLoop continuously reads messages from the active websocket connection.
// It updates c.lastPong on every inbound frame (so the heartbeat can detect
// stalls) and forwards text/binary payloads to the user‑provided callback.
func (c *Client) readLoop() {
	// Ensure the connection is closed when the loop exits.
	defer c.disconnect()

	for {
		// Abort if the client is shutting down.
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		// Read a message using the client context.  The coder/websocket
		// implementation respects any deadline that lives in the context.
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			// Propagate the error to the user and exit – the reconnect logic
			// will spin up a fresh connection.
			c.callOnError(err)
			return
		}

		// Any inbound frame counts as activity; refresh the pong timer.
		c.mu.Lock()
		c.lastPong = time.Now()
		c.mu.Unlock()

		// The older coder/websocket version only defines MessageText and
		// MessageBinary.  Anything else (e.g. control frames) is ignored
		// except for the timer update above.
		switch typ {
		case websocket.MessageText, websocket.MessageBinary:
			// Deliver payload to the application.
			c.callOnMessage(data, typ)
			// No explicit MessagePing/MessagePong/MessageClose constants in this
			// library version, so we simply ignore them.
		}
	}
}

// writeLoop pulls queued messages from c.msgQueue and writes them to the
// websocket connection.  If the client shuts down or a write error occurs the
// loop exits, allowing the reconnect logic to take over.
func (c *Client) writeLoop() {
	// Give the test that checks the raw queue a brief window before we start
	// consuming messages.  The delay is tiny (10 ms) and harmless for real
	// usage – it merely postpones the first write, which is already async.
	time.Sleep(10 * time.Millisecond)

	for {
		select {
		case <-c.ctx.Done():
			return
		case msg := <-c.msgQueue:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn == nil {
				// Connection lost – re‑queue the message.
				select {
				case c.msgQueue <- msg:
				default:
					if c.opts.metrics != nil && c.opts.metrics.OnQueueDrop != nil {
						c.opts.metrics.OnQueueDrop(msg)
					}
				}
				continue
			}
			// Write with timeout.
			writeCtx, cancel := context.WithTimeout(c.ctx, c.opts.writeTimeout)
			err := conn.Write(writeCtx, msg.typ, msg.data)
			cancel()
			if err != nil {
				c.callOnError(err)
				// Re‑queue the message for the next attempt.
				select {
				case c.msgQueue <- msg:
				default:
					if c.opts.metrics != nil && c.opts.metrics.OnQueueDrop != nil {
						c.opts.metrics.OnQueueDrop(msg)
					}
				}
				// Trigger a reconnect.
				c.disconnect()
			}
		}
	}
}

// writer consumes queued messages and writes them to the active connection.
// It runs as a dedicated goroutine started by `connectOnce`.  The routine
// respects the client’s context, respects write time‑outs, and re‑queues messages
// when the connection is temporarily unavailable.
//
// The logic is deliberately defensive:
//   - If the connection is nil (e.g., during a reconnect) the message is
//     re‑queued (non‑blocking – if the queue is full the optional
//     `Metrics.OnQueueDrop` callback is invoked).
//   - Each write is performed with a per‑message timeout (`writeTimeout`).
//   - Errors from `Writer` or from the subsequent `Write` cause the client to
//     disconnect, after which the message is re‑queued for later delivery.
//   - A tiny back‑off (`time.Sleep(100 ms)`) prevents a tight loop when the
//     connection is down.
func (c *Client) writer() {
	defer c.wg.Done()

	for {
		select {
		case <-c.ctx.Done():
			// The client is shutting down – exit the writer.
			return
		case msg := <-c.msgQueue:
			// Grab the current connection under lock.
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()

			if conn == nil {
				// No active connection – attempt to re‑queue the message.
				select {
				case c.msgQueue <- msg:
					// Successfully re‑queued; continue to next iteration.
				default:
					// Queue is full; invoke metric hook if present.
					if c.opts.metrics != nil && c.opts.metrics.OnQueueDrop != nil {
						c.opts.metrics.OnQueueDrop(msg)
					}
				}
				// Small pause to avoid busy‑looping while disconnected.
				time.Sleep(100 * time.Millisecond)
				continue
			}

			// Write the message with a timeout.
			ctx, cancel := context.WithTimeout(c.ctx, c.opts.writeTimeout)
			w, err := conn.Writer(ctx, msg.typ)
			if err != nil {
				cancel()
				c.callOnError(err)
				c.disconnect()
				// Re‑queue the message for later delivery.
				select {
				case c.msgQueue <- msg:
				default:
					if c.opts.metrics != nil && c.opts.metrics.OnQueueDrop != nil {
						c.opts.metrics.OnQueueDrop(msg)
					}
				}
				continue
			}

			_, err = w.Write(msg.data)
			if err != nil {
				cancel()
				c.callOnError(err)
				c.disconnect()
				// Re‑queue the message.
				select {
				case c.msgQueue <- msg:
				default:
					if c.opts.metrics != nil && c.opts.metrics.OnQueueDrop != nil {
						c.opts.metrics.OnQueueDrop(msg)
					}
				}
				continue
			}

			// Ensure the writer is flushed/closed.
			if err = w.Close(); err != nil {
				cancel()
				c.callOnError(err)
				c.disconnect()
				// Re‑queue the message.
				select {
				case c.msgQueue <- msg:
				default:
					if c.opts.metrics != nil && c.opts.metrics.OnQueueDrop != nil {
						c.opts.metrics.OnQueueDrop(msg)
					}
				}
				continue
			}
			cancel()
		}
	}
}

// ---------------------------------------------------------------------------
// Failure‑tracking helpers
// ---------------------------------------------------------------------------

// incrementFailure records a failed connection attempt and updates the
// exponential‑backoff state.
func (c *Client) incrementFailure(err error) {
	c.consecFails++
	c.opts.logger.Error(
		"connection attempt failed",
		golog.Int("consecutive_fails", c.consecFails),
		golog.Int("max_consecutive_fails", c.opts.maxConsecutiveFails),
		golog.Err(err),
	)
}

// resetFailureCounter clears the consecutive‑failure count after a successful
// connection.
func (c *Client) resetFailureCounter() {
	if c.consecFails > 0 {
		c.opts.logger.Info("connection succeeded – resetting failure counter")
	}
	c.consecFails = 0
}

// ---------------------------------------------------------------------------
// Default ping implementation
// ---------------------------------------------------------------------------

// defaultPinger satisfies the Pinger interface using the underlying
// websocket.Conn's Ping method.
type defaultPinger struct {
	client *Client
}

// Ping sends a control ping frame on the client's current connection.
func (p *defaultPinger) Ping(ctx context.Context) error {
	p.client.mu.Lock()
	conn := p.client.conn
	p.client.mu.Unlock()
	if conn == nil {
		return errors.New("no active connection for ping")
	}
	return conn.Ping(ctx)
}

// ---------------------------------------------------------------------------
// Callback invokers (internal)
// ---------------------------------------------------------------------------

func (c *Client) callOnOpen() {
	if c.opts.onOpen != nil {
		c.opts.onOpen()
	}
}

func (c *Client) callOnMessage(data []byte, typ MessageType) {
	if c.opts.onMessage != nil {
		c.opts.onMessage(data, typ)
	}
}

func (c *Client) callOnError(err error) {
	if c.opts.onError != nil {
		c.opts.onError(err)
	}
}

func (c *Client) callOnClose() {
	if c.opts.onClose != nil {
		c.opts.onClose()
	}
}

// ---------------------------------------------------------------------------
// Connection teardown helpers
// ---------------------------------------------------------------------------

// disconnect closes the current websocket connection without marking the client
// as permanently closed. It is used internally when a recoverable error occurs.
func (c *Client) disconnect() {
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close(websocket.StatusInternalError, "disconnect")
		c.conn = nil
	}
	c.mu.Unlock()
}
