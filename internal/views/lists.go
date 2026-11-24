package views

import (
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed embed/lists.sql
var listsSQL string

// EnsureLists creates the list tables (wa_list, wa_list_chat) when
// missing and (re)creates v_lists and v_list_chats. Needs v_chats, so
// it runs after EnsureChats. Live fills the tables.
func EnsureLists(db *sql.DB) error {
	if _, err := db.Exec(listsSQL); err != nil {
		return fmt.Errorf("apply lists schema: %w", err)
	}
	return nil
}
