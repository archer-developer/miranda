package agentloop

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/history"
)

// downloadedFile is one file executeTool's detector found and staged this
// turn — everything toDownloadRefs needs to build the turn's structured
// history.DownloadRef entry for it. filename/sizeBytes/mimeType are
// best-effort (see detectRemoteFileLinks) and may be zero/empty; the web
// UI's downloadChip already degrades cleanly when they are.
type downloadedFile struct {
	fileID    string
	filename  string
	sizeBytes int64
	mimeType  string
}

// remoteFileLink is one URL executeTool found rooted at a file-exposing
// server's own FilesEndpoint inside an MCP tool's raw result text, plus
// whatever best-effort metadata could be read from sibling JSON fields next
// to it — see detectRemoteFileLinks. This is the one shape every
// file-exposing server's download flows into, sandbox included: there is
// deliberately no second, tool-specific shape.
type remoteFileLink struct {
	url       string
	filename  string
	mimeType  string
	sizeBytes int64
}

// detectRemoteFileLinks scans an MCP tool call's raw result text for every
// absolute URL rooted at endpoint.FilesURL — e.g. a "fileUri" field in
// miranda-medical-card's medical.get_document response
// ("https://127.0.0.1:8791/files/file_49948e03-...") — and returns one entry
// per distinct URL found, with best-effort filename/mime_type/size_bytes
// recovered two ways depending on the result's shape: for a JSON result,
// from sibling fields in whichever object the URL itself came from (see
// findSiblingObject); for a plain-text result — e.g.
// miranda-code-execution-sandbox's download_file, which reports
// "file_id: ...\nfile_uri: ...\nfilename: ...\n..." rather than JSON — from
// "key: value" lines instead (see keyValueStringField/keyValueInt64Field).
// Both paths share the same priority-ordered key lists
// (filenameKeys/mimeTypeKeys/sizeBytesKeys) since the two shapes happen to
// use the same field names for this data. Detection of the URL itself is
// purely by shape, never by tool name, so it works for any tool on a server
// opted into config.MCPServer.ExposeFiles, present or future, without
// Miranda needing to know that tool's schema — including the sandbox's own
// download_file, which this same function covers now that tool's result
// carries a full file_uri rather than a bare id.
//
// Only a URL whose prefix matches endpoint.FilesURL exactly is ever treated
// as a file reference — this is the security boundary between "a trusted,
// explicitly opted-in server's own file link" and an arbitrary URL a
// compromised/malicious tool result could otherwise smuggle in for
// handleDownload to proxy (with that server's own bearer token attached) to
// wherever the attacker chose. See config.MCPServer.ExposeFiles.
func detectRemoteFileLinks(result string, endpoint config.FileServerEndpoint) []remoteFileLink {
	prefix := strings.TrimRight(endpoint.FilesURL, "/") + "/"
	// RFC 3986 unreserved characters plus "%" for percent-encoding, so a
	// file id containing e.g. an extension suffix or an encoded character
	// isn't truncated mid-id (a truncated RemoteURL would still get staged
	// and chipped, then 404/502 on click — see git history).
	pattern := regexp.MustCompile(regexp.QuoteMeta(prefix) + `[A-Za-z0-9._~%-]+`)

	var doc interface{}
	hasJSON := json.Unmarshal([]byte(result), &doc) == nil

	seen := make(map[string]bool)
	var links []remoteFileLink
	for _, u := range pattern.FindAllString(result, -1) {
		if seen[u] {
			continue
		}
		seen[u] = true
		link := remoteFileLink{url: u}
		if hasJSON {
			if obj := findSiblingObject(doc, u); obj != nil {
				link.filename = stringSiblingField(obj, filenameKeys)
				link.mimeType = stringSiblingField(obj, mimeTypeKeys)
				link.sizeBytes = int64SiblingField(obj, sizeBytesKeys)
			}
		} else {
			link.filename = keyValueStringField(result, filenameKeys)
			link.mimeType = keyValueStringField(result, mimeTypeKeys)
			link.sizeBytes = keyValueInt64Field(result, sizeBytesKeys)
		}
		links = append(links, link)
	}
	return links
}

// filenameKeys/mimeTypeKeys/sizeBytesKeys are the priority-ordered field
// names treated as describing a file — as JSON object keys for
// findSiblingObject's caller (e.g. medical_card_medical.get_document's
// "title"), or as "key: value" line prefixes for
// keyValueStringField/keyValueInt64Field (e.g. the sandbox's download_file
// "filename: ..."/"mime_type: ..."/"size_bytes: ..." lines — deliberately
// the same lists, since both shapes happen to use these exact field names).
// A miss on all of them leaves that piece of metadata unset rather than
// guessed wrong; the download chip (downloadChip in downloads.js) already
// degrades cleanly when filename/size are empty/zero.
var (
	filenameKeys  = []string{"title", "filename", "fileName", "name", "documentTitle"}
	mimeTypeKeys  = []string{"mime_type", "mimeType", "contentType", "content_type"}
	sizeBytesKeys = []string{"size_bytes", "sizeBytes", "fileSize", "size"}
)

// findSiblingObject recursively walks a generically decoded JSON value and
// returns the object (map) that holds target as one of its own string
// field values — e.g. for {"title":"x","fileUri":target}, returns that
// whole map so the caller can then read whichever other fields it wants
// out of it. Returns nil if target isn't found anywhere.
func findSiblingObject(node interface{}, target string) map[string]interface{} {
	switch v := node.(type) {
	case map[string]interface{}:
		for _, val := range v {
			if s, ok := val.(string); ok && s == target {
				return v
			}
		}
		for _, val := range v {
			if obj := findSiblingObject(val, target); obj != nil {
				return obj
			}
		}
	case []interface{}:
		for _, item := range v {
			if obj := findSiblingObject(item, target); obj != nil {
				return obj
			}
		}
	}
	return nil
}

// stringSiblingField returns the first non-empty string value found in obj
// under any of keys, in order, or "" if none match.
func stringSiblingField(obj map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if s, ok := obj[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// int64SiblingField returns the first positive numeric value found in obj
// under any of keys, in order, or 0 if none match. JSON numbers decode as
// float64 via encoding/json's default interface{} unmarshaling.
func int64SiblingField(obj map[string]interface{}, keys []string) int64 {
	for _, k := range keys {
		if n, ok := obj[k].(float64); ok && n > 0 {
			return int64(n)
		}
	}
	return 0
}

// keyValueStringField is detectRemoteFileLinks' non-JSON counterpart to
// stringSiblingField: it scans result line by line for the first
// "key: value" line (colon-space separated, the shape
// miranda-code-execution-sandbox's download_file and similar plain-text
// tool results use) whose key matches any of keys, in priority order, and
// returns that value. Unlike stringSiblingField there's no JSON object to
// scope the search to, so this scans the whole result — fine in practice,
// since every known plain-text result of this shape describes exactly one
// file. Returns "" if no line matches.
func keyValueStringField(result string, keys []string) string {
	for _, line := range strings.Split(result, "\n") {
		key, value, found := strings.Cut(line, ": ")
		if !found || value == "" {
			continue
		}
		for _, k := range keys {
			if key == k {
				return value
			}
		}
	}
	return ""
}

// keyValueInt64Field is keyValueStringField's numeric counterpart, mirroring
// int64SiblingField: returns the first positive integer value found on a
// matching "key: value" line, or 0 if none match or parse.
func keyValueInt64Field(result string, keys []string) int64 {
	for _, line := range strings.Split(result, "\n") {
		key, value, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		for _, k := range keys {
			if key != k {
				continue
			}
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// toDownloadRefs converts the turn's detected files to the form
// history.AppendAssistantMessage/InputResponse/ChatEvent carry them in —
// structurally separate from the reply's own text (see
// history.Message.Downloads for why). Replacing the earlier design (an
// in-band "\n\n<download>{json}</download>" marker appended to the reply
// text itself) is what actually closes the failure mode that design had:
// once such a marker had been written into a conversation's history, it
// came back as plain assistant text in every later turn's model-facing
// context, and a model was observed pattern-matching that exact tag shape
// back into a new reply of its own — filling it in with the wrong id (e.g.
// the sandbox's raw internal file_id rather than the attachments-store id)
// and producing a second, broken chip alongside the real one. A model can
// only mimic a shape it has actually seen; since a downloaded file is never
// represented as any kind of tag in message content anymore — not in what
// gets replayed as history, not even in the current turn's own reply before
// this function's caller attaches it — there is nothing left in text for it
// to copy. See git history for the incident and the two narrower
// text-marker-patching attempts (strip-on-append, then keep-latest-wins
// dedup) this replaced.
func toDownloadRefs(files []downloadedFile) []history.DownloadRef {
	if len(files) == 0 {
		return nil
	}
	refs := make([]history.DownloadRef, len(files))
	for i, f := range files {
		refs[i] = history.DownloadRef{FileID: f.fileID, Filename: f.filename, SizeBytes: f.sizeBytes, MIMEType: f.mimeType}
	}
	return refs
}

// appendDownloadFootnotes appends one human-readable "📎 filename (size)"
// line per downloaded file to text, for channels with no client-side chip
// renderer to hand structured data to instead. Only the web UI's chat
// screen renders a chip from InputResponse/ChatEvent's own Downloads field
// (see toDownloadRefs) — every other channel (ha_assist speaks/shows the
// reply text as-is, Telegram sends it verbatim via the Bot API) needs the
// file mentioned in the text itself or the user has no way to know it
// exists.
func appendDownloadFootnotes(text string, files []downloadedFile, source string) string {
	if source == WebUISource {
		return text
	}
	for _, f := range files {
		if f.sizeBytes > 0 {
			text += fmt.Sprintf("\n\n📎 %s (%s)", f.filename, formatByteSize(f.sizeBytes))
		} else {
			text += fmt.Sprintf("\n\n📎 %s", f.filename)
		}
	}
	return text
}

// formatByteSize renders a byte count as a short human-readable string,
// mirroring the web UI's formatFileSize (internal/webui/static/js/downloads.js)
// for the plain-text fallback appendDownloadFootnotes produces.
func formatByteSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", n/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
