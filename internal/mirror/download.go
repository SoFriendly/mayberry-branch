package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Per-file caps. Mirror clients reject anything larger before they even
// start streaming bytes. Values match MIRROR.md security section 2.
const (
	MaxEPUBBytes      int64 = 100 * 1024 * 1024       // 100 MB
	MaxAudiobookBytes int64 = 2 * 1024 * 1024 * 1024  // 2 GB
)

// httpClient is the mirror manager's outbound HTTP client. Timeouts are
// generous enough for slow rural sources but bounded so a stalled peer
// can't tie up the download goroutine forever.
var httpClient = &http.Client{
	Timeout: 30 * time.Minute, // upper bound for the whole transfer
	Transport: &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		DisableKeepAlives:     false,
	},
}

// DownloadResult is what a successful download returns. SHA256 is the
// hex digest of the bytes we received (not what the source claimed).
type DownloadResult struct {
	Size   int64
	SHA256 string
}

// Download fetches url and writes bytes to dst, enforcing maxBytes and
// computing the SHA-256 as it streams. bandwidthBps caps the byte rate
// (best-effort, per-write sleep); pass 0 to disable.
//
// The X-Mayberry-Mirror header lets source branches identify mirror
// traffic and de-prioritize it relative to real user downloads (Phase 4
// implements the source side; sending the header now is harmless and
// keeps deployments compatible).
//
// On size-cap violation (either announced Content-Length or actual bytes
// streamed) we abort with an error — partially-written staging bytes
// remain for the caller to clean up.
func Download(ctx context.Context, url string, maxBytes int64, bandwidthBps int64, dst io.Writer) (DownloadResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("mirror: request: %w", err)
	}
	req.Header.Set("X-Mayberry-Mirror", "1")

	resp, err := httpClient.Do(req)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("mirror: do: %w", err)
	}
	defer resp.Body.Close()

	// 503 from the source means it's busy serving real users (Phase 4
	// throttling). Not an error worth alarming about — the manager's
	// next tick will try a different candidate, and the rate-preset
	// interval is already longer than the source's Retry-After.
	if resp.StatusCode == http.StatusServiceUnavailable {
		retry := resp.Header.Get("Retry-After")
		return DownloadResult{}, fmt.Errorf("mirror: source busy (503, retry-after %q) — skipping this tick", retry)
	}
	if resp.StatusCode != http.StatusOK {
		return DownloadResult{}, fmt.Errorf("mirror: http %d from source", resp.StatusCode)
	}

	// Pre-flight size check via Content-Length. Sources can lie here, so
	// the actual size is also enforced byte-by-byte during streaming.
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if announced, err := strconv.ParseInt(cl, 10, 64); err == nil {
			if announced < 0 || announced > maxBytes {
				return DownloadResult{}, fmt.Errorf("mirror: announced size %d exceeds cap %d", announced, maxBytes)
			}
		}
	}

	hash := sha256.New()
	// MultiWriter so every byte we accept flows into both the staging file
	// and the running hash. If either errors we abort.
	dual := io.MultiWriter(dst, hash)

	written, err := copyCapped(ctx, dual, resp.Body, maxBytes, bandwidthBps)
	if err != nil {
		return DownloadResult{}, err
	}
	return DownloadResult{
		Size:   written,
		SHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

// copyCapped streams src to dst until EOF or maxBytes is exceeded. When
// bandwidthBps > 0 it sleeps between writes to approximate that rate.
//
// The rate limiter is intentionally simple: after each chunk we compute
// how long the chunk "should have taken" at bandwidthBps and sleep the
// difference. Sufficient for being-polite-to-the-source; not a hard
// guarantee.
func copyCapped(ctx context.Context, dst io.Writer, src io.Reader, maxBytes, bandwidthBps int64) (int64, error) {
	const chunkSize = 32 * 1024
	buf := make([]byte, chunkSize)
	var written int64
	for {
		if ctx.Err() != nil {
			return written, ctx.Err()
		}
		started := time.Now()
		n, rerr := src.Read(buf)
		if n > 0 {
			if written+int64(n) > maxBytes {
				return written, fmt.Errorf("mirror: stream exceeded cap %d bytes", maxBytes)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return written, fmt.Errorf("mirror: write: %w", werr)
			}
			written += int64(n)
		}
		if bandwidthBps > 0 && n > 0 {
			wantElapsed := time.Duration(float64(n) / float64(bandwidthBps) * float64(time.Second))
			if extra := wantElapsed - time.Since(started); extra > 0 {
				select {
				case <-ctx.Done():
					return written, ctx.Err()
				case <-time.After(extra):
				}
			}
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, fmt.Errorf("mirror: read: %w", rerr)
		}
	}
}
