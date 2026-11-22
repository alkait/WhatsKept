package live

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/appstate/lthash"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/cbcutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// TestArchiveAfterKeyRotation guards the whatsmeow patch in
// third_party/whatsmeow: archiving a chat whose archive state was
// written under an earlier app-state key must remove that record, or
// the primary — which counts one record per index — signs a different
// LTHash than ours and every later regular_low sync fails.
func TestArchiveAfterKeyRotation(t *testing.T) {
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite3",
		"file:"+filepath.Join(t.TempDir(), "session.db")+"?_foreign_keys=on", waLog.Noop)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { container.Close() })
	dev := container.NewDevice()
	dev.ID = &types.JID{User: "971500000000", Server: types.DefaultUserServer}
	dev.Account = &waAdv.ADVSignedDeviceIdentity{
		Details: []byte{}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64),
	}
	if err := container.PutDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	proc := appstate.NewProcessor(dev, waLog.Noop)

	oldKey, newKey := []byte{0, 0, 0, 0, 0x7e, 0xf5}, []byte{0, 0, 0, 0, 0x7f, 0x1c}
	keyData := map[string][]byte{}
	putKey := func(id []byte, ts int64) {
		data := bytes.Repeat([]byte{id[5]}, 32)
		keyData[string(id)] = data
		if err := dev.AppStateKeys.PutAppStateSyncKey(ctx, id, store.AppStateSyncKey{Data: data, Fingerprint: []byte{}, Timestamp: ts}); err != nil {
			t.Fatal(err)
		}
	}
	chat := types.NewJID("120363000000000001", types.GroupServer)
	other := types.NewJID("120363000000000002", types.GroupServer)

	// primary models the phone: one record per logical index.
	primary := map[string][]byte{}
	// send encodes a patch the way SendAppState does, checks the
	// primary accepts its snapshot MAC, then applies it to our store
	// with MAC validation, as the fetch after sending does.
	send := func(keyID []byte, info appstate.PatchInfo) *waServerSync.SyncdPatch {
		t.Helper()
		version, hash, err := dev.AppState.GetAppStateVersion(ctx, string(info.Type))
		if err != nil {
			t.Fatal(err)
		}
		state := appstate.HashState{Version: version, Hash: hash}
		raw, err := proc.EncodePatch(ctx, keyID, state, info)
		if err != nil {
			t.Fatal(err)
		}
		var patch waServerSync.SyncdPatch
		if err := proto.Unmarshal(raw, &patch); err != nil {
			t.Fatal(err)
		}
		patch.Version = &waServerSync.SyncdVersion{Version: proto.Uint64(version + 1)}

		for _, m := range patch.GetMutations() {
			index := decryptIndex(t, keyData[string(m.GetRecord().GetKeyID().GetID())], m)
			if m.GetOperation() == waServerSync.SyncdMutation_REMOVE {
				delete(primary, index)
			} else {
				blob := m.GetRecord().GetValue().GetBlob()
				primary[index] = blob[len(blob)-32:]
			}
		}
		var values [][]byte
		for _, v := range primary {
			values = append(values, v)
		}
		var primaryHash [128]byte
		lthash.WAPatchIntegrity.SubtractThenAddInPlace(primaryHash[:], nil, values)
		snapKey := hkdfutil.SHA256(keyData[string(keyID)], nil, []byte("WhatsApp Mutation Keys"), 160)[96:128]
		if !hmac.Equal(snapshotMAC(snapKey, primaryHash, version+1, info.Type), patch.GetSnapshotMAC()) {
			t.Fatalf("v%d: primary's LTHash does not match the patch's snapshot MAC", version+1)
		}

		list := &appstate.PatchList{Name: info.Type, Patches: []*waServerSync.SyncdPatch{&patch}}
		if _, _, err := proc.DecodePatches(ctx, list, state, true); err != nil {
			t.Fatalf("v%d: applying our own patch: %v", version+1, err)
		}
		return &patch
	}
	ops := func(p *waServerSync.SyncdPatch) (out []string) {
		for _, m := range p.GetMutations() {
			key := "new"
			if bytes.Equal(m.GetRecord().GetKeyID().GetID(), oldKey) {
				key = "old"
			}
			out = append(out, m.GetOperation().String()+"/"+key)
		}
		return out
	}
	expect := func(what string, p *waServerSync.SyncdPatch, want ...string) {
		t.Helper()
		if got := ops(p); !slices.Equal(got, want) {
			t.Fatalf("%s: mutations %v, want %v", what, got, want)
		}
	}

	putKey(oldKey, 1)
	expect("archive under the first key", send(oldKey, appstate.BuildArchive(chat, true, time.Now(), nil)),
		"SET/old", "SET/old")

	// The phone rotates the key. Archiving again must replace both
	// records (archive, unpin) instead of adding a second copy of each.
	putKey(newKey, 2)
	expect("archive after rotation", send(newKey, appstate.BuildArchive(chat, true, time.Now(), nil)),
		"REMOVE/old", "SET/new", "REMOVE/old", "SET/new")
	expect("archive once both records are under the new key", send(newKey, appstate.BuildArchive(chat, true, time.Now(), nil)),
		"SET/new", "SET/new")
	expect("read on a chat with no history", send(newKey, appstate.BuildMarkChatAsRead(other, true, time.Now(), nil)),
		"SET/new")
}

func decryptIndex(t *testing.T, keyData []byte, m *waServerSync.SyncdMutation) string {
	t.Helper()
	enc := hkdfutil.SHA256(keyData, nil, []byte("WhatsApp Mutation Keys"), 160)[32:64]
	blob := bytes.Clone(m.GetRecord().GetValue().GetBlob())
	content := blob[:len(blob)-32]
	plain, err := cbcutil.Decrypt(enc, content[:16], content[16:])
	if err != nil {
		t.Fatal(err)
	}
	var data waSyncAction.SyncActionData
	if err := proto.Unmarshal(plain, &data); err != nil {
		t.Fatal(err)
	}
	return string(data.GetIndex())
}

func snapshotMAC(key []byte, hash [128]byte, version uint64, name appstate.WAPatchName) []byte {
	v := make([]byte, 8)
	binary.BigEndian.PutUint64(v, version)
	h := hmac.New(sha256.New, key)
	h.Write(hash[:])
	h.Write(v)
	h.Write([]byte(name))
	return h.Sum(nil)
}
