package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// tunnelRequest is received from the hub over the WebSocket.
type tunnelRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64-encoded
}

// tunnelResponse is sent back to the hub over the WebSocket.
type tunnelResponse struct {
	ID      string            `json:"id"`
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`    // base64 (legacy)
	Chunked bool              `json:"chunked,omitempty"` // streaming mode
	Chunk   string            `json:"chunk,omitempty"`   // base64 chunk
	Done    bool              `json:"done,omitempty"`    // end of stream
}

const chunkSize = 256 * 1024 // 256KB chunks

// TokenRefreshFunc is called to get a fresh tunnel token when reconnecting.
type TokenRefreshFunc func() string

// Client manages a WebSocket reverse tunnel connection to the Proxy Hub.
type Client struct {
	subdomain    string
	localPort    int
	hubURL       string
	token        string
	refreshToken TokenRefreshFunc
	conn         *websocket.Conn
	mu           sync.Mutex // protects conn writes
	cancelFunc   context.CancelFunc
}

// NewClient creates a tunnel client that will connect the local Branch
// server to the branch.pub proxy hub via WebSocket.
func NewClient(subdomain string, localPort int, hubURL, token string, refresh TokenRefreshFunc) *Client {
	return &Client{
		subdomain:    subdomain,
		localPort:    localPort,
		hubURL:       hubURL,
		token:        token,
		refreshToken: refresh,
	}
}

// buildWSURL constructs the WebSocket URL from the hub HTTP URL.
func (c *Client) buildWSURL() string {
	wsURL := strings.Replace(c.hubURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	params := url.Values{
		"subdomain": {c.subdomain},
		"port":      {fmt.Sprintf("%d", c.localPort)},
	}
	if c.token != "" {
		params.Set("token", c.token)
	}
	return wsURL + "/api/tunnels/connect?" + params.Encode()
}

// Connect establishes a WebSocket reverse tunnel to the Proxy Hub.
// It dials the hub's /api/tunnels/connect endpoint, then loops reading
// incoming HTTP requests, forwarding them to localhost, and returning
// responses over the WebSocket.
func (c *Client) Connect(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	c.cancelFunc = cancel

	wsURL := c.buildWSURL()
	log.Printf("tunnel: connecting WebSocket to hub")

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		log.Printf("tunnel: connecting (will retry in background)")
		go c.reconnectLoop(ctx, wsURL)
		return nil
	}

	c.conn = conn
	log.Printf("tunnel: WebSocket connected as %s.branch.pub", c.subdomain)

	go c.pingLoop(ctx)
	go c.readLoop(ctx, wsURL)
	return nil
}

// pingLoop sends WebSocket pings to keep the connection alive through proxies.
func (c *Client) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			err := c.conn.WriteMessage(websocket.PingMessage, nil)
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// readLoop reads tunnel requests from the WebSocket and forwards them to the local server.
func (c *Client) readLoop(ctx context.Context, wsURL string) {
	defer func() {
		if c.conn != nil {
			c.conn.Close()
		}
	}()

	localBase := fmt.Sprintf("http://localhost:%d", c.localPort)
	localClient := &http.Client{Timeout: 120 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var req tunnelRequest
		if err := c.conn.ReadJSON(&req); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("tunnel: connection lost, reconnecting...")
			// Connection lost — attempt reconnect (blocks until success or ctx cancel).
			c.reconnectLoop(ctx, wsURL)
			return
		}

		// Forward the request to the local Branch HTTP server.
		go c.forwardToLocal(localClient, localBase, req)
	}
}

func (c *Client) writeResponse(resp *tunnelResponse) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(resp)
}

// forwardToLocal sends a tunneled request to the local Branch HTTP server
// and streams the response back through the WebSocket in chunks.
func (c *Client) forwardToLocal(client *http.Client, localBase string, req tunnelRequest) {
	bodyBytes, err := base64.StdEncoding.DecodeString(req.Body)
	if err != nil {
		c.writeResponse(&tunnelResponse{ID: req.ID, Status: 400})
		return
	}

	httpReq, err := http.NewRequest(req.Method, localBase+req.Path, bytes.NewReader(bodyBytes))
	if err != nil {
		c.writeResponse(&tunnelResponse{ID: req.ID, Status: 502})
		return
	}

	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("X-Mayberry-Via-Tunnel", "true")

	resp, err := client.Do(httpReq)
	if err != nil {
		c.writeResponse(&tunnelResponse{ID: req.ID, Status: 502})
		return
	}
	defer resp.Body.Close()

	headers := make(map[string]string)
	for k, vals := range resp.Header {
		headers[k] = strings.Join(vals, ", ")
	}

	// Send headers first.
	if err := c.writeResponse(&tunnelResponse{
		ID:      req.ID,
		Status:  resp.StatusCode,
		Headers: headers,
		Chunked: true,
	}); err != nil {
		return
	}

	// Stream body in chunks.
	buf := make([]byte, chunkSize)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if err := c.writeResponse(&tunnelResponse{
				ID:    req.ID,
				Chunk: base64.StdEncoding.EncodeToString(buf[:n]),
			}); err != nil {
				return
			}
		}
		if readErr != nil {
			break
		}
	}

	// Signal end of stream.
	c.writeResponse(&tunnelResponse{ID: req.ID, Done: true})
}

// reconnectLoop attempts to re-establish the WebSocket connection with backoff.
func (c *Client) reconnectLoop(ctx context.Context, wsURL string) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	backoff := 2 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Refresh the tunnel token before reconnecting (it may have expired).
		if c.refreshToken != nil {
			if newToken := c.refreshToken(); newToken != "" {
				c.token = newToken
				wsURL = c.buildWSURL()
			}
		}

		log.Printf("tunnel: reconnecting...")
		conn, _, err := dialer.DialContext(ctx, wsURL, nil)
		if err != nil {
			log.Printf("tunnel: reconnect retry in %s", backoff*2)
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}

		c.conn = conn
		log.Printf("tunnel: reconnected as %s.branch.pub", c.subdomain)
		go c.pingLoop(ctx)
		c.readLoop(ctx, wsURL)
		return
	}
}

// Close tears down the tunnel connection.
func (c *Client) Close() {
	if c.cancelFunc != nil {
		c.cancelFunc()
	}
	if c.conn != nil {
		c.conn.Close()
	}
}
