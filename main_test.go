package gowscl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// In-memory dialer helpers (adapted from coder/websocket/internal/test/wstest)
// -----------------------------------------------------------------------------

type hijackRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

type memoryTransport struct {
	handler func(http.ResponseWriter, *http.Request)
}

func (t memoryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clientConn, serverConn := net.Pipe()

	recorder := httptest.NewRecorder()
	hijacker := &hijackRecorder{ResponseRecorder: recorder, conn: serverConn}
	t.handler(hijacker, r)

	resp := recorder.Result()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		resp.Body = clientConn
	} else {
		_ = clientConn.Close()
		_ = serverConn.Close()
	}
	return resp, nil
}

func dialPipeConn(ctx context.Context, dialOpts *websocket.DialOptions, acceptOpts *websocket.AcceptOptions) (*websocket.Conn, *websocket.Conn, error) {
	serverConnCh := make(chan *websocket.Conn, 1)
	acceptErrCh := make(chan error, 1)

	transport := memoryTransport{
		handler: func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, acceptOpts)
			acceptErrCh <- err
			if err == nil {
				serverConnCh <- conn
			}
		},
	}

	var optsCopy *websocket.DialOptions
	if dialOpts != nil {
		copy := *dialOpts
		optsCopy = &copy
	} else {
		optsCopy = &websocket.DialOptions{}
	}
	optsCopy.HTTPClient = &http.Client{Transport: transport}

	clientConn, _, err := websocket.Dial(ctx, "ws://example.com/test", optsCopy)
	if err != nil {
		return nil, nil, err
	}

	if acceptErr := <-acceptErrCh; acceptErr != nil {
		_ = clientConn.Close(websocket.StatusInternalError, "accept failed")
		return nil, nil, acceptErr
	}

	serverConn := <-serverConnCh
	return clientConn, serverConn, nil
}

func inMemoryDialer(t *testing.T, handler func(context.Context, *websocket.Conn)) func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
	return func(ctx context.Context, _ string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		clientConn, serverConn, err := dialPipeConn(ctx, opts, nil)
		if err != nil {
			return nil, nil, err
		}

		go func() {
			defer serverConn.Close(websocket.StatusNormalClosure, "test server shutdown")
			if handler != nil {
				handler(context.Background(), serverConn)
				return
			}
			for {
				_, reader, err := serverConn.Reader(context.Background())
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, reader)
			}
		}()

		return clientConn, nil, nil
	}
}

// -----------------------------------------------------------------------------
// Option defaults
// -----------------------------------------------------------------------------

func TestNewClient_Defaults(t *testing.T) {
	c := NewClient("ws://example.com")

	assert.Equal(t, DefaultInitialReconnectInterval, c.opts.initialReconnect)
	assert.Equal(t, DefaultMaxReconnectInterval, c.opts.maxReconnect)
	assert.Equal(t, DefaultReconnectFactor, c.opts.reconnectFactor)
	assert.Equal(t, DefaultMaxConsecutiveFailures, c.opts.maxConsecutiveFails)
}

func TestNewClient_CustomOptions(t *testing.T) {
	c := NewClient(
		"ws://example.com",
		WithPingInterval(5*time.Second),
		WithMaxConsecutiveFailures(3),
	)
	assert.Equal(t, 5*time.Second, c.opts.pingInterval)
	assert.Equal(t, 3, c.opts.maxConsecutiveFails)
}

// -----------------------------------------------------------------------------
// Send / queue behaviour
// -----------------------------------------------------------------------------

func TestClient_Send_QueuesWhenDisconnected(t *testing.T) {
	c := NewClient("ws://example.com")
	err := c.Send([]byte("hello"), websocket.MessageText)
	require.NoError(t, err)

	select {
	case qm := <-c.msgQueue:
		assert.Equal(t, []byte("hello"), qm.data)
	default:
		t.Fatalf("expected message to be queued")
	}
}

func TestClient_Send_QueueOverflow(t *testing.T) {
	c := NewClient("ws://example.com", WithMessageQueueSize(2))
	require.NoError(t, c.Send([]byte("a"), websocket.MessageText))
	require.NoError(t, c.Send([]byte("b"), websocket.MessageText))

	err := c.Send([]byte("c"), websocket.MessageText)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "message queue full")
}

// -----------------------------------------------------------------------------
// Successful connection & callbacks
// -----------------------------------------------------------------------------

func TestClient_Connect_SuccessfulHandshake(t *testing.T) {
	echoDialer := inMemoryDialer(t, func(ctx context.Context, conn *websocket.Conn) {
		defer conn.Close(websocket.StatusNormalClosure, "echo done")
		for {
			mt, reader, err := conn.Reader(ctx)
			if err != nil {
				return
			}
			payload, err := io.ReadAll(reader)
			if err != nil {
				return
			}
			writer, err := conn.Writer(ctx, mt)
			if err != nil {
				return
			}
			_, _ = writer.Write(payload)
			_ = writer.Close()
		}
	})

	openCalled, msgCalled, closeCalled := false, false, false

	c := NewClient(
		"ws://example.com/echo",
		WithDialer(echoDialer),
		WithOnOpen(func() { openCalled = true }),
		WithOnMessage(func(data []byte, typ websocket.MessageType) {
			msgCalled = true
			assert.Equal(t, websocket.MessageText, typ)
			assert.Equal(t, []byte("payload"), data)
		}),
		WithOnClose(func() { closeCalled = true }),
		WithPingInterval(50*time.Millisecond),
		WithPongTimeout(25*time.Millisecond),
	)

	require.NoError(t, c.Connect())
	require.NoError(t, c.Send([]byte("payload"), websocket.MessageText))

	time.Sleep(100 * time.Millisecond)
	c.Close()

	assert.True(t, openCalled)
	assert.True(t, msgCalled)
	assert.True(t, closeCalled)
}

// -----------------------------------------------------------------------------
// Reconnect back-off & failure limits
// -----------------------------------------------------------------------------

func TestClient_Reconnect_BackoffStopsAfterMaxFails(t *testing.T) {
	var mu sync.Mutex
	var reconnectDurations []time.Duration
	done := make(chan struct{})
	metrics := &Metrics{
		OnReconnect: func(wait time.Duration) {
			mu.Lock()
			reconnectDurations = append(reconnectDurations, wait)
			mu.Unlock()
		},
		OnPermanentError: func(err error) {
			close(done)
		},
	}

	failingDialer := func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		return nil, nil, errors.New("dial failure")
	}

	c := NewClient(
		"ws://example.com",
		WithMetrics(metrics),
		WithDialer(failingDialer),
		WithMaxConsecutiveFailures(3),
		WithInitialReconnect(10*time.Millisecond),
		WithReconnectFactor(2.0),
		WithReconnectJitter(0.0),
	)

	go func() { _ = c.Connect() }()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for permanent error")
	}

	mu.Lock()
	require.Len(t, reconnectDurations, 3)
	expected := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	assert.Equal(t, expected, reconnectDurations)
	mu.Unlock()
}

// -----------------------------------------------------------------------------
// Heartbeat / ping failure
// -----------------------------------------------------------------------------

type flakyPinger struct {
	callCount int
	failAfter int
}

func (f *flakyPinger) Ping(ctx context.Context) error {
	f.callCount++
	if f.callCount > f.failAfter {
		return errors.New("simulated ping failure")
	}
	return nil
}

func TestClient_Heartbeat_PingFailure(t *testing.T) {
	metrics := &Metrics{}
	var pingFailed atomic.Bool
	var errorCallback atomic.Bool
	metrics.OnPingFailure = func(err error) { pingFailed.Store(true) }
	pinger := &flakyPinger{failAfter: 2}

	c := NewClient(
		"ws://example.com",
		WithDialer(inMemoryDialer(t, nil)),
		WithMetrics(metrics),
		WithPinger(pinger),
		WithOnError(func(err error) { errorCallback.Store(true) }),
		WithPingInterval(20*time.Millisecond),
		WithPongTimeout(10*time.Millisecond),
	)

	require.NoError(t, c.Connect())
	time.Sleep(250 * time.Millisecond)
	require.True(t, pingFailed.Load())
	require.True(t, errorCallback.Load())
	assert.GreaterOrEqual(t, pinger.callCount, 3)

	c.Close()
}

func TestClient_Heartbeat_NoPongTriggersFailure(t *testing.T) {
	var pingFailed atomic.Bool
	var errCb atomic.Bool
	metrics := &Metrics{
		OnPingFailure: func(err error) { pingFailed.Store(true) },
	}
	pinger := &flakyPinger{failAfter: 0}

	c := NewClient(
		"ws://example.com",
		WithDialer(inMemoryDialer(t, nil)),
		WithMetrics(metrics),
		WithOnError(func(err error) { errCb.Store(true) }),
		WithPinger(pinger),
		WithPingInterval(20*time.Millisecond),
		WithPongTimeout(10*time.Millisecond),
	)

	require.NoError(t, c.Connect())
	time.Sleep(200 * time.Millisecond)
	require.True(t, pingFailed.Load())
	require.True(t, errCb.Load())
	assert.GreaterOrEqual(t, pinger.callCount, 1)

	c.Close()
}

// -----------------------------------------------------------------------------
// Graceful shutdown
// -----------------------------------------------------------------------------

func TestClient_CloseWithTimeout_ContextCancellation(t *testing.T) {
	c := NewClient(
		"ws://example.com",
		WithDialer(inMemoryDialer(t, nil)),
	)
	require.NoError(t, c.Connect())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	c.CloseWithTimeout(ctx)
	elapsed := time.Since(start)
	assert.LessOrEqual(t, elapsed, 100*time.Millisecond)

	select {
	case <-c.ctx.Done():
	default:
		t.Fatalf("client context should be cancelled")
	}
}

func TestClient_CloseWithPendingMessages(t *testing.T) {
	c := NewClient(
		"ws://example.com",
		WithDialer(inMemoryDialer(t, nil)),
		WithMessageQueueSize(5),
	)
	require.NoError(t, c.Connect())

	for i := 0; i < 3; i++ {
		assert.NoError(t, c.Send([]byte(fmt.Sprintf("msg-%d", i)), websocket.MessageText))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	c.CloseWithTimeout(ctx)
	elapsed := time.Since(start)

	assert.LessOrEqual(t, elapsed.Milliseconds(), int64(50))
}

func TestClient_CloseGracePeriod(t *testing.T) {
	grace := 50 * time.Millisecond

	c := NewClient(
		"ws://example.com",
		WithDialer(inMemoryDialer(t, nil)),
		WithCloseGracePeriod(grace),
	)
	require.NoError(t, c.Connect())

	start := time.Now()
	c.Close()
	elapsed := time.Since(start)

	assert.LessOrEqual(t, elapsed, grace+50*time.Millisecond)
}

// -----------------------------------------------------------------------------
// SendJSON helper
// -----------------------------------------------------------------------------

func TestClient_SendJSON(t *testing.T) {
	c := NewClient("ws://example.com")

	payload := struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}{"Alice", 30}

	require.NoError(t, c.SendJSON(payload))

	select {
	case qm := <-c.msgQueue:
		var out struct {
			Name string `json:"name"`
			Age  int    `json:"age"`
		}
		require.NoError(t, json.Unmarshal(qm.data, &out))
		assert.Equal(t, payload, out)
	default:
		t.Fatalf("expected queued JSON message")
	}
}

// -----------------------------------------------------------------------------
// Misc configuration tests
// -----------------------------------------------------------------------------

func TestClient_SubprotocolsAndHeaders(t *testing.T) {
	c := NewClient(
		"ws://example.invalid",
		WithSubprotocols("chat", "super"),
		WithHeaders(map[string][]string{
			"X-Custom": {"magic"},
		}),
	)

	assert.Contains(t, c.opts.subprotocols, "chat")
	assert.Contains(t, c.opts.subprotocols, "super")
	assert.Equal(t, []string{"magic"}, c.opts.headers["X-Custom"])
}

func TestClient_CompressionFlagIsStored(t *testing.T) {
	c := NewClient(
		"ws://example.com",
		WithCompression(true),
	)
	assert.True(t, c.opts.compressionEnabled)
}

func TestClient_WriteTimeoutTriggersError(t *testing.T) {
	var errSeen atomic.Bool
	c := NewClient(
		"ws://example.com",
		WithDialer(func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
			clientConn, serverConn, err := dialPipeConn(ctx, opts, nil)
			if err != nil {
				return nil, nil, err
			}
			_ = serverConn.Close(websocket.StatusInternalError, "forced close")
			return clientConn, nil, nil
		}),
		WithWriteTimeout(30*time.Millisecond),
		WithOnError(func(err error) { errSeen.Store(true) }),
	)

	require.NoError(t, c.Connect())
	_ = c.Send([]byte("msg-that-will-fail"), websocket.MessageText)
	time.Sleep(60 * time.Millisecond)

	assert.True(t, errSeen.Load())
	c.Close()
}

func TestClient_MaxConsecutiveFailsStopsRetries(t *testing.T) {
	badDialer := func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		return nil, nil, errors.New("always fail")
	}

	permanentErrCh := make(chan error, 1)
	metrics := &Metrics{
		OnPermanentError: func(err error) { permanentErrCh <- err },
	}

	c := NewClient(
		"ws://invalid",
		WithDialer(badDialer),
		WithMetrics(metrics),
		WithMaxConsecutiveFailures(2),
		WithInitialReconnect(5*time.Millisecond),
		WithReconnectFactor(1.0),
		WithReconnectJitter(0.0),
	)

	go func() { _ = c.Connect() }()

	select {
	case permanentErr := <-permanentErrCh:
		assert.NotNil(t, permanentErr)
		assert.Contains(t, permanentErr.Error(), "max consecutive reconnect failures")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for permanent error")
	}
}

func TestClient_DialerReceivesHeadersAndSubprotocols(t *testing.T) {
	var receivedOpts *websocket.DialOptions

	customDialer := func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		receivedOpts = opts
		clientConn, serverConn, err := dialPipeConn(ctx, opts, nil)
		if err != nil {
			return nil, nil, err
		}
		_ = serverConn.Close(websocket.StatusNormalClosure, "done")
		return clientConn, nil, nil
	}

	c := NewClient(
		"ws://example.com",
		WithDialer(customDialer),
		WithSubprotocols("protoA", "protoB"),
		WithHeaders(map[string][]string{
			"X-Test": {"value"},
		}),
	)

	require.NoError(t, c.Connect())
	require.NotNil(t, receivedOpts)
	assert.ElementsMatch(t, []string{"protoA", "protoB"}, receivedOpts.Subprotocols)
	assert.Equal(t, []string{"value"}, receivedOpts.HTTPHeader["X-Test"])
	c.Close()
}

func TestClient_MetricsQueueDrop(t *testing.T) {
	var droppedMsg queuedMessage
	metrics := &Metrics{
		OnQueueDrop: func(msg queuedMessage) { droppedMsg = msg },
	}

	c := NewClient(
		"ws://example.com",
		WithMessageQueueSize(1),
		WithMetrics(metrics),
	)

	require.NoError(t, c.Send([]byte("first"), websocket.MessageText))
	err := c.Send([]byte("second"), websocket.MessageText)
	assert.Error(t, err)
	assert.Equal(t, []byte("second"), droppedMsg.data)
}

func TestClient_HandshakeTimeout(t *testing.T) {
	slowDialer := func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		select {
		case <-time.After(200 * time.Millisecond):
			return nil, nil, errors.New("handshake took too long")
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}

	c := NewClient(
		"ws://example.com",
		WithHandshakeTimeout(50*time.Millisecond),
		WithDialer(slowDialer),
	)

	err := c.Connect()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "context deadline exceeded")
}
