package claudedesktop

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
)

// idLike checks whether a path segment is "an ID": either a short
// alphabetic prefix (e.g. "cse", "session", "msg", "toolu") followed by an
// underscore and a generated tail, or a bare UUID. Used to replace IDs with
// "{id}" in the path field, per the design ("path has IDs replaced with
// {id}").
// at all: it must contain a digit and be at least 6 characters, or be a
// dotted UUID. This avoids mangling ordinary path words like "sessions" or
// "events".
var idLike = regexp.MustCompile(`(?i)^[a-z0-9]{2,12}_[0-9a-zA-Z_-]{4,}$|^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// scrubPath replaces ID-shaped path segments with "{id}", keeping query
// parameters off entirely (they often carry organization/session IDs too,
// and are not needed for grouping in the console).
func scrubPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	segs := strings.Split(u.Path, "/")
	for i, s := range segs {
		if s == "" {
			continue
		}
		if idLike.MatchString(s) {
			segs[i] = "{id}"
		}
	}
	return strings.Join(segs, "/")
}

// bareID strips a known "<prefix>_" wrapper (e.g. "cse_", "session_") from
// an ID, returning the tail that is shared across the different prefixes
// Claude Desktop uses for what is, underneath, the same code session (see
// codetab.go doc comment). Unknown prefixes are returned unchanged.
func bareID(id string) string {
	if i := strings.IndexByte(id, '_'); i > 0 && i < len(id)-1 {
		return id[i+1:]
	}
	return id
}

// icpRequestID builds the "icp_" + upstream message ID request_id used for
// Code tab LLM records.
func icpRequestID(messageID string) string {
	return "icp_" + messageID
}

// icpChatRequestID builds the "icp_chat_" + stable-ID request_id used for
// Chat tab LLM records, per the design's "icp_chat_ plus a stable ID from
// the stream, or a hash of the conversation ID and block ID".
func icpChatRequestID(stableID string) string {
	if stableID == "" {
		return ""
	}
	return "icp_chat_" + stableID
}

// hashID hashes parts into a short stable hex ID, for the fallback case
// where no stream-provided ID is available.
func hashID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
