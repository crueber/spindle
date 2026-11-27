package runner

import "bytes"

// markerBuffer is the test sink for a commandSink: plain text plus the two
// structural group markers, rendered as the lines a text-only reader sees (the
// production sink is a Streamer/bufferStream, whose markers are typed frames).
type markerBuffer struct {
	bytes.Buffer
}

func (m *markerBuffer) Group(name string) {
	_, _ = m.WriteString("▸ group: " + name + "\n")
}

func (m *markerBuffer) GroupEnd() {
	_, _ = m.WriteString("◂ group end\n")
}
