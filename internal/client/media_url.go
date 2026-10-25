package client

import (
	"context"
	"fmt"
	"os"

	"go.mau.fi/whatsmeow/types"

	"github.com/sealjay/mcp-whatsapp/internal/media"
)

// prepareSendMedia runs the send throttle before consuming a one-time URL.
// Connection and recipient checks happen before this method is reached.
func (c *Client) prepareSendMedia(ctx context.Context, recipient types.JID, mediaPath, sourceURL string) (string, func(), error) {
	if err := c.checkSendRate(ctx, recipient); err != nil {
		return "", nil, err
	}
	if sourceURL == "" {
		return mediaPath, func() {}, nil
	}
	return c.stageMediaURL(ctx, sourceURL)
}

// stageMediaURL confines temporary downloads to the same root as media_path.
func (c *Client) stageMediaURL(ctx context.Context, sourceURL string) (string, func(), error) {
	if c.allowedMediaRoot == "" {
		return "", nil, fmt.Errorf("media root is not configured")
	}
	if err := os.MkdirAll(c.allowedMediaRoot, 0700); err != nil {
		return "", nil, fmt.Errorf("media root is unavailable")
	}
	return media.DownloadURL(ctx, sourceURL, c.allowedMediaRoot)
}
