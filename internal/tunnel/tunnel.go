package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
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
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64-encoded
}

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
		go func(req tunnelRequest) {
			resp := c.forwardToLocal(localClient, localBase, &req)
			c.mu.Lock()
			writeErr := c.conn.WriteJSON(resp)
			c.mu.Unlock()
			if writeErr != nil {
				log.Printf("tunnel: WebSocket write error: %v", writeErr)
			}
		}(req)
	}
}

// forwardToLocal sends a tunneled request to the local Branch HTTP server.
func (c *Client) forwardToLocal(client *http.Client, localBase string, req *tunnelRequest) *tunnelResponse {
	bodyBytes, err := base64.StdEncoding.DecodeString(req.Body)
	if err != nil {
		log.Printf("tunnel: bad base64 body for %s: %v", req.ID, err)
		return &tunnelResponse{ID: req.ID, Status: 400, Headers: map[string]string{}, Body: ""}
	}
	localURL := localBase + req.Path

	httpReq, err := http.NewRequest(req.Method, localURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return &tunnelResponse{ID: req.ID, Status: 502, Headers: map[string]string{}, Body: ""}
	}

	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	// Mark request as arriving via the public tunnel so the Branch
	// HTTP server can restrict local-only endpoints.
	httpReq.Header.Set("X-Mayberry-Via-Tunnel", "true")

	resp, err := client.Do(httpReq)
	if err != nil {
		log.Printf("tunnel: local forward error: %v", err)
		return &tunnelResponse{ID: req.ID, Status: 502, Headers: map[string]string{}, Body: ""}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("tunnel: read local response: %v", err)
		return &tunnelResponse{ID: req.ID, Status: 502, Headers: map[string]string{}, Body: ""}
	}

	headers := make(map[string]string)
	for k, vals := range resp.Header {
		headers[k] = strings.Join(vals, ", ")
	}

	return &tunnelResponse{
		ID:      req.ID,
		Status:  resp.StatusCode,
		Headers: headers,
		Body:    base64.StdEncoding.EncodeToString(respBody),
	}
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
