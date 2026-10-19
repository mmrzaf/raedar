package docker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"
)

// frameHeader is the size of one Docker stream frame header: the stream type,
// three zero bytes and the big-endian payload length.
const frameHeader = 8

// maxFrame is the largest stream frame Raedar accepts.
const maxFrame = 1 << 20

// LogOptions select the part of a container's log to read.
type LogOptions struct {
	// Follow keeps reading as the container writes.
	Follow bool
	// Timestamps prefixes every line with its time.
	Timestamps bool
	// Since starts at this time; the zero time starts at the beginning.
	Since time.Time
	// TTY is the container's Tty setting. Without a TTY the daemon frames the
	// stream and the framing is removed.
	TTY bool
}

// Logs returns a reader over one container's standard output and error. The
// caller closes the reader.
func (c *Client) Logs(ctx context.Context, id string, options LogOptions) (io.ReadCloser, error) {
	query := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	if options.Follow {
		query.Set("follow", "1")
	}
	if options.Timestamps {
		query.Set("timestamps", "1")
	}
	if !options.Since.IsZero() {
		query.Set("since", strconv.FormatInt(options.Since.Unix(), 10))
	}
	resp, err := c.get(ctx, "/containers/"+url.PathEscape(id)+"/logs", query, true)
	if err != nil {
		return nil, err
	}
	if options.TTY {
		return resp.Body, nil
	}
	return newDemuxReader(resp.Body), nil
}

// demuxReader removes Docker's stream framing from a container log stream.
type demuxReader struct {
	source io.Reader
	frame  []byte
	err    error
}

// newDemuxReader returns a reader over the payloads of a framed stream.
func newDemuxReader(source io.Reader) *demuxReader {
	return &demuxReader{source: source}
}

// Read returns the next bytes of the container's output.
func (r *demuxReader) Read(p []byte) (int, error) {
	for len(r.frame) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if err := r.nextFrame(); err != nil {
			r.err = err
			return 0, err
		}
	}
	n := copy(p, r.frame)
	r.frame = r.frame[n:]
	return n, nil
}

// nextFrame reads one frame header and payload.
func (r *demuxReader) nextFrame() error {
	header := make([]byte, frameHeader)
	if _, err := io.ReadFull(r.source, header); err != nil {
		return err
	}
	stream := header[0]
	if stream != 0 && stream != 1 && stream != 2 {
		return fmt.Errorf("docker: unknown log stream type %d", stream)
	}
	size := binary.BigEndian.Uint32(header[4:])
	if size > maxFrame {
		return fmt.Errorf("docker: log frame of %d bytes is too large", size)
	}
	r.frame = make([]byte, size)
	_, err := io.ReadFull(r.source, r.frame)
	return err
}

// Close closes the underlying stream.
func (r *demuxReader) Close() error {
	if closer, ok := r.source.(io.Closer); ok {
		return closer.Close()
	}
	return errors.New("docker: the log stream cannot be closed")
}
