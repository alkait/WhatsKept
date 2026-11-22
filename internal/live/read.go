package live

// Marking a chat read. Like archiving, it is an app-state change
// (WhatsApp's "Mark as read" / "Mark as unread"), synced to the user's
// own devices and seen by nobody else: no read receipts go out, so
// senders get no blue ticks. The database keeps no unread state, so
// nothing is recorded.

// MarkedRead describes a chat after a read change.
type MarkedRead struct {
	Chat string `json:"chat" jsonschema:"the chat JID the change was made for, as WhatsApp addresses it"`
	Read bool   `json:"read" jsonschema:"true if the chat was marked read, false if marked unread"`
}
