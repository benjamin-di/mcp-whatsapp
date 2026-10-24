package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// expectedReadOnlyToolNames is the reviewed allowlist for read-only mode,
// kept separate from readOnlyPolicies so that marking another tool
// keepInReadOnly needs a matching, deliberate change here too.
var expectedReadOnlyToolNames = []string{
	"download_media",
	"get_blocklist",
	"get_chat",
	"get_group_info",
	"get_message_context",
	"get_poll_results",
	"get_privacy_settings",
	"get_status",
	"list_chats",
	"list_groups",
	"list_messages",
	"pairing_status",
	"request_sync",
	"search_contacts",
}

// readOnlyHintExceptions lists read-only-mode tools whose ReadOnlyHint is
// false, with the reason each is still safe to keep.
var readOnlyHintExceptions = map[string]string{
	"download_media": "writes the decrypted file to the local cache (and optional output_path under the media root); fetches from the CDN only, the sender is not notified",
	"request_sync":   "sends a history-sync request to the account's own primary phone; nothing reaches contacts or changes a chat",
}

func registeredToolNames(s *Server) []string {
	names := make([]string, 0, len(s.MCP().ListTools()))
	for name := range s.MCP().ListTools() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Every tool the server registers normally must carry an explicit read-only
// decision, and the policy map must not name tools that no longer exist.
func TestReadOnlyPolicies_ClassifyEveryTool(t *testing.T) {
	registered := registeredToolNames(NewServer(nil, nil))
	for _, name := range registered {
		if _, ok := readOnlyPolicies[name]; !ok {
			t.Errorf("tool %q has no entry in readOnlyPolicies (readonly.go); classify it keepInReadOnly or dropInReadOnly", name)
		}
	}
	known := make(map[string]bool, len(registered))
	for _, name := range registered {
		known[name] = true
	}
	for name, policy := range readOnlyPolicies {
		if !known[name] {
			t.Errorf("readOnlyPolicies names %q, which is not a registered tool", name)
		}
		if policy != keepInReadOnly && policy != dropInReadOnly {
			t.Errorf("readOnlyPolicies[%q] = %d, not a valid policy", name, policy)
		}
	}
}

func TestReadOnlyMode_RegistersOnlyAllowlist(t *testing.T) {
	got := registeredToolNames(NewServer(nil, nil, WithReadOnly(true)))
	if strings.Join(got, ",") != strings.Join(expectedReadOnlyToolNames, ",") {
		t.Fatalf("read-only tools = %v\nwant %v", got, expectedReadOnlyToolNames)
	}
	for _, name := range got {
		if readOnlyPolicies[name] != keepInReadOnly {
			t.Errorf("tool %q registered in read-only mode but not classified keepInReadOnly", name)
		}
	}
}

// Every tool read-only mode keeps must declare ReadOnlyHint=true unless it is
// a documented exception, and exceptions must not go stale.
func TestReadOnlyMode_ReadOnlyHintCrossCheck(t *testing.T) {
	tools := NewServer(nil, nil, WithReadOnly(true)).MCP().ListTools()
	for name, st := range tools {
		hint := st.Tool.Annotations.ReadOnlyHint
		readOnly := hint != nil && *hint
		_, excepted := readOnlyHintExceptions[name]
		switch {
		case !readOnly && !excepted:
			t.Errorf("tool %q is kept in read-only mode but ReadOnlyHint is not true and it has no readOnlyHintExceptions entry", name)
		case readOnly && excepted:
			t.Errorf("tool %q has ReadOnlyHint=true; drop its stale readOnlyHintExceptions entry", name)
		}
	}
	for name := range readOnlyHintExceptions {
		if _, ok := tools[name]; !ok {
			t.Errorf("readOnlyHintExceptions names %q, which read-only mode does not register", name)
		}
	}
}

// Tools that declare themselves mutating must never be kept, whatever the
// policy map says: a guard against a keepInReadOnly typo.
func TestReadOnlyMode_DropsEveryMutatingTool(t *testing.T) {
	all := NewServer(nil, nil).MCP().ListTools()
	readOnly := NewServer(nil, nil, WithReadOnly(true)).MCP().ListTools()
	for name, st := range all {
		hint := st.Tool.Annotations.ReadOnlyHint
		if hint != nil && *hint {
			continue
		}
		if _, kept := readOnly[name]; kept {
			if _, excepted := readOnlyHintExceptions[name]; !excepted {
				t.Errorf("mutating tool %q is registered in read-only mode", name)
			}
		}
	}
	for _, name := range []string{"send_message", "send_reaction", "mark_chat_read", "send_presence", "is_on_whatsapp", "get_group_invite_link"} {
		if _, ok := readOnly[name]; ok {
			t.Errorf("%q must not be registered in read-only mode", name)
		}
	}
}

func TestReadOnlyMode_OffKeepsAllTools(t *testing.T) {
	got := registeredToolNames(NewServer(nil, nil, WithReadOnly(false)))
	if strings.Join(got, ",") != strings.Join(expectedToolNames, ",") {
		t.Fatalf("WithReadOnly(false) tools = %v\nwant %v", got, expectedToolNames)
	}
}

// The media resource template backs the same download path as download_media,
// which read-only mode keeps, so it stays too.
func TestReadOnlyMode_KeepsMediaResource(t *testing.T) {
	s := NewServer(nil, nil, WithReadOnly(true))
	req := `{"jsonrpc":"2.0","id":1,"method":"resources/templates/list"}`
	raw, err := json.Marshal(s.MCP().HandleMessage(context.Background(), json.RawMessage(req)))
	if err != nil {
		t.Fatalf("marshal resources/templates/list response: %v", err)
	}
	if !strings.Contains(string(raw), mediaURITemplate) {
		t.Fatalf("media resource template %q not registered in read-only mode: %s", mediaURITemplate, raw)
	}
}

func callGetStatus(t *testing.T, s *Server) map[string]any {
	t.Helper()
	tool, ok := s.MCP().ListTools()["get_status"]
	if !ok || tool == nil || tool.Handler == nil {
		t.Fatal("get_status not registered")
	}
	res, err := tool.Handler(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("get_status handler: %v", err)
	}
	text, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("get_status content not text: %T", res.Content[0])
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(text.Text), &status); err != nil {
		t.Fatalf("decode %q: %v", text.Text, err)
	}
	return status
}

func TestGetStatus_ReportsReadOnly(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		status := callGetStatus(t, NewServer(nil, nil, WithReadOnly(readOnly)))
		if status["read_only"] != readOnly {
			t.Errorf("WithReadOnly(%v): read_only = %v", readOnly, status["read_only"])
		}
		if status["connected"] != false || status["paired"] != false {
			t.Errorf("nil client: connected=%v paired=%v, want false/false", status["connected"], status["paired"])
		}
		if status["state"] != "unpaired" || status["ready"] != false {
			t.Errorf("nil client: state=%v ready=%v, want unpaired/false", status["state"], status["ready"])
		}
	}
}

// initializeInstructions runs an MCP initialize request and returns the
// server instructions from the result.
func initializeInstructions(t *testing.T, s *Server) string {
	t.Helper()
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	resp := s.MCP().HandleMessage(context.Background(), json.RawMessage(req))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal initialize response: %v", err)
	}
	var out struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode initialize response %s: %v", raw, err)
	}
	return out.Result.Instructions
}

func TestReadOnlyMode_ServerInstructions(t *testing.T) {
	if got := initializeInstructions(t, NewServer(nil, nil, WithReadOnly(true))); got != readOnlyInstructions {
		t.Errorf("read-only instructions = %q, want %q", got, readOnlyInstructions)
	}
	if got := initializeInstructions(t, NewServer(nil, nil)); got != "" {
		t.Errorf("default instructions = %q, want none", got)
	}
}
