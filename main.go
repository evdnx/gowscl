package gowscl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	OnReconnect      func(wait time.Duration)
	OnQueueDrop      func(msg queuedMessage)
	OnPingFailure    func(err error)
	OnPermanentError func(err error)
}

// ---------------------------------------------------------------------------
// Default values
// ---------------------------------------------------------------------------

const (
	DefaultInitialReconnectInterval = 1 * time.Second
	DefaultMaxReconnectInterval     = 30 * time.Second
	DefaultReconnectFactor          = 2.0
	DefaultReconnectJitter          = 0.5
	DefaultPingInterval             = 30 * time.Second
	DefaultPongTimeout              = 10 * time.Second
	DefaultWriteTimeout             = 10 * time.Second
	DefaultReadTimeout              = 10 * time.Second
	DefaultHandshakeTimeout         = 5 * time.Second
	DefaultMessageQueueSize         = 100
	DefaultCloseGracePeriod         = 5 * time.Second
	DefaultMaxConsecutiveFailures   = 10
)

// ErrClosed indicates the client is closed.
var ErrClosed = errors.New("client closed")

var errQueueFull = errors.New("message queue full")

// ---------------------------------------------------------------------------
// Core structs
// ---------------------------------------------------------------------------

// Client is a robust WebSocket client with auto-reconnect and advanced features.
type Client struct {
	url    string
	opts   *clientOptions
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	conn       *websocket.Conn
	connClosed chan struct{}
	closed     bool

	msgQueue chan queuedMessage
	wg       sync.WaitGroup

	reconnectWait time.Duration
	consecFails   int

	rng *rand.Rand
}

type queuedMessage struct {
	data []byte
	typ  MessageType
}

type clientOptions struct {
	dialOpts            *websocket.DialOptions
	onOpen              func()
	onMessage           func([]byte, MessageType)
	onError             func(error)
	onClose             func()
	initialReconnect    time.Duration
	maxReconnect        time.Duration
	reconnectFactor     float64
	reconnectJitter     float64
	pingInterval        time.Duration
	pongTimeout         time.Duration
	writeTimeout        time.Duration
	readTimeout         time.Duration
	handshakeTimeout    time.Duration
	messageQueueSize    int
	closeGracePeriod    time.Duration
	logger              *golog.Logger
	subprotocols        []string
	headers             map[string][]string
	compressionEnabled  bool
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

func WithOnOpen(fn func()) ClientOption { return func(o *clientOptions) { o.onOpen = fn } }
func WithOnMessage(fn func([]byte, MessageType)) ClientOption {
	return func(o *clientOptions) { o.onMessage = fn }
}
func WithOnError(fn func(error)) ClientOption { return func(o *clientOptions) { o.onError = fn } }
func WithOnClose(fn func()) ClientOption      { return func(o *clientOptions) { o.onClose = fn } }
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
func WithLogger(l *golog.Logger) ClientOption {
	return func(o *clientOptions) { o.logger = l }
}
func WithSubprotocols(subs ...string) ClientOption {
	return func(o *clientOptions) { o.subprotocols = append([]string(nil), subs...) }
}
func WithHeaders(h map[string][]string) ClientOption {
	return func(o *clientOptions) {
		if h == nil {
			o.headers = nil
			return
		}
		o.headers = make(map[string][]string, len(h))
		for k, v := range h {
			o.headers[k] = append([]string(nil), v...)
		}
	}
}
func WithCompression(enabled bool) ClientOption {
	return func(o *clientOptions) { o.compressionEnabled = enabled }
}
func WithMetrics(m *Metrics) ClientOption {
	return func(o *clientOptions) { o.metrics = m }
}
func WithPinger(p Pinger) ClientOption {
	return func(o *clientOptions) { o.pinger = p }
}
func WithMaxConsecutiveFailures(n int) ClientOption {
	return func(o *clientOptions) { o.maxConsecutiveFails = n }
}
func WithDialer(d func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)) ClientOption {
	return func(o *clientOptions) { o.dialer = d }
}

// ---------------------------------------------------------------------------
// Helper functions for constructing defaults
// ---------------------------------------------------------------------------

func populateDefaults(opts *clientOptions) {
	if opts.initialReconnect <= 0 {
		opts.initialReconnect = DefaultInitialReconnectInterval
	}
	if opts.maxReconnect <= 0 {
		opts.maxReconnect = DefaultMaxReconnectInterval
	}
	if opts.reconnectFactor <= 0 {
		opts.reconnectFactor = DefaultReconnectFactor
	}
	if opts.reconnectJitter < 0 {
		opts.reconnectJitter = DefaultReconnectJitter
	}
	if opts.pingInterval <= 0 {
		opts.pingInterval = DefaultPingInterval
	}
	if opts.pongTimeout <= 0 {
		opts.pongTimeout = DefaultPongTimeout
	}
	if opts.writeTimeout <= 0 {
		opts.writeTimeout = DefaultWriteTimeout
	}
	if opts.readTimeout <= 0 {
		opts.readTimeout = DefaultReadTimeout
	}
	if opts.handshakeTimeout <= 0 {
		opts.handshakeTimeout = DefaultHandshakeTimeout
	}
	if opts.messageQueueSize <= 0 {
		opts.messageQueueSize = DefaultMessageQueueSize
	}
	if opts.closeGracePeriod <= 0 {
		opts.closeGracePeriod = DefaultCloseGracePeriod
	}
	if opts.logger == nil {
		logger, err := golog.NewLogger(golog.WithStdOutProvider(golog.JSONEncoder))
		if err != nil {
			panic(fmt.Errorf("gowscl: failed to construct default logger: %w", err))
		}
		opts.logger = logger
	}
	if opts.maxConsecutiveFails <= 0 {
		opts.maxConsecutiveFails = DefaultMaxConsecutiveFailures
	}
	if opts.dialer == nil {
		opts.dialer = websocket.Dial
	}
}

func buildDialOptions(opts *clientOptions) *websocket.DialOptions {
	dial := &websocket.DialOptions{
		Subprotocols: append([]string(nil), opts.subprotocols...),
	}
	if len(opts.headers) > 0 {
		dial.HTTPHeader = make(http.Header, len(opts.headers))
		for k, v := range opts.headers {
			dial.HTTPHeader[k] = append([]string(nil), v...)
		}
	}
	return dial
}

// ---------------------------------------------------------------------------
// Constructor
// ---------------------------------------------------------------------------

// NewClient creates a new robust WebSocket client. The resulting client is
// safe for concurrent use; call Connect to initiate the connection lifecycle.
func NewClient(url string, options ...ClientOption) *Client {
	opts := &clientOptions{}
	for _, opt := range options {
		opt(opts)
	}
	populateDefaults(opts)
	opts.dialOpts = buildDialOptions(opts)

	ctx, cancel := context.WithCancel(context.Background())

	return &Client{
		url:           url,
		opts:          opts,
		ctx:           ctx,
		cancel:        cancel,
		msgQueue:      make(chan queuedMessage, opts.messageQueueSize),
		reconnectWait: opts.initialReconnect,
		rng:           rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// Connect performs the initial handshake. It returns once the first attempt
// either succeeds or fails, while the reconnect manager continues running in
// the background.
func (c *Client) Connect() error {
	result := make(chan error, 1)

	c.wg.Add(1)
	go c.run(result)

	err := <-result
	return err
}

// Close gracefully shuts down the client, waiting indefinitely for all
// goroutines to exit.
func (c *Client) Close() {
	c.CloseWithTimeout(context.Background())
}

// CloseWithTimeout shuts down the client while honouring the provided context.
// If the context expires, the method returns immediately.
func (c *Client) CloseWithTimeout(parent context.Context) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	c.cancel()

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		c.disconnectConn(conn, websocket.StatusNormalClosure, "client closing")
	}

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	graceTimer := time.NewTimer(c.opts.closeGracePeriod)
	defer graceTimer.Stop()

	select {
	case <-done:
	case <-parent.Done():
	case <-graceTimer.C:
	}

	c.callOnClose()
}

// Send queues a message for delivery. Returns ErrClosed if the client is
// closed, or errQueueFull if the outbound queue is full.
func (c *Client) Send(data []byte, typ MessageType) error {
	return c.enqueue(queuedMessage{data: append([]byte(nil), data...), typ: typ})
}

// SendJSON marshals v to JSON and enqueues the payload as a text frame.
func (c *Client) SendJSON(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.enqueue(queuedMessage{data: data, typ: websocket.MessageText})
}

// ---------------------------------------------------------------------------
// Internal control loop
// ---------------------------------------------------------------------------

func (c *Client) run(result chan<- error) {
	defer c.wg.Done()

	firstAttempt := true

	for {
		if c.ctx.Err() != nil {
			if firstAttempt {
				result <- ErrClosed
				close(result)
				firstAttempt = false
			}
			return
		}

		err := c.connectOnce()
		if err != nil {
			c.callOnError(err)
			c.incrementFailure(err)

			if firstAttempt {
				result <- err
				close(result)
				firstAttempt = false
			}

			wait, jitter := c.nextBackoff()
			c.emitReconnect(wait, jitter)

			if c.consecFails >= c.opts.maxConsecutiveFails {
				c.handlePermanentError(err)
				return
			}

			if !c.sleepWithContext(wait) {
				return
			}
			c.growBackoff()
			continue
		}

		c.resetAfterSuccess()

		if firstAttempt {
			result <- nil
			close(result)
			firstAttempt = false
		}

		if !c.waitForDisconnect() {
			return
		}
	}
}

func (c *Client) connectOnce() error {
	handshakeCtx, cancel := context.WithTimeout(c.ctx, c.opts.handshakeTimeout)
	defer cancel()

	conn, resp, err := c.opts.dialer(handshakeCtx, c.url, c.opts.dialOpts)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.connClosed = make(chan struct{})
	done := c.connClosed
	c.mu.Unlock()

	c.callOnOpen()

	var pinger Pinger
	if c.opts.pinger != nil {
		pinger = c.opts.pinger
	} else {
		pinger = &defaultPinger{conn: conn}
	}

	c.wg.Add(3)
	go func() {
		defer c.wg.Done()
		c.readLoop(conn, done)
	}()
	go func() {
		defer c.wg.Done()
		c.writeLoop(conn, done)
	}()
	go func() {
		defer c.wg.Done()
		c.heartbeat(pinger, conn, done)
	}()

	return nil
}

func (c *Client) waitForDisconnect() bool {
	c.mu.Lock()
	ch := c.connClosed
	c.mu.Unlock()

	if ch == nil {
		return true
	}

	select {
	case <-c.ctx.Done():
		return false
	case <-ch:
		return true
	}
}

func (c *Client) nextBackoff() (time.Duration, time.Duration) {
	jitter := time.Duration(0)
	if c.opts.reconnectJitter > 0 {
		jitter = time.Duration(c.rng.Float64() *
			float64(c.reconnectWait) * c.opts.reconnectJitter)
	}
	return c.reconnectWait + jitter, jitter
}

func (c *Client) emitReconnect(wait, jitter time.Duration) {
	if c.opts.metrics != nil && c.opts.metrics.OnReconnect != nil {
		c.opts.metrics.OnReconnect(wait)
	}
	c.opts.logger.Info(
		"reconnect scheduled",
		golog.Duration("wait", wait),
		golog.Duration("jitter", jitter),
		golog.Int("consecutive_fails", c.consecFails),
	)
}

func (c *Client) sleepWithContext(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-c.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *Client) growBackoff() {
	next := time.Duration(float64(c.reconnectWait) * c.opts.reconnectFactor)
	if next > c.opts.maxReconnect {
		next = c.opts.maxReconnect
	}
	c.reconnectWait = next
}

func (c *Client) resetAfterSuccess() {
	if c.consecFails > 0 {
		c.opts.logger.Info("connection re-established, resetting failure counter")
	}
	c.consecFails = 0
	c.reconnectWait = c.opts.initialReconnect
}

func (c *Client) handlePermanentError(lastErr error) {
	err := fmt.Errorf("max consecutive reconnect failures reached (%d): %w",
		c.opts.maxConsecutiveFails, lastErr)
	if c.opts.metrics != nil && c.opts.metrics.OnPermanentError != nil {
		c.opts.metrics.OnPermanentError(err)
	}
	c.opts.logger.Error(
		"max consecutive reconnect failures reached – stopping retries",
		golog.Int("consecutive_fails", c.consecFails),
		golog.Err(err),
	)
}

func (c *Client) disconnectConn(conn *websocket.Conn, status StatusCode, reason string) {
	if conn == nil {
		return
	}

	_ = conn.Close(status, reason)

	c.mu.Lock()
	if c.conn == conn {
		if c.connClosed != nil {
			close(c.connClosed)
		}
		c.conn = nil
		c.connClosed = nil
	}
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Goroutines for a single connection
// ---------------------------------------------------------------------------

func (c *Client) readLoop(conn *websocket.Conn, done <-chan struct{}) {
	defer c.disconnectConn(conn, websocket.StatusInternalError, "read loop exit")

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-done:
			return
		default:
		}

		readCtx, cancel := context.WithTimeout(c.ctx, c.opts.readTimeout)
		typ, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			if status := websocket.CloseStatus(err); status == websocket.StatusNormalClosure {
				return
			}
			c.callOnError(err)
			return
		}

		switch typ {
		case websocket.MessageText, websocket.MessageBinary:
			c.callOnMessage(data, typ)
		}
	}
}

func (c *Client) writeLoop(conn *websocket.Conn, done <-chan struct{}) {
	defer c.disconnectConn(conn, websocket.StatusInternalError, "write loop exit")

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-done:
			return
		case msg := <-c.msgQueue:
			writeCtx, cancel := context.WithTimeout(c.ctx, c.opts.writeTimeout)
			err := conn.Write(writeCtx, msg.typ, msg.data)
			cancel()
			if err != nil {
				c.callOnError(err)
				c.requeue(msg)
				return
			}
		}
	}
}

func (c *Client) heartbeat(pinger Pinger, conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(c.opts.pingInterval)
	defer ticker.Stop()
	defer c.disconnectConn(conn, websocket.StatusInternalError, "heartbeat exit")

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(c.ctx, c.opts.pongTimeout)
			err := pinger.Ping(pingCtx)
			cancel()
			if err != nil {
				if c.opts.metrics != nil && c.opts.metrics.OnPingFailure != nil {
					c.opts.metrics.OnPingFailure(err)
				}
				c.callOnError(err)
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

func (c *Client) enqueue(msg queuedMessage) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}

	select {
	case <-c.ctx.Done():
		return ErrClosed
	default:
	}

	select {
	case c.msgQueue <- msg:
		return nil
	default:
		if c.opts.metrics != nil && c.opts.metrics.OnQueueDrop != nil {
			c.opts.metrics.OnQueueDrop(msg)
		}
		return errQueueFull
	}
}

func (c *Client) requeue(msg queuedMessage) {
	select {
	case c.msgQueue <- msg:
	default:
		if c.opts.metrics != nil && c.opts.metrics.OnQueueDrop != nil {
			c.opts.metrics.OnQueueDrop(msg)
		}
	}
}

func (c *Client) incrementFailure(err error) {
	c.consecFails++
	c.opts.logger.Error(
		"connection attempt failed",
		golog.Int("consecutive_fails", c.consecFails),
		golog.Int("max_consecutive_fails", c.opts.maxConsecutiveFails),
		golog.Err(err),
	)
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
// Default ping implementation
// ---------------------------------------------------------------------------

type defaultPinger struct {
	conn *websocket.Conn
}

func (p *defaultPinger) Ping(ctx context.Context) error {
	if p.conn == nil {
		return errors.New("no active connection for ping")
	}
	return p.conn.Ping(ctx)
}
