package postprocess

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Verifies the user's core requirement: people tags survive a re-sync.
func TestPersonCarryForward(t *testing.T) {
	dir := t.TempDir()
	oldP := filepath.Join(dir, "old.sqlite")
	newP := filepath.Join(dir, "new.sqlite")

	od, _ := sql.Open("sqlite3", oldP)
	od.Exec(`CREATE TABLE ZWAMESSAGE (Z_PK INTEGER)`)
	for _, id := range []int{100, 101, 102} {
		od.Exec(`INSERT INTO ZWAMESSAGE VALUES(?)`, id)
	}
	od.Exec(PersonSidecarsSQL)
	od.Exec(`INSERT INTO wa_person(person_id,name,hidden) VALUES(1,'dad',0)`)
	od.Exec(`INSERT INTO wa_person_face VALUES(100,0,1),(101,0,1),(999,0,1)`) // 999 is gone in new
	od.Close()

	// Fresh extract: messages 100,101 survive; 102 and 999 are gone. No
	// person tables (just like a real re-extracted ChatStorage.sqlite).
	nd, _ := sql.Open("sqlite3", newP)
	nd.Exec(`CREATE TABLE ZWAMESSAGE (Z_PK INTEGER)`)
	nd.Exec(`INSERT INTO ZWAMESSAGE VALUES(100),(101)`)
	nd.Close()

	if _, err := mergeSidecarsForward(oldP, newP); err != nil {
		t.Fatalf("merge: %v", err)
	}

	vd, _ := sql.Open("sqlite3", newP)
	defer vd.Close()
	var name string
	if err := vd.QueryRow(`SELECT name FROM wa_person WHERE person_id=1`).Scan(&name); err != nil || name != "dad" {
		t.Fatalf("wa_person not carried: name=%q err=%v", name, err)
	}
	var faces int
	vd.QueryRow(`SELECT COUNT(*) FROM wa_person_face`).Scan(&faces)
	if faces != 2 { // 100,101 kept; 999 pruned (message gone)
		t.Fatalf("wa_person_face want 2 carried, got %d", faces)
	}
	var has999 int
	vd.QueryRow(`SELECT COUNT(*) FROM wa_person_face WHERE rowid=999`).Scan(&has999)
	if has999 != 0 {
		t.Fatal("orphaned face (deleted message) was carried forward")
	}
	t.Log("person tags survive a re-sync ✓")
}
