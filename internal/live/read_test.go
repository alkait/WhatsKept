package live

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A read change goes on the wire addressed like an archive change —
// a DM by its LID, with the chat's latest message — and leaves the
// database alone.
func TestMarkReadThroughLive(t *testing.T) {
	w, root, wire := archiveFixture(t)
	ctx := context.Background()

	got, err := MarkRead(ctx, root, "+971 50 000 0001", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != (MarkedRead{Chat: "110000000001@lid", Read: true}) {
		t.Errorf("read = %+v", got)
	}
	if len(wire.readOf) != 1 || wire.readOf[0].JID.String() != "110000000001@lid" ||
		wire.readOf[0].Last.GetID() != "D-NEW" || !wire.reads[0] {
		t.Errorf("wire = %+v %v", wire.readOf, wire.reads)
	}

	got, err = MarkRead(ctx, root, "120363000000000001@g.us", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != (MarkedRead{Chat: "120363000000000001@g.us", Read: false}) || wire.reads[1] ||
		wire.readOf[1].Last.GetParticipant() != "110000000009@lid" {
		t.Errorf("unread = %+v, wire %+v %v", got, wire.readOf[1], wire.reads)
	}
	if len(wire.archived) != 0 {
		t.Errorf("archive wire = %+v, want nothing", wire.archived)
	}
	if v := archivedOf(t, w, 2); v.Int64 != 0 {
		t.Errorf("ZARCHIVED = %v, want 0", v)
	}
}

// A request that can't be carried out puts nothing on the wire.
func TestMarkReadRejects(t *testing.T) {
	_, root, wire := archiveFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		to   string
		read bool
		want string
	}{
		{"Sarah", true, "invalid chat"},
		{"status@broadcast", true, "is not a chat"},
		{"+971509999999", true, "cannot mark read: chat 971509999999@s.whatsapp.net is not in the database"},
		{"+971509999999", false, "cannot mark unread: chat"},
	} {
		if _, err := MarkRead(ctx, root, c.to, c.read); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("MarkRead(%q, %v): err = %v, want %q", c.to, c.read, err, c.want)
		}
	}

	wire.readErr = errors.New("websocket not connected")
	if _, err := MarkRead(ctx, root, "120363000000000001@g.us", true); err == nil ||
		!strings.Contains(err.Error(), "mark read failed: websocket not connected") {
		t.Errorf("wire failure: err = %v", err)
	}
	if len(wire.readOf) != 0 {
		t.Errorf("wire = %+v, want nothing", wire.readOf)
	}
}

func TestMarkReadLiveNotRunning(t *testing.T) {
	if _, err := MarkRead(context.Background(), initWorkspace(t), "+971500000001", true); !errors.Is(err, ErrNotRunning) {
		t.Errorf("err = %v, want ErrNotRunning", err)
	}
}
