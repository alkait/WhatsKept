package live

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
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

// TestToleratesUncountedRecords guards the tolerance in the vendored
// whatsmeow: a primary that folded a record it never uploaded into its
// LTHash signs snapshots and patches no client can add up. Those are
// accepted — like WhatsApp's own linked devices — as long as every
// record and every patch still verifies on its own; tampering, a bad
// patch MAC and a skipped version are still refused.
func TestToleratesUncountedRecords(t *testing.T) {
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite3",
		"file:"+filepath.Join(t.TempDir(), "session.db")+"?_foreign_keys=on", waLog.Noop)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { container.Close() })
	device := func(user string) *store.Device {
		dev := container.NewDevice()
		dev.ID = &types.JID{User: user, Server: types.DefaultUserServer}
		dev.Account = &waAdv.ADVSignedDeviceIdentity{
			Details: []byte{}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64),
		}
		if err := container.PutDevice(ctx, dev); err != nil {
			t.Fatal(err)
		}
		return dev
	}
	keyID := []byte{0, 0, 0, 0, 0x7f, 0x12}
	keyData := bytes.Repeat([]byte{7}, 32)
	snapKey := hkdfutil.SHA256(keyData, nil, []byte("WhatsApp Mutation Keys"), 160)[96:128]
	phoneDev, ourDev := device("971500000001"), device("971500000002")
	for _, d := range []*store.Device{phoneDev, ourDev} {
		if err := d.AppStateKeys.PutAppStateSyncKey(ctx, keyID, store.AppStateSyncKey{Data: keyData, Fingerprint: []byte{}, Timestamp: 1}); err != nil {
			t.Fatal(err)
		}
	}
	phone, ours := appstate.NewProcessor(phoneDev, waLog.Noop), appstate.NewProcessor(ourDev, waLog.Noop)
	name := appstate.WAPatchRegularHigh
	mute := func(user string) appstate.PatchInfo {
		return appstate.BuildMute(types.NewJID(user, types.DefaultUserServer), true, 0)
	}
	// encode has the phone write a patch on top of its state.
	encode := func(state appstate.HashState, info appstate.PatchInfo) *waServerSync.SyncdPatch {
		t.Helper()
		raw, err := phone.EncodePatch(ctx, keyID, state, info)
		if err != nil {
			t.Fatal(err)
		}
		var p waServerSync.SyncdPatch
		if err := proto.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		p.Version = &waServerSync.SyncdVersion{Version: proto.Uint64(state.Version + 1)}
		return &p
	}
	valueMAC := func(m *waServerSync.SyncdMutation) []byte {
		b := m.GetRecord().GetValue().GetBlob()
		return b[len(b)-32:]
	}

	// The server holds two records; the phone's hash also counts a third
	// it never uploaded.
	a, b, hidden := encode(appstate.HashState{}, mute("971500000010")), encode(appstate.HashState{}, mute("971500000011")),
		encode(appstate.HashState{}, mute("971500000012"))
	var phoneState appstate.HashState
	phoneState.Version = 5
	lthash.WAPatchIntegrity.SubtractThenAddInPlace(phoneState.Hash[:], nil,
		[][]byte{valueMAC(a.Mutations[0]), valueMAC(b.Mutations[0]), valueMAC(hidden.Mutations[0])})
	snapshot := func(records ...*waServerSync.SyncdRecord) *waServerSync.SyncdSnapshot {
		return &waServerSync.SyncdSnapshot{
			Version: &waServerSync.SyncdVersion{Version: proto.Uint64(5)},
			Records: records,
			Mac:     snapshotMAC(snapKey, phoneState.Hash, 5, name),
			KeyID:   &waServerSync.KeyId{ID: keyID},
		}
	}
	decode := func(state appstate.HashState, list *appstate.PatchList) (appstate.HashState, error) {
		_, next, err := ours.DecodePatches(ctx, list, state, true)
		return next, err
	}

	ourState, err := decode(appstate.HashState{}, &appstate.PatchList{Name: name,
		Snapshot: snapshot(a.Mutations[0].Record, b.Mutations[0].Record)})
	if err != nil {
		t.Fatalf("snapshot with an uncounted record: %v", err)
	}
	if ourState.Version != 5 || ourState.Hash == phoneState.Hash {
		t.Fatalf("state after snapshot: v%d, hash equal to the phone's: %v", ourState.Version, ourState.Hash == phoneState.Hash)
	}

	tampered := proto.Clone(a.Mutations[0].Record).(*waServerSync.SyncdRecord)
	tampered.Value.Blob[20] ^= 1
	if _, err := decode(appstate.HashState{}, &appstate.PatchList{Name: name,
		Snapshot: snapshot(tampered, b.Mutations[0].Record)}); err == nil {
		t.Fatal("snapshot with a tampered record was accepted")
	}

	// The phone's next patch carries its own hash; ours differs by the
	// uncounted record, and the patch is still applied.
	next := encode(phoneState, mute("971500000013"))
	if _, err := decode(ourState, &appstate.PatchList{Name: name, Patches: []*waServerSync.SyncdPatch{next}}); err != nil {
		t.Fatalf("validly signed patch on top: %v", err)
	}

	badMAC := proto.Clone(next).(*waServerSync.SyncdPatch)
	badMAC.PatchMAC[0] ^= 1
	if _, err := decode(ourState, &appstate.PatchList{Name: name, Patches: []*waServerSync.SyncdPatch{badMAC}}); err == nil {
		t.Fatal("patch with a bad patch MAC was accepted")
	}

	skipped := proto.Clone(next).(*waServerSync.SyncdPatch)
	skipped.Version = &waServerSync.SyncdVersion{Version: proto.Uint64(7)}
	if _, err := decode(ourState, &appstate.PatchList{Name: name, Patches: []*waServerSync.SyncdPatch{skipped}}); err == nil {
		t.Fatal("patch after a skipped version was accepted")
	}
}

// TestHealer covers live's answer to a stuck collection: a failed
// incremental sync becomes one full sync, at most hourly per collection.
func TestHealer(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	broken := map[appstate.WAPatchName]bool{appstate.WAPatchRegularHigh: true}
	var calls []string
	h := newAppStateHealer(func(_ context.Context, name appstate.WAPatchName, full bool) error {
		kind := "incremental"
		if full {
			kind = "full"
		}
		calls = append(calls, string(name)+"/"+kind)
		if !full && broken[name] {
			return errors.New("failed to verify patch v222: mismatching LTHash")
		}
		return nil
	})
	h.now = func() time.Time { return now }

	h.syncAll(ctx)
	want := []string{"critical_block/incremental", "critical_unblock_low/incremental",
		"regular_high/incremental", "regular_high/full", "regular/incremental", "regular_low/incremental"}
	if !slices.Equal(calls, want) {
		t.Fatalf("syncAll: %v, want %v", calls, want)
	}

	// The same failure reported again by whatsmeow: no second full sync.
	calls = nil
	if h.heal(ctx, appstate.WAPatchRegularHigh, errors.New("again")) || len(calls) != 0 {
		t.Fatalf("second heal within the hour ran: %v", calls)
	}
	now = now.Add(30 * time.Minute)
	if h.heal(ctx, appstate.WAPatchRegularHigh, errors.New("again")) {
		t.Fatal("heal ran within the hour")
	}
	now = now.Add(31 * time.Minute)
	if !h.heal(ctx, appstate.WAPatchRegularHigh, errors.New("again")) || !slices.Equal(calls, []string{"regular_high/full"}) {
		t.Fatalf("heal after an hour: %v", calls)
	}
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
