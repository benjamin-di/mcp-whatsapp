package mcp

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestSendFileRequiresExactlyOneMediaSource(t *testing.T) {
	s := NewServer(nil, nil)
	tool := s.MCP().ListTools()["send_file"]
	for _, args := range []map[string]any{
		{"recipient": "447700000001"},
		{"recipient": "447700000001", "media_path": "/tmp/image.jpg", "source_url": "https://example.com/image.jpg"},
		{"source_url": "https://example.com/image.jpg"},
	} {
		req := mcp.CallToolRequest{}
		req.Params.Arguments = args
		result, err := tool.Handler(context.Background(), req)
		if err != nil || !result.IsError {
			t.Fatalf("invalid sources were accepted: result=%+v err=%v", result, err)
		}
	}
}
