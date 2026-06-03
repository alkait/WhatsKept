package app

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Exercises the People tagging stored in the workspace DB (wa_person +
// wa_person_face): name, merge-by-name, rename-merge, hide, detach, the
// legacy-table drop, and the v_person_photo agent view.
func TestPeopleTaggingFlow(t *testing.T) {
	ws := t.TempDir()
	fd := filepath.Join(ws, "faces")
	_ = os.MkdirAll(fd, 0o755)
	_ = os.WriteFile(filepath.Join(fd, "clusters.json"), []byte(`{"clusters":[
	  {"id":0,"count":2,"representative":"crops/100_0.jpg","members":[{"crop":"crops/100_0.jpg","rowid":100,"quality":0.9},{"crop":"crops/101_0.jpg","rowid":101,"quality":0.8}]},
	  {"id":1,"count":1,"representative":"crops/102_0.jpg","members":[{"crop":"crops/102_0.jpg","rowid":102,"quality":0.7}]},
	  {"id":2,"count":1,"representative":"crops/103_0.jpg","members":[{"crop":"crops/103_0.jpg","rowid":103,"quality":0.6}]}]}`), 0o644)

	dbPath := filepath.Join(ws, "ChatStorage.sqlite")
	db, _ := sql.Open("sqlite3", dbPath)
	db.Exec(`CREATE TABLE m(id INTEGER, ts TEXT, chat_title TEXT, sender_name TEXT)`)
	for _, id := range []int{100, 101, 102, 103} {
		db.Exec(`INSERT INTO m VALUES(?,?,?,?)`, id, "2024-01-01", "Family", "me")
	}
	db.Exec(`CREATE VIEW v_messages AS SELECT id AS rowid, ts, chat_title, sender_name FROM m`)
	db.Exec(`CREATE TABLE wa_person_face (rowid INTEGER, person TEXT)`) // legacy shape → must be dropped
	db.Exec(`INSERT INTO wa_person_face VALUES (999,'stale')`)
	db.Close()

	s := &server{ws: newWorkspaceState(), jobs: newJobManager()}
	s.ws.set(ws)
	get := func(q string) map[string]any {
		rec := httptest.NewRecorder()
		s.handleFaceClusters(rec, httptest.NewRequest(http.MethodGet, "/x"+q, nil))
		if rec.Code != 200 {
			t.Fatalf("clusters %d: %s", rec.Code, rec.Body.String())
		}
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	label := func(b string) string {
		rec := httptest.NewRecorder()
		s.handleFaceLabel(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(b)))
		if rec.Code != 200 {
			t.Fatalf("label %d: %s", rec.Code, rec.Body.String())
		}
		var m map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m["person_id"]
	}
	find := func(m map[string]any, name string) (string, int) {
		for _, p := range m["people"].([]any) {
			pp := p.(map[string]any)
			if pp["name"] == name {
				return pp["id"].(string), int(pp["count"].(float64))
			}
		}
		return "", 0
	}

	// Clean start: one visible auto group (cluster 0, count 2), no names.
	if n := len(get("")["people"].([]any)); n != 1 {
		t.Fatalf("clean start want 1 group, got %d", n)
	}

	// name (lowercased) + merge-by-name + rename-merge.
	label(`{"action":"name","crops":["crops/100_0.jpg","crops/101_0.jpg"],"name":"Dad"}`)
	if _, n := find(get(""), "dad"); n != 2 {
		t.Fatal("name failed")
	}
	label(`{"action":"name","crops":["crops/102_0.jpg"],"name":"dad"}`) // merge
	momID := label(`{"action":"name","crops":["crops/103_0.jpg"],"name":"mom"}`)
	label(`{"action":"name","crops":[],"person_id":"` + momID + `","name":"dad"}`) // rename-merge
	m := get("")
	if _, n := find(m, "dad"); n != 4 {
		t.Fatalf("dad should be 4 after merges, got %d", n)
	}
	if _, mn := find(m, "mom"); mn != 0 {
		t.Fatal("mom should be gone")
	}
	if ns := m["names"].([]any); len(ns) != 1 || ns[0] != "dad" {
		t.Fatalf("autocomplete names wrong: %v", ns)
	}

	// detach one wrong photo → dad drops to 3, the face reverts to auto.
	label(`{"action":"detach","crops":["crops/103_0.jpg"]}`)
	if _, n := find(get(""), "dad"); n != 3 {
		t.Fatalf("detach: dad should be 3, got %d", n)
	}

	// agent view + legacy drop.
	vdb, _ := sql.Open("sqlite3", dbPath)
	defer vdb.Close()
	var photos, legacy int
	vdb.QueryRow(`SELECT COUNT(*) FROM v_person_photo WHERE person='dad'`).Scan(&photos)
	if photos != 3 {
		t.Fatalf("v_person_photo dad want 3, got %d", photos)
	}
	vdb.QueryRow(`SELECT COUNT(*) FROM wa_person_face WHERE rowid=999`).Scan(&legacy)
	if legacy != 0 {
		t.Fatal("legacy wa_person_face row survived")
	}

	// hide → out of grid + view.
	dadID, _ := find(get(""), "dad")
	label(`{"action":"hide","person_id":"` + dadID + `"}`)
	if _, n := find(get(""), "dad"); n != 0 {
		t.Fatal("hidden dad still visible")
	}
	vdb.QueryRow(`SELECT COUNT(*) FROM v_person_photo WHERE person='dad'`).Scan(&photos)
	if photos != 0 {
		t.Fatalf("hidden dad still in view (%d)", photos)
	}
}
