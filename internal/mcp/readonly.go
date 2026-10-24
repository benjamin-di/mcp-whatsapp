package mcp

import (
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// readOnlyPolicy says whether a tool is registered when the server runs in
// read-only mode (`serve -read-only`).
type readOnlyPolicy int

const (
	// keepInReadOnly marks a tool that cannot change anything on WhatsApp or
	// act on the paired account, so read-only mode still registers it.
	keepInReadOnly readOnlyPolicy = iota + 1
	// dropInReadOnly marks a tool that sends, changes account or group state,
	// or queries WhatsApp about third parties; read-only mode skips it.
	dropInReadOnly
)

// readOnlyPolicies is the reviewed classification of every MCP tool. A tool
// missing from this map is never registered in read-only mode (fail closed),
// and tools_readonly_test.go fails until it is classified, so a new tool
// needs an explicit decision here.
var readOnlyPolicies = map[string]readOnlyPolicy{
	// Local cache reads.
	"search_contacts":     keepInReadOnly,
	"list_messages":       keepInReadOnly,
	"list_chats":          keepInReadOnly,
	"get_chat":            keepInReadOnly,
	"get_message_context": keepInReadOnly,
	"get_poll_results":    keepInReadOnly,

	// Bridge and pairing state.
	"get_status":     keepInReadOnly,
	"pairing_status": keepInReadOnly,

	// Live reads of the paired account's own groups and settings: no
	// notification, nothing changes.
	"list_groups":          keepInReadOnly,
	"get_group_info":       keepInReadOnly,
	"get_blocklist":        keepInReadOnly,
	"get_privacy_settings": keepInReadOnly,

	// Kept although their ReadOnlyHint is false (see readOnlyHintExceptions
	// in tools_readonly_test.go). download_media fetches media of an
	// already-received message from the CDN and writes it locally, without
	// telling the sender. request_sync sends a history-sync request to the
	// account's own primary phone only; no contact sees anything.
	"download_media": keepInReadOnly,
	"request_sync":   keepInReadOnly,

	// Sending of any kind.
	"send_message":       dropInReadOnly,
	"send_file":          dropInReadOnly,
	"send_audio_message": dropInReadOnly,
	"send_reply":         dropInReadOnly,
	"send_reaction":      dropInReadOnly,
	"send_poll":          dropInReadOnly,
	"send_poll_vote":     dropInReadOnly,
	"send_contact_card":  dropInReadOnly,

	// Changes to existing messages and read state.
	"edit_message":   dropInReadOnly,
	"delete_message": dropInReadOnly,
	"mark_read":      dropInReadOnly,
	"mark_chat_read": dropInReadOnly,

	// Presence, typing, privacy and profile.
	"send_typing":         dropInReadOnly,
	"send_presence":       dropInReadOnly,
	"set_privacy_setting": dropInReadOnly,
	"set_status_message":  dropInReadOnly,
	"block_contact":       dropInReadOnly,
	"unblock_contact":     dropInReadOnly,

	// Group changes. get_group_invite_link is dropped too: reset=true revokes
	// the link, and even a plain read hands out a credential to join the group.
	"create_group":              dropInReadOnly,
	"leave_group":               dropInReadOnly,
	"update_group_participants": dropInReadOnly,
	"set_group_name":            dropInReadOnly,
	"set_group_topic":           dropInReadOnly,
	"set_group_announce":        dropInReadOnly,
	"set_group_locked":          dropInReadOnly,
	"get_group_invite_link":     dropInReadOnly,
	"join_group_with_link":      dropInReadOnly,

	// Asks WhatsApp's servers about arbitrary phone numbers (third parties).
	"is_on_whatsapp": dropInReadOnly,
}

// readOnlyInstructions is sent as the MCP server instructions in read-only
// mode so clients know why send and action tools are absent.
const readOnlyInstructions = "This WhatsApp server runs in read-only mode: only tools that read the local message cache or the account's own state are available. Sending, reactions, edits, deletes, mark-read, typing/presence, group, privacy and blocklist changes are disabled by the operator and cannot be enabled from a tool call."

// addTool registers tool unless read-only mode is on and the tool is not
// classified keepInReadOnly in readOnlyPolicies.
func (s *Server) addTool(tool mcp.Tool, handler server.ToolHandlerFunc) {
	if s.readOnly && readOnlyPolicies[tool.Name] != keepInReadOnly {
		return
	}
	s.mcp.AddTool(tool, handler)
}
