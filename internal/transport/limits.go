package transport

import "io"

// MaxUpstreamBody bounds what an instrumentation read pulls an upstream body
// into memory. 64 MiB is the cap internal/cloudcode already puts on a Cloud
// Code response body, and a completion that large does not occur; the read
// exists to find the usage fields. An error envelope is short by nature, so it
// gets the smaller bound. Declared once so the gateway caps cannot drift.
const (
	MaxUpstreamBody = 64 << 20
	MaxErrorBody    = 1 << 20
)

// ReadCapped reads at most limit bytes of r. It asks for one byte more, so a
// body of exactly limit bytes is whole and a longer one is reported as
// truncated with data cut at limit. A read error is returned with the bytes
// read so far.
func ReadCapped(r io.Reader, limit int64) (data []byte, truncated bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(data)) > limit {
		return data[:limit], true, err
	}
	return data, false, err
}
