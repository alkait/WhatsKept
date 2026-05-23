package backup

import (
	"regexp"
	"strconv"
	"strings"
)

// WhatsAppProfilePrefix is the manifest prefix every WhatsApp profile
// picture lives under inside an iOS backup. Files are named:
//
//	Media/Profile/<phone>-<picid>.jpg            -- DM/user, full-res
//	Media/Profile/<phone>-<picid>.thumb          -- DM/user, cached preview
//	Media/Profile/<digits>-<digits>-<picid>.jpg  -- legacy group (<creator>-<created-ts>@g.us)
//
// LIDs (<digits>@lid) and new-style group JIDs (<digits>@g.us) share the
// 2-segment form — we can't tell which from the filename alone, so we
// resolve the prefix against the set of known JIDs in ChatStorage.
const WhatsAppProfilePrefix = "Media/Profile/"

// ProfileFile is one WhatsApp profile-picture record after the manifest
// path has been parsed and resolved against the chat DB.
type ProfileFile struct {
	Record    Record
	JID       string // <phone>@s.whatsapp.net | <digits>@lid | <...>@g.us
	PictureID string // last numeric segment of the filename
	Kind      string // "jpg" (full-res) or "thumb" (cached preview)
}

// Two-segment Media/Profile/<prefix>-<picid>.<ext>. The prefix may be a
// phone JID, a LID, or a new-style group ID — caller disambiguates.
var profileTwoSegRE = regexp.MustCompile(`^Media/Profile/(\d+)-(\d+)\.(jpg|thumb)$`)

// Three-segment Media/Profile/<a>-<b>-<picid>.<ext> is the older group
// JID format: <creator-phone>-<group-create-ts>@g.us.
var profileThreeSegRE = regexp.MustCompile(`^Media/Profile/(\d+)-(\d+)-(\d+)\.(jpg|thumb)$`)

// parseProfilePath turns a Media/Profile/... manifest path into a
// (jid, picture_id, kind) triple, resolved against `knownJIDs`.
//
// Returns nil for filenames whose shape we don't recognise (other asset
// types occasionally land in the directory) or whose prefix matches no
// known JID — we'd otherwise extract avatars for forwarded contact cards
// the user has never actually messaged.
//
// If knownJIDs is nil/empty, 2-segment names are assumed phone-JID and
// 3-segment names old-style group-JID, matching the Python fallback.
func parseProfilePath(rel string, knownJIDs map[string]struct{}) (jid, picID, kind string, ok bool) {
	if m := profileThreeSegRE.FindStringSubmatch(rel); m != nil {
		creator, createdTS, picID, ext := m[1], m[2], m[3], m[4]
		jid := creator + "-" + createdTS + "@g.us"
		if knownJIDs != nil {
			if _, hit := knownJIDs[jid]; !hit {
				return "", "", "", false
			}
		}
		return jid, picID, ext, true
	}
	if m := profileTwoSegRE.FindStringSubmatch(rel); m != nil {
		prefix, picID, ext := m[1], m[2], m[3]
		if knownJIDs == nil {
			return prefix + "@s.whatsapp.net", picID, ext, true
		}
		// Resolve by exact membership in the known set. Order doesn't
		// matter (the JID's suffix makes each candidate unique).
		for _, cand := range []string{
			prefix + "@s.whatsapp.net",
			prefix + "@lid",
			prefix + "@g.us",
		} {
			if _, hit := knownJIDs[cand]; hit {
				return cand, picID, ext, true
			}
		}
		return "", "", "", false
	}
	return "", "", "", false
}

// ListProfileFiles walks the bundle's manifest for every Media/Profile
// file in the WhatsApp shared domain and returns the best pick per JID.
// Preference order per JID: highest-picture_id `.jpg` (full-res) wins;
// otherwise highest-picture_id `.thumb`.
//
// knownJIDs filters out cached avatars for randos (forwarded contact
// cards, group-of-groups members the user never DM'd, etc.) and
// disambiguates 2-segment filenames between phone/LID/group forms.
// Pass an empty map only if you genuinely want everything.
func ListProfileFiles(b *Bundle, knownJIDs map[string]struct{}) []ProfileFile {
	byJID := make(map[string]ProfileFile)
	for _, rec := range b.mb.Records {
		if rec.Domain != WhatsAppDomain {
			continue
		}
		if !strings.HasPrefix(rec.Path, WhatsAppProfilePrefix) {
			continue
		}
		jid, picID, kind, ok := parseProfilePath(rec.Path, knownJIDs)
		if !ok {
			continue
		}
		cand := ProfileFile{Record: rec, JID: jid, PictureID: picID, Kind: kind}
		existing, have := byJID[jid]
		if !have || profileBetter(cand, existing) {
			byJID[jid] = cand
		}
	}
	out := make([]ProfileFile, 0, len(byJID))
	for _, f := range byJID {
		out = append(out, f)
	}
	return out
}

// profileBetter returns true when `cand` should replace `existing`:
// full-res beats thumb; same-kind tie broken by higher picture_id (more
// recent upload). Non-numeric picture_ids are treated as oldest.
func profileBetter(cand, existing ProfileFile) bool {
	if cand.Kind == "jpg" && existing.Kind != "jpg" {
		return true
	}
	if cand.Kind != "jpg" && existing.Kind == "jpg" {
		return false
	}
	c, cerr := strconv.Atoi(cand.PictureID)
	e, eerr := strconv.Atoi(existing.PictureID)
	if cerr != nil || eerr != nil {
		return false
	}
	return c > e
}
