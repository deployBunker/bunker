package server

// GAP-094 helper: an encoding-aware, cap-aware wrapper around the exec stream
// sink's per-frame sender.
//
// Design notes:
//   - TEXT passes bytes through unchanged; that is the compatibility proof
//     surface (no flag -> byte-identical to pre-GAP-094).
//   - BASE64 encodes each frame independently (standard encoding, no line
//     wrapping) so the stream stays decodable frame-by-frame.
//   - The cap counts RAW bytes (pre-encoding); the notice names the raw total,
//     the cap, and the remedy, and is emitted ONLY on the final frame.
//   - This file exists so the exec-path change can be unit-tested without
//     spinning the whole service.

import (
	"encoding/base64"
	"fmt"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ExecResponseCapBytes is the default per-direction raw cap. LARGE by design:
// the cap's job is to stop a 50MB runaway search, not to squeeze normal build
// logs (the DF-BUNKER-27 tests stream ~74KB and require completeness). The
// wire cap is per-direction and counts RAW bytes before encoding.
const ExecResponseCapBytes = 512 << 20 // 512 MiB

// execEncodingWriter wraps the frame sender with encoding + truncation.
type execEncodingWriter struct {
	sender   *execStreamSender
	encoding v1.ExecEncoding
	capBytes int
	stderr   bool

	sent      bool // raw bytes actually delivered
	truncated bool
	total     int
}

// Write encodes and forwards one chunk, honoring the cap. Returns (written, nil)
// always: a truncation is recorded, not an error, so the child's exit-code
// semantics are preserved.
func (w *execEncodingWriter) Write(p []byte) (int, error) {
	n := len(p)
	total := w.total + n
	if total > w.capBytes {
		keep := w.capBytes - w.total
		if keep < 0 {
			keep = 0
		}
		w.truncated = true
		w.total = total
		if keep == 0 {
			return n, nil
		}
		// Deliver the final within-cap slice; sent is bookkeeping only and
		// must NEVER gate the send itself (a `sent ||` short-circuit here
		// silently dropped every frame after the first).
		w.sender.streamFrames(w.encoding, p[:keep], w.stderr)
		w.sent = true
		return n, nil
	}
	w.total = total
	w.sender.streamFrames(w.encoding, p, w.stderr)
	if n > 0 {
		w.sent = true
	}
	return n, nil
}

// finalNotice builds the truncation notice for the final frame, or "" when
// nothing was dropped. The notice names the cap and HOW to get the rest —
// narrowing (grep, head, writing to a file to fetch via cp) — never silently.
func (w *execEncodingWriter) finalNotice() string {
	if !w.truncated {
		return ""
	}
	narrowed := w.total - w.capBytes
	return fmt.Sprintf(
		"output truncated: cap is %d bytes, %d more were produced but not sent; narrow the command (grep/head/awk, or write to a file and fetch it with bunker cp), or raise --exec-cap",
		w.capBytes, narrowed)
}

// streamFrames is the shared frame path: raw bytes for TEXT, base64 per-frame
// for BASE64. Returns whether anything was sent.
func (s *execStreamSender) streamFrames(encoding v1.ExecEncoding, frame []byte, stderr bool) bool {
	if stderr {
		switch encoding {
		case v1.ExecEncoding_EXEC_ENCODING_BASE64:
			s.send("send stderr", &v1.ExecAgentResponse{
				Output: &v1.ExecAgentResponse_Stderr{Stderr: []byte(base64.StdEncoding.EncodeToString(frame))},
			})
		default: // TEXT: byte-identical legacy path
			c := make([]byte, len(frame))
			copy(c, frame)
			s.send("send stderr", &v1.ExecAgentResponse{Output: &v1.ExecAgentResponse_Stderr{Stderr: c}})
		}
	} else {
		switch encoding {
		case v1.ExecEncoding_EXEC_ENCODING_BASE64:
			s.send("send stdout", &v1.ExecAgentResponse{
				Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte(base64.StdEncoding.EncodeToString(frame))},
			})
		default:
			c := make([]byte, len(frame))
			copy(c, frame)
			s.send("send stdout", &v1.ExecAgentResponse{Output: &v1.ExecAgentResponse_Stdout{Stdout: c}})
		}
	}
	return true
}
