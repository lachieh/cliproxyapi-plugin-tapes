package capture

import "bytes"

// Stream chunks reach plugins in three shapes, depending on the client protocol:
//   - OpenAI Chat Completions: one bare JSON object per chunk; the handler adds
//     the "data: " framing when writing to the client.
//   - Claude Messages: already-framed SSE ("event: ...\ndata: ...\n\n").
//   - OpenAI Responses: SSE text that may split or merge frames arbitrarily;
//     the handler re-frames it on the way out.
//
// appendSSE rebuilds the SSE stream the client received, so tapes can reduce
// it with the same reducers its own proxy uses.
func appendSSE(stream, chunk []byte) []byte {
	if len(bytes.TrimSpace(chunk)) == 0 {
		// Blank separators are dropped; frames are re-terminated as needed below.
		return stream
	}
	atBoundary := len(stream) == 0 || bytes.HasSuffix(stream, []byte("\n\n"))

	field := bytes.TrimLeft(chunk, "\r\n")
	if !startsWithSSEField(field) {
		if atBoundary {
			stream = append(stream, "data: "...)
			stream = append(stream, bytes.TrimSpace(chunk)...)
			return append(stream, "\n\n"...)
		}
		// The rest of a frame split across chunks.
		return append(stream, chunk...)
	}
	chunk = field

	if !atBoundary && startsNewFrame(currentFrame(stream), chunk) {
		stream = terminateFrame(stream)
	} else if len(stream) > 0 && stream[len(stream)-1] != '\n' {
		stream = append(stream, '\n')
	}
	return append(stream, chunk...)
}

// terminateFrame ends the frame in progress so the next field starts a new event.
func terminateFrame(stream []byte) []byte {
	for len(stream) > 0 && !bytes.HasSuffix(stream, []byte("\n\n")) {
		stream = append(stream, '\n')
	}
	return stream
}

func currentFrame(stream []byte) []byte {
	if i := bytes.LastIndex(stream, []byte("\n\n")); i >= 0 {
		return stream[i+2:]
	}
	return stream
}

// startsNewFrame reports whether chunk must begin a new event rather than
// extend frame: an "event:" line always starts one, and a second "data:" line
// would otherwise be concatenated into the first event's JSON.
func startsNewFrame(frame, chunk []byte) bool {
	if bytes.HasPrefix(chunk, []byte("event:")) {
		return true
	}
	return bytes.HasPrefix(chunk, []byte("data:")) &&
		(bytes.HasPrefix(frame, []byte("data:")) || bytes.Contains(frame, []byte("\ndata:")))
}

func startsWithSSEField(chunk []byte) bool {
	for _, field := range [][]byte{[]byte("data:"), []byte("event:"), []byte("id:"), []byte("retry:"), []byte(":")} {
		if bytes.HasPrefix(chunk, field) {
			return true
		}
	}
	return false
}
