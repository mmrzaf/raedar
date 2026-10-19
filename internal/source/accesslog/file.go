package accesslog

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// errNoLine reports that the file has no complete line available.
var errNoLine = errors.New("no complete line")

// filePosition is the reader position in file mode.
type filePosition struct {
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

// follower reads one access log file, following it like tail -F. It detects
// rotation by inode change and truncation by a size smaller than the current
// offset. After a rotation the file that was rotated away is read to its end
// before the follower switches to the file that replaced it.
type follower struct {
	path    string
	file    *os.File
	reader  *bufio.Reader
	pending []byte
	inode   uint64
	offset  int64

	// replacement is the file that was put in place after a rotation.
	replacement *os.File
}

// openFollower opens the access log file. When position names the current
// file, reading resumes at its offset; otherwise reading starts at the end of
// the file.
func openFollower(path string, position *filePosition) (*follower, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open access log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat access log: %w", err)
	}
	offset := info.Size()
	if position != nil && position.Inode == inodeOf(info) && position.Offset <= info.Size() && position.Offset >= 0 {
		offset = position.Offset
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		file.Close()
		return nil, fmt.Errorf("seek access log: %w", err)
	}
	return &follower{
		path:   path,
		file:   file,
		reader: bufio.NewReader(file),
		inode:  inodeOf(info),
		offset: offset,
	}, nil
}

// next returns the next complete line and the position after it, or errNoLine
// when no complete line is available yet.
func (f *follower) next() (string, filePosition, error) {
	if err := f.checkRotation(); err != nil {
		return "", filePosition{}, err
	}
	for {
		text, position, err := f.readLine()
		if err == nil {
			return text, position, nil
		}
		if !errors.Is(err, errNoLine) {
			return "", filePosition{}, err
		}
		if f.grew() {
			continue // more bytes arrived in the file being read
		}
		if f.replacement != nil && hasData(f.replacement) {
			if err := f.switchToReplacement(); err != nil {
				return "", filePosition{}, err
			}
			continue
		}
		return "", filePosition{}, errNoLine
	}
}

// readLine reads one complete line from the file being followed.
func (f *follower) readLine() (string, filePosition, error) {
	for {
		chunk, err := f.reader.ReadSlice('\n')
		f.pending = append(f.pending, chunk...)
		f.offset += int64(len(chunk))
		switch {
		case err == nil:
			text := string(f.pending)
			f.pending = f.pending[:0]
			return strings.TrimRight(text, "\r\n"), filePosition{Inode: f.inode, Offset: f.offset}, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return "", filePosition{}, errNoLine
		default:
			return "", filePosition{}, fmt.Errorf("read access log: %w", err)
		}
	}
}

// checkRotation detects a rotated or truncated file and opens the file that
// replaced a rotated one.
func (f *follower) checkRotation() error {
	info, err := os.Stat(f.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // the file comes back after a rotation
		}
		return fmt.Errorf("stat access log: %w", err)
	}
	inode := inodeOf(info)
	switch {
	case inode == f.inode:
		if info.Size() < f.offset {
			// The file was truncated in place.
			if _, err := f.file.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("seek access log: %w", err)
			}
			f.reader.Reset(f.file)
			f.pending = f.pending[:0]
			f.offset = 0
		}
	case f.replacement != nil:
		// Another rotation while the previous replacement waits; the file
		// being read is still finished first.
	default:
		replacement, err := os.Open(f.path)
		if err != nil {
			return fmt.Errorf("open access log: %w", err)
		}
		f.replacement = replacement
	}
	return nil
}

// switchToReplacement reads from the file that replaced the rotated one.
func (f *follower) switchToReplacement() error {
	info, err := f.replacement.Stat()
	if err != nil {
		return fmt.Errorf("stat access log: %w", err)
	}
	old := f.file
	f.file = f.replacement
	f.replacement = nil
	f.reader.Reset(f.file)
	f.pending = f.pending[:0]
	f.inode = inodeOf(info)
	f.offset = 0
	if err := old.Close(); err != nil {
		return fmt.Errorf("close access log: %w", err)
	}
	return nil
}

// grew reports whether the file being read has bytes beyond the current
// offset.
func (f *follower) grew() bool {
	info, err := f.file.Stat()
	return err == nil && info.Size() > f.offset
}

// close closes the open files.
func (f *follower) close() error {
	if f.replacement != nil {
		f.replacement.Close()
	}
	return f.file.Close()
}

// hasData reports whether a file has content.
func hasData(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Size() > 0
}

// inodeOf returns the inode of a file.
func inodeOf(info fs.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Ino
	}
	return 0
}
