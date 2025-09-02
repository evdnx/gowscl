package gowscl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// Helper: start a minimal echo WebSocket server for the tests.
// The server upgrades the HTTP request, then reads messages and writes them back.
// It runs in the same process, so the client receives a genuine *websocket.Conn.
// ---------------------------------------------------------------------------
func startTestWSServer(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Upgrade the HTTP connection to a WebSocket.
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("server accept failed: %v", err)
		}
		defer c.Close(websocket.StatusNormalClosure, "")

		// Simple echo loop – read a message, write it back.
		for {
			typ, rdr, err := c.Reader(context.Background())
			if err != nil {
				return
			}
			data, err := io.ReadAll(rdr)
			if err != nil {
				return
			}
			wtr, err := c.Writer(context.Background(), typ)
			if err != nil {
				return
			}
			_, _ = wtr.Write(data)
			_ = wtr.Close()
		}
	}))
	return s
}

// --------------------
// 1. Construction & Defaults
// --------------------
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

// --------------------
// 2. Send / Queue Behaviour
// --------------------
func TestClient_Send_QueuesWhenDisconnected(t *testing.T) {
	c := NewClient("ws://example.com")
	err := c.Send([]byte(`hello`), websocket.MessageText)
	assert.NoError(t, err)

	select {
	case qm := <-c.msgQueue:
		assert.Equal(t, []byte(`hello`), qm.data)
	default:
		t.Fatalf("expected message to be queued")
	}
}

func TestClient_Send_QueueOverflow(t *testing.T) {
	c := NewClient("ws://example.com", WithMessageQueueSize(2))
	_ = c.Send([]byte(`a`), websocket.MessageText)
	_ = c.Send([]byte(`b`), websocket.MessageText)

	err := c.Send([]byte(`c`), websocket.MessageText)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "message queue full")
}

// --------------------
// 3. Successful Connection & Callbacks
// --------------------
func TestClient_Connect_SuccessfulHandshake(t *testing.T) {
	openCalled, msgCalled, closeCalled := false, false, false

	// Spin up a real in‑process WS server.
	srv := startTestWSServer(t)
	defer srv.Close()

	c := NewClient(
		"ws://"+srv.Listener.Addr().String(),
		WithOnOpen(func() { openCalled = true }),
		WithOnMessage(func(data []byte, typ websocket.MessageType) {
			msgCalled = true
			assert.Equal(t, []byte(`payload`), data)
			assert.Equal(t, websocket.MessageText, typ)
		}),
		WithOnClose(func() { closeCalled = true }),
		// Speed things up for the test.
		WithReadTimeout(500*time.Millisecond),
		WithHandshakeTimeout(500*time.Millisecond),
		WithPingInterval(200*time.Millisecond),
		WithPongTimeout(100*time.Millisecond),
	)

	// Start the client.
	err := c.Connect()
	assert.NoError(t, err)

	// Send a message – the server will echo it back, triggering OnMessage.
	err = c.Send([]byte(`payload`), websocket.MessageText)
	assert.NoError(t, err)

	// Allow a short window for the round‑trip.
	time.Sleep(200 * time.Millisecond)

	// Shut down cleanly.
	c.Close()

	assert.True(t, openCalled, "onOpen should have fired")
	assert.True(t, msgCalled, "onMessage should have fired")
	assert.True(t, closeCalled, "onClose should have fired")
}

// --------------------
// 4. Reconnect Back‑off & Max Failures
// --------------------
func TestClient_Reconnect_BackoffStopsAfterMaxFails(t *testing.T) {
	var reconnectDurations []time.Duration
	metrics := &Metrics{
		OnReconnect: func(wait time.Duration) { reconnectDurations = append(reconnectDurations, wait) },
	}

	// Dialer that always fails.
	failingDialer := func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		return nil, nil, errors.New("dial failure")
	}

	// Use a dummy server just to have a valid URL.
	srv := startTestWSServer(t)
	defer srv.Close()

	c := NewClient(
		"ws://"+srv.Listener.Addr().String(),
		WithMetrics(metrics),
		WithDialer(failingDialer),
		WithMaxConsecutiveFailures(3),
		WithInitialReconnect(10*time.Millisecond),
		WithReconnectFactor(2.0),
		WithReconnectJitter(0.0), // deterministic for assertions
	)

	// Run the client in background.
	go func() { _ = c.Connect() }()

	// Wait enough time for three retries to occur.
	time.Sleep(200 * time.Millisecond)

	assert.Len(t, reconnectDurations, 3, "should have attempted three reconnects")
	expected := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	assert.Equal(t, expected, reconnectDurations)
	assert.Equal(t, 3, c.consecFails, "consecutive failure count should equal max")
}

// --------------------
// 5. Heartbeat / Ping Failure
// --------------------
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
	pingFailed := false
	errorCallback := false

	metrics := &Metrics{
		OnPingFailure: func(err error) { pingFailed = true },
	}

	// Server for a real connection.
	srv := startTestWSServer(t)
	defer srv.Close()

	c := NewClient(
		"ws://"+srv.Listener.Addr().String(),
		WithMetrics(metrics),
		WithPinger(&flakyPinger{failAfter: 2}), // fail on third tick
		WithOnError(func(err error) { errorCallback = true }),
		WithPingInterval(20*time.Millisecond),
		WithPongTimeout(10*time.Millisecond),
	)

	_ = c.Connect()
	// Let the heartbeat run a few cycles.
	time.Sleep(150 * time.Millisecond)

	assert.True(t, pingFailed, "OnPingFailure metric should have been invoked")
	assert.True(t, errorCallback, "onError callback should have been invoked")
	c.mu.Lock()
	connNil := c.conn == nil
	c.mu.Unlock()
	assert.True(t, connNil, "connection should have been cleared after ping failure")
}

// --------------------
// 6. Graceful Shutdown with Timeout
// --------------------
func TestClient_CloseWithTimeout_ContextCancellation(t *testing.T) {
	// Server for a real connection.
	srv := startTestWSServer(t)
	defer srv.Close()

	c := NewClient(
		"ws://" + srv.Listener.Addr().String(),
	)

	_ = c.Connect()

	// Cancel after a short deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	c.CloseWithTimeout(ctx)
	elapsed := time.Since(start)

	// Should return quickly once the deadline fires.
	assert.LessOrEqual(t, elapsed, 100*time.Millisecond)

	// Verify internal context is cancelled.
	select {
	case <-c.ctx.Done():
		// ok
	default:
		t.Fatalf("client context should be cancelled")
	}
}

// --------------------
// 7. SendJSON helper
// --------------------
func TestClient_SendJSON(t *testing.T) {
	// Server for a real connection.
	srv := startTestWSServer(t)
	defer srv.Close()

	c := NewClient(
		"ws://" + srv.Listener.Addr().String(),
	)

	// Connect so the writer goroutine is alive.
	_ = c.Connect()

	payload := struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}{"Alice", 30}
	err := c.SendJSON(payload)
	assert.NoError(t, err)

	// Pull the queued message and verify JSON encoding.
	select {
	case qm := <-c.msgQueue:
		var out struct {
			Name string `json:"name"`
			Age  int    `json:"age"`
		}
		err = json.Unmarshal(qm.data, &out)
		assert.NoError(t, err)
		assert.Equal(t, payload, out)
	default:
		t.Fatalf("expected a message to be queued")
	}
}
