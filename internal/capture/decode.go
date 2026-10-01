package capture

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/Tuskira/claude-desktop-utility/internal/protodec"
)

// DecodeProto decodes a proto-typed body (see protodec.KindOf) into compact
// JSON. raw is the captured bytes; a gzip Content-Encoding is undone first.
// It returns nil for other content types. A non-empty string describes a
// failure or a partial result.
func DecodeProto(contentType, contentEncoding string, raw []byte, truncated bool, limit int64) (json.RawMessage, string) {
	kind := protodec.KindOf(contentType)
	if kind == protodec.KindNone {
		return nil, ""
	}
	data := raw
	if strings.EqualFold(strings.TrimSpace(contentEncoding), "gzip") && len(raw) > 0 {
		if dec, more, ok := gunzip(raw, limit); ok {
			data = dec
			truncated = truncated || more
		}
	}
	return protodec.DecodeBody(kind, data, truncated)
}

// AddDecodeError appends "side: msg" to DecodeError.
func (r *Record) AddDecodeError(side, msg string) {
	if msg == "" {
		return
	}
	if r.DecodeError != "" {
		r.DecodeError += "; "
	}
	r.DecodeError += side + ": " + msg
}

// HeaderValue returns the first value of a header, ignoring case.
func HeaderValue(h map[string][]string, key string) string {
	for k, v := range h {
		if strings.EqualFold(k, key) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// bodyBytes turns a logged body string back into bytes.
func bodyBytes(body string, isBase64 bool) []byte {
	if isBase64 {
		if b, err := base64.StdEncoding.DecodeString(body); err == nil {
			return b
		}
	}
	return []byte(body)
}

// Decoded recomputes the protobuf decoding of both sides from the logged
// bodies (useful for capture files written before decoding existed).
// A nil result means that side is not a proto content type.
func (r *Record) Decoded() (req, resp json.RawMessage, errs string) {
	tmp := &Record{}
	req, e := DecodeProto(HeaderValue(r.ReqHeaders, "Content-Type"), "", bodyBytes(r.ReqBody, r.ReqBodyBase64), r.ReqBodyTruncated, 1<<62)
	tmp.AddDecodeError("req", e)
	resp, e = DecodeProto(HeaderValue(r.RespHeaders, "Content-Type"), "", bodyBytes(r.RespBody, r.RespBodyBase64), r.RespBodyTruncated, 1<<62)
	tmp.AddDecodeError("resp", e)
	return req, resp, tmp.DecodeError
}
