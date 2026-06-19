package app

import (
	"archive/zip"
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"whatskept/internal/helpers"
	"whatskept/internal/postprocess"
)

// This file owns the "Find people" MVP: a face-clustering pass over the
// workspace's already-decrypted media/ folder. Unlike the other
// indexers it touches neither the encrypted backup nor ChatStorage.sqlite
// — it works purely with whatever image files are on disk under media/.
//
// The heavy lifting lives in the bundled `whatskept-faces` Swift binary
// (build/faces-helper/main.swift): it detects + aligns faces, embeds them
// with the AdaFace CoreML model (fetched + cached on first use),
// clusters them across all cores, writes crop thumbnails to
// <ws>/faces/crops/, and emits <ws>/faces/clusters.json.
//
// Go's role is thin: start the binary as an SSE job, translate its
// stdout progress lines into "progress" events the People card renders,
// then serve clusters.json + the crop thumbnails back to the UI.

// facesDir returns <workspace>/faces, the output tree for the clusterer.
func facesDir(ws string) string { return filepath.Join(ws, "faces") }

// faceModelDirName is the unzipped .mlpackage directory the AdaFace zip
// expands into, under the shared models dir. The faces helper loads it.
const faceModelDirName = "AdaFace_IR101.mlpackage"

// faceModelPackagePath returns the absolute path to the unzipped AdaFace
// .mlpackage and whether it currently exists on disk.
func faceModelPackagePath() (string, bool) {
	dir, err := helpers.ModelDir()
	if err != nil {
		return "", false
	}
	p := filepath.Join(dir, faceModelDirName)
	st, err := os.Stat(p)
	return p, err == nil && st.IsDir()
}

// ensureFaceModelUnzipped expands the verified AdaFace zip into the
// models dir if the .mlpackage isn't already there. Idempotent: a no-op
// once the package exists. Returns the .mlpackage path.
func ensureFaceModelUnzipped() (string, error) {
	if p, ok := faceModelPackagePath(); ok {
		return p, nil
	}
	zipPath, err := helpers.ModelPath(helpers.AdaFaceModel)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(zipPath); err != nil {
		return "", fmt.Errorf("face model archive not downloaded")
	}
	dir, err := helpers.ModelDir()
	if err != nil {
		return "", err
	}
	if err := unzipInto(zipPath, dir); err != nil {
		return "", fmt.Errorf("unzip face model: %w", err)
	}
	p, ok := faceModelPackagePath()
	if !ok {
		return "", fmt.Errorf("face model unzip produced no %s", faceModelDirName)
	}
	return p, nil
}

// unzipInto extracts a zip archive into destDir, rejecting any entry
// whose path would escape destDir (zip-slip guard).
func unzipInto(zipPath, destDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		target := filepath.Join(destDir, f.Name)
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe zip entry: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, rc)
		rc.Close()
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// faceStatusResponse drives the People card before/after a run: whether a
// media/ folder exists with images to scan, and whether a prior run left
// a clusters.json we can show without re-scanning.
type faceStatusResponse struct {
	HasMedia     bool  `json:"has_media"`     // media/ exists and holds at least one image
	ImageCount   int   `json:"image_count"`   // images currently in media/
	HasClusters  bool  `json:"has_clusters"`  // faces/clusters.json exists
	ClusterCount int   `json:"cluster_count"` // clusters with >= 2 faces (the grid surface)
	FaceCount    int   `json:"face_count"`    // total faces found in the last run
	PeopleCount  int   `json:"people_count"`  // clusters with >= 2 faces (recurring people)
	HasModel     bool  `json:"has_model"`     // AdaFace .mlpackage present on disk
	ModelBytes   int64 `json:"model_bytes"`   // download size of the model archive
}

// handleFaceStatus reports whether there's anything to scan and whether a
// prior clustering result is available to show. Read-only, best-effort.
func (s *server) handleFaceStatus(w http.ResponseWriter, _ *http.Request) {
	resp := faceStatusResponse{}
	cur := s.ws.get()
	if cur == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	mediaDir := filepath.Join(cur, "media")
	resp.ImageCount = countImages(mediaDir)
	resp.HasMedia = resp.ImageCount > 0

	_, resp.HasModel = faceModelPackagePath()
	resp.ModelBytes = helpers.AdaFaceModel.Bytes

	if cl, err := loadClusters(facesDir(cur)); err == nil {
		resp.HasClusters = true
		resp.FaceCount = cl.FaceCount
		for _, c := range cl.Clusters {
			if c.Count >= 2 {
				resp.PeopleCount++
			}
		}
		resp.ClusterCount = resp.PeopleCount
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleFaceIndex starts the `whatskept-faces` clusterer as an in-process
// SSE job. No password, no backup — it only reads <ws>/media. The job
// emits "progress" events ({phase, done, total, faces}) the card renders
// as a two-phase (scan → cluster) progress bar.
func (s *server) handleFaceIndex(w http.ResponseWriter, r *http.Request) {
	cur := s.ws.get()
	if cur == "" {
		httpError(w, http.StatusBadRequest, "no workspace open")
		return
	}
	mediaDir := filepath.Join(cur, "media")
	if countImages(mediaDir) == 0 {
		httpError(w, http.StatusBadRequest, "no images in media/ — download images first")
		return
	}

	// The face model must be present. If not, signal model_required so the
	// UI routes to the download step (mirrors the voice-index flow).
	modelPath, err := ensureFaceModelUnzipped()
	if err != nil {
		httpError(w, http.StatusPreconditionFailed, "model_required")
		return
	}

	jobID := s.jobs.startInProcessProgressCtx("face-index",
		func(ctx context.Context, log func(string), progress func(any)) error {
			return runFaceCluster(ctx, cur, modelPath, log, progress)
		})

	writeJSON(w, http.StatusOK, map[string]string{"job_id": jobID})
}

// faceModelStatusResponse drives the People card's model gate.
type faceModelStatusResponse struct {
	Present bool   `json:"present"`
	Bytes   int64  `json:"bytes"`
	Display string `json:"display"`
}

// handleFaceModelStatus reports whether the AdaFace model is on disk, so
// the UI can show a one-time "Download model" step before Find people.
func (s *server) handleFaceModelStatus(w http.ResponseWriter, _ *http.Request) {
	_, present := faceModelPackagePath()
	writeJSON(w, http.StatusOK, faceModelStatusResponse{
		Present: present,
		Bytes:   helpers.AdaFaceModel.Bytes,
		Display: helpers.AdaFaceModel.Display,
	})
}

// handleFaceModelDownload fetches + verifies the AdaFace archive and
// unzips it, as an SSE job emitting helpers.DownloadProgress events.
func (s *server) handleFaceModelDownload(w http.ResponseWriter, _ *http.Request) {
	jobID := s.jobs.startInProcessProgressCtx("face-model-download",
		func(ctx context.Context, log func(string), progress func(any)) error {
			log("Downloading face-recognition model (AdaFace IR-18, MIT)…")
			if err := helpers.DownloadModel(helpers.AdaFaceModel, helpers.DownloadOptions{
				Ctx:      ctx,
				Progress: func(p helpers.DownloadProgress) { progress(p) },
			}); err != nil {
				return err
			}
			log("Unpacking model…")
			if _, err := ensureFaceModelUnzipped(); err != nil {
				return err
			}
			log("Face model ready.")
			return nil
		})
	writeJSON(w, http.StatusOK, map[string]string{"job_id": jobID})
}

// runFaceCluster execs the bundled helper against <ws>/media, streaming
// its stdout progress lines into `progress` and its stderr log lines into
// `log`. Returns an error on non-zero exit or context cancellation.
func runFaceCluster(ctx context.Context, ws, modelPath string, log func(string), progress func(any)) error {
	dir, err := helpers.Path()
	if err != nil {
		return fmt.Errorf("locate helpers: %w", err)
	}
	bin := filepath.Join(dir, helpers.WhatskeptFaces)
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("faces helper not found at %s: %w", bin, err)
	}

	out := facesDir(ws)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("mkdir faces dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, bin, filepath.Join(ws, "media"), out, modelPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start faces helper: %w", err)
	}

	// Pump stderr → human log lines.
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			log(sc.Text())
		}
	}()

	// Pump stdout → structured progress. Each line is one JSON object
	// ({"type":"progress",...} or {"type":"done",...}); forward both as
	// progress events, which the card keys off the `phase` field.
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // ignore non-JSON noise
		}
		progress(ev)
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("faces helper failed: %w", err)
	}
	return nil
}

// clustersFile mirrors the shape the Swift helper writes. We decode only
// the fields the UI needs; `members` carries enough to render the grid +
// a per-person sample.
type clustersFile struct {
	ImageCount      int           `json:"image_count"`
	ImagesWithFaces int           `json:"images_with_faces"`
	FaceCount       int           `json:"face_count"`
	ClusterCount    int           `json:"cluster_count"`
	Threshold       float64       `json:"threshold"`
	Clusters        []faceCluster `json:"clusters"`
}

type faceCluster struct {
	ID             int          `json:"id"`
	Count          int          `json:"count"`
	Representative string       `json:"representative"`
	Members        []faceMember `json:"members"`
}

type faceMember struct {
	File    string  `json:"file"`
	Rowid   int64   `json:"rowid"`
	Crop    string  `json:"crop"`
	Quality float64 `json:"quality"`
}

func loadClusters(facesDir string) (*clustersFile, error) {
	b, err := os.ReadFile(filepath.Join(facesDir, "clusters.json"))
	if err != nil {
		return nil, err
	}
	var cl clustersFile
	if err := json.Unmarshal(b, &cl); err != nil {
		return nil, err
	}
	return &cl, nil
}

// People tags live in the workspace DB (wa_person + wa_person_face),
// GUI-written and carried forward across re-syncs like the other sidecars
// (see postprocess/sidecar.go). The agent reads them through v_person_photo
// (read-only). A "person" is unique by lowercase name; a face is keyed by
// (message rowid, face index) parsed from its crop path crops/<rowid>_<idx>.jpg.

// personMu serialises tag writes (one workspace DB, small writes).
var personMu sync.Mutex

func normName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// faceKeyFromCrop parses (rowid, face_idx) from "crops/<rowid>_<idx>.jpg".
func faceKeyFromCrop(crop string) (int64, int, bool) {
	base := filepath.Base(crop)
	us := strings.IndexByte(base, '_')
	dot := strings.LastIndexByte(base, '.')
	if us <= 0 || dot <= us {
		return 0, 0, false
	}
	rowid, e1 := strconv.ParseInt(base[:us], 10, 64)
	idx, e2 := strconv.Atoi(base[us+1 : dot])
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	return rowid, idx, true
}

// personPhotoView is the agent's read surface. Kept in sync with the copy
// in views.sql (which recreates it on every messages-sync).
const personPhotoView = `CREATE VIEW v_person_photo AS
	SELECT DISTINCT p.name AS person, m.rowid AS rowid, m.ts AS ts,
	       m.chat_title AS chat_title, m.sender_name AS sender_name,
	       './media/' || m.rowid || '.jpg' AS image_path
	FROM   wa_person_face pf
	JOIN   wa_person  p ON p.person_id = pf.person_id AND p.name <> '' AND p.hidden = 0
	JOIN   v_messages m ON m.rowid = pf.rowid`

// openPersonDB opens the workspace ChatStorage.sqlite, ensures the person
// schema + view, and runs the one-time labels.json → DB migration. Caller
// must Close the returned DB. Returns an error if there's no DB yet.
func openPersonDB(ws string) (*sql.DB, error) {
	dbPath := filepath.Join(ws, "ChatStorage.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := ensurePersonSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func appTableColumns(db *sql.DB, table string) map[string]bool {
	cols := map[string]bool{}
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return cols
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk) == nil {
			cols[name] = true
		}
	}
	return cols
}

// ensurePersonSchema creates the person tables + view if absent. It also
// drops a legacy wa_person_face (the earlier 2-column (rowid, person)
// shape this feature shipped with) so the new schema can be created.
// Steady-state this does no writes (everything IF NOT EXISTS).
func ensurePersonSchema(db *sql.DB) error {
	if c := appTableColumns(db, "wa_person_face"); c["person"] && !c["person_id"] {
		_, _ = db.Exec(`DROP TABLE IF EXISTS wa_person_face`)
		_, _ = db.Exec(`DROP VIEW IF EXISTS v_person_photo`)
	}
	if _, err := db.Exec(postprocess.PersonSidecarsSQL); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE VIEW IF NOT EXISTS` + personPhotoView[len(`CREATE VIEW`):]); err != nil {
		return err
	}
	return nil
}

// personCluster is one displayed tile: either a named person or an
// untouched auto-cluster (id "auto:<n>"). It carries the curation state
// the UI needs to render name + actions.
type personCluster struct {
	ID             string       `json:"id"` // "p<n>" (named) or "auto:<n>"
	Name           string       `json:"name"`
	Count          int          `json:"count"`
	Representative string       `json:"representative"`
	Hidden         bool         `json:"hidden"`
	Members        []faceMember `json:"members"`
}

// handleFaceClusters returns the people for the grid, with user curation
// (names, merges, hides from the wa_person tables) applied on top of the
// auto-clusters. Whole-cluster assignment: a cluster belongs to whichever
// person any of its faces is assigned to (the UI assigns entire tiles),
// so merging two tiles is just both clusters pointing at one person.
//
// `?min=1` includes singletons; `?hidden=1` includes hidden people.
func (s *server) handleFaceClusters(w http.ResponseWriter, r *http.Request) {
	cur := s.ws.get()
	if cur == "" {
		httpError(w, http.StatusBadRequest, "no workspace open")
		return
	}
	cl, err := loadClusters(facesDir(cur))
	if err != nil {
		httpError(w, http.StatusNotFound, "no clusters yet — run Find people first")
		return
	}
	minN := 2
	if r.URL.Query().Get("min") == "1" {
		minN = 1
	}
	showHidden := r.URL.Query().Get("hidden") == "1"

	// Load the user's tags from the DB: face (rowid,idx) -> person, and the
	// person registry. A missing DB just means "no tags" (everything auto).
	faceToPerson := map[string]int64{} // "rowid_idx" -> person_id
	type personInfo struct {
		name   string
		hidden bool
	}
	persons := map[int64]personInfo{}
	names := []string{}
	if db, derr := openPersonDB(cur); derr == nil {
		defer db.Close()
		if rows, e := db.Query(`SELECT person_id, name, hidden FROM wa_person`); e == nil {
			seen := map[string]bool{}
			for rows.Next() {
				var id int64
				var nm string
				var hid int
				if rows.Scan(&id, &nm, &hid) == nil {
					persons[id] = personInfo{name: nm, hidden: hid != 0}
					if nm != "" && !seen[nm] {
						seen[nm] = true
						names = append(names, nm)
					}
				}
			}
			rows.Close()
		}
		if rows, e := db.Query(`SELECT rowid, face_idx, person_id FROM wa_person_face`); e == nil {
			for rows.Next() {
				var rid, pid int64
				var idx int
				if rows.Scan(&rid, &idx, &pid) == nil {
					faceToPerson[strconv.FormatInt(rid, 10)+"_"+strconv.Itoa(idx)] = pid
				}
			}
			rows.Close()
		}
	}

	// Group at the FACE level: an assigned face goes to its person's tile,
	// an unassigned face to its auto-cluster's tile. This lets a single
	// mis-assigned photo be detached from a person (just delete its
	// wa_person_face row) without dragging its whole cluster along.
	groups := map[string]*personCluster{}
	order := []string{}
	ensureTile := func(id, name string, hidden bool) *personCluster {
		g := groups[id]
		if g == nil {
			g = &personCluster{ID: id, Name: name, Hidden: hidden}
			groups[id] = g
			order = append(order, id)
		}
		return g
	}
	for _, c := range cl.Clusters {
		autoID := "auto:" + strconv.Itoa(c.ID)
		for _, m := range c.Members {
			var g *personCluster
			if rid, idx, ok := faceKeyFromCrop(m.Crop); ok {
				if pid, ok := faceToPerson[strconv.FormatInt(rid, 10)+"_"+strconv.Itoa(idx)]; ok {
					g = ensureTile("p"+strconv.FormatInt(pid, 10), persons[pid].name, persons[pid].hidden)
				}
			}
			if g == nil {
				g = ensureTile(autoID, "", false)
			}
			g.Members = append(g.Members, m)
		}
	}
	for _, g := range groups {
		g.Count = len(g.Members)
	}

	// Representative = highest-quality member; respect min/hidden filters.
	out := struct {
		ImageCount      int             `json:"image_count"`
		ImagesWithFaces int             `json:"images_with_faces"`
		FaceCount       int             `json:"face_count"`
		ClusterCount    int             `json:"cluster_count"`
		Names           []string        `json:"names"` // existing names, for autocomplete
		People          []personCluster `json:"people"`
	}{
		ImageCount:      cl.ImageCount,
		ImagesWithFaces: cl.ImagesWithFaces,
		FaceCount:       cl.FaceCount,
		ClusterCount:    cl.ClusterCount,
		Names:           names,
		People:          []personCluster{},
	}
	for _, pid := range order {
		g := groups[pid]
		if g.Hidden && !showHidden {
			continue
		}
		if g.Name == "" && g.Count < minN {
			continue
		}
		best := -1.0
		for _, m := range g.Members {
			if m.Quality > best {
				best = m.Quality
				g.Representative = m.Crop
			}
		}
		out.People = append(out.People, *g)
	}
	sortPeopleByCount(out.People)
	writeJSON(w, http.StatusOK, out)
}

func sortPeopleByCount(p []personCluster) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].Count > p[j-1].Count; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

// faceLabelRequest is the body of POST /api/database/face-label. `crops`
// carries the affected tile's face crop paths (the UI has them).
type faceLabelRequest struct {
	Action   string   `json:"action"` // name | hide | unhide | delete | detach
	Crops    []string `json:"crops"`
	PersonID string   `json:"person_id"`
	Name     string   `json:"name"`
}

// handleFaceLabel applies one curation action directly to the workspace
// DB (wa_person + wa_person_face). Naming is the only grouping primitive:
// a person name is unique, so naming a group with a name that already
// exists MERGES this group into that person. Empty name ungroups.
//
// Tile ids: "p<person_id>" for a saved person, "auto:<n>" for an
// untouched cluster (parsePID returns 0 for the latter → "create one").
func (s *server) handleFaceLabel(w http.ResponseWriter, r *http.Request) {
	cur := s.ws.get()
	if cur == "" {
		httpError(w, http.StatusBadRequest, "no workspace open")
		return
	}
	var req faceLabelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	personMu.Lock()
	defer personMu.Unlock()
	db, err := openPersonDB(cur)
	if err != nil {
		httpError(w, http.StatusBadRequest, "no database in workspace yet")
		return
	}
	defer db.Close()

	parsePID := func(id string) int64 {
		if strings.HasPrefix(id, "p") {
			n, _ := strconv.ParseInt(id[1:], 10, 64)
			return n
		}
		return 0
	}
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := db.Begin()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "begin: "+err.Error())
		return
	}
	defer tx.Rollback() //nolint:errcheck

	assign := func(crops []string, pid int64) error {
		for _, c := range crops {
			rid, idx, ok := faceKeyFromCrop(c)
			if !ok {
				continue
			}
			if _, e := tx.Exec(`INSERT OR REPLACE INTO wa_person_face(rowid, face_idx, person_id) VALUES(?,?,?)`, rid, idx, pid); e != nil {
				return e
			}
		}
		return nil
	}

	var resultID string
	var actErr error
	switch req.Action {
	case "name":
		name := normName(req.Name)
		curPID := parsePID(req.PersonID)
		if name == "" { // ungroup
			if curPID > 0 {
				_, _ = tx.Exec(`DELETE FROM wa_person_face WHERE person_id=?`, curPID)
				_, _ = tx.Exec(`DELETE FROM wa_person WHERE person_id=?`, curPID)
			}
			break
		}
		var target int64
		_ = tx.QueryRow(`SELECT person_id FROM wa_person WHERE name=?`, name).Scan(&target)
		if target == 0 {
			if curPID > 0 { // rename the current person
				_, actErr = tx.Exec(`UPDATE wa_person SET name=?, updated_at=? WHERE person_id=?`, name, now, curPID)
				target = curPID
			} else { // brand-new person
				res, e := tx.Exec(`INSERT INTO wa_person(name, updated_at) VALUES(?,?)`, name, now)
				if e != nil {
					actErr = e
				} else {
					target, _ = res.LastInsertId()
				}
			}
		}
		if actErr == nil {
			actErr = assign(req.Crops, target)
		}
		// If the tile was a different saved person, fold its remaining
		// faces into the target and drop the now-empty source.
		if actErr == nil && curPID > 0 && curPID != target {
			_, _ = tx.Exec(`UPDATE wa_person_face SET person_id=? WHERE person_id=?`, target, curPID)
			_, _ = tx.Exec(`DELETE FROM wa_person WHERE person_id=?`, curPID)
		}
		resultID = "p" + strconv.FormatInt(target, 10)
	case "hide":
		pid := parsePID(req.PersonID)
		if pid == 0 { // hiding an untouched cluster: materialise it first
			res, e := tx.Exec(`INSERT INTO wa_person(name, updated_at) VALUES('', ?)`, now)
			if e != nil {
				actErr = e
			} else {
				pid, _ = res.LastInsertId()
				actErr = assign(req.Crops, pid)
			}
		}
		if actErr == nil {
			_, actErr = tx.Exec(`UPDATE wa_person SET hidden=1 WHERE person_id=?`, pid)
		}
		resultID = "p" + strconv.FormatInt(pid, 10)
	case "unhide":
		_, actErr = tx.Exec(`UPDATE wa_person SET hidden=0 WHERE person_id=?`, parsePID(req.PersonID))
		resultID = req.PersonID
	case "detach":
		// Pull specific faces out of their person (the "this photo isn't
		// them" fix): just un-assign them. They revert to their auto-
		// cluster. The empty-person prune below cleans up if none remain.
		for _, c := range req.Crops {
			if rid, idx, ok := faceKeyFromCrop(c); ok {
				if _, e := tx.Exec(`DELETE FROM wa_person_face WHERE rowid=? AND face_idx=?`, rid, idx); e != nil {
					actErr = e
					break
				}
			}
		}
		resultID = req.PersonID
	case "delete":
		pid := parsePID(req.PersonID)
		_, _ = tx.Exec(`DELETE FROM wa_person_face WHERE person_id=?`, pid)
		_, actErr = tx.Exec(`DELETE FROM wa_person WHERE person_id=?`, pid)
	default:
		httpError(w, http.StatusBadRequest, "unknown action: "+req.Action)
		return
	}
	if actErr != nil {
		httpError(w, http.StatusInternalServerError, "tag: "+actErr.Error())
		return
	}
	// Drop people left with no faces (e.g. merged away), but keep hidden
	// ones (a hidden person may legitimately have had its faces removed).
	_, _ = tx.Exec(`DELETE FROM wa_person
		WHERE hidden=0 AND person_id NOT IN (SELECT DISTINCT person_id FROM wa_person_face)`)
	if err := tx.Commit(); err != nil {
		httpError(w, http.StatusInternalServerError, "commit: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"person_id": resultID})
}

// handleFaceCrop serves one face-crop thumbnail from <ws>/faces/crops.
// The path is restricted to that directory (basename only, no traversal)
// so the endpoint can't be coaxed into serving arbitrary files.
func (s *server) handleFaceCrop(w http.ResponseWriter, r *http.Request) {
	cur := s.ws.get()
	if cur == "" {
		httpError(w, http.StatusBadRequest, "no workspace open")
		return
	}
	name := r.PathValue("name")
	// Reject anything that isn't a plain crop filename. The helper only
	// ever writes "<stem>_<idx>.jpg" under crops/.
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		httpError(w, http.StatusBadRequest, "bad crop name")
		return
	}
	p := filepath.Join(facesDir(cur), "crops", name)
	if _, err := os.Stat(p); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, p)
}

// countImages returns how many image files live directly in dir. Mirrors
// the helper's extension set so the card's "N photos" matches what the
// clusterer will actually scan.
func countImages(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".jpg", ".jpeg", ".png", ".heic", ".heif", ".gif":
			n++
		}
	}
	return n
}
