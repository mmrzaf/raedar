package accesslog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// followerLines reads the lines a follower can produce right now.
func followerLines(t *testing.T, reader *follower) []string {
	t.Helper()
	var lines []string
	for {
		text, _, err := reader.next()
		if errors.Is(err, errNoLine) {
			return lines
		}
		if err != nil {
			t.Fatalf("next() error = %v", err)
		}
		lines = append(lines, text)
	}
}

func TestFollowerResumesAtTheSavedOffset(t *testing.T) {
	path := writeFile(t, "a", "b", "c")
	reader, err := openFollower(path, &filePosition{Inode: 0, Offset: 0})
	if err != nil {
		t.Fatalf("openFollower() error = %v", err)
	}
	defer reader.close()
	// A position of inode zero does not match, so reading starts at the end.
	if got := followerLines(t, reader); len(got) != 0 {
		t.Fatalf("lines = %v, want none when starting at the end", got)
	}
	appendFile(t, path, "d")
	if got := followerLines(t, reader); len(got) != 1 || got[0] != "d" {
		t.Errorf("lines = %v, want the appended line", got)
	}
}

func TestFollowerReadsFromASavedPosition(t *testing.T) {
	path := writeFile(t, "a", "b", "c")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	reader, err := openFollower(path, &filePosition{Inode: inodeOf(info), Offset: 0})
	if err != nil {
		t.Fatalf("openFollower() error = %v", err)
	}
	defer reader.close()
	lines := followerLines(t, reader)
	if len(lines) != 3 || lines[0] != "a" || lines[2] != "c" {
		t.Fatalf("lines = %v, want a, b and c", lines)
	}
}

func TestFollowerKeepsAPartialLine(t *testing.T) {
	path := writeFile(t, "complete")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := file.WriteString("partial"); err != nil {
		t.Fatalf("write: %v", err)
	}
	file.Close()
	reader, err := openFollower(path, &filePosition{Inode: inodeOf(info), Offset: 0})
	if err != nil {
		t.Fatalf("openFollower() error = %v", err)
	}
	defer reader.close()
	appendFile(t, path, " rest")
	lines := followerLines(t, reader)
	if len(lines) != 2 || lines[0] != "complete" || lines[1] != "partial rest" {
		t.Errorf("lines = %v, want the completed line", lines)
	}
}

func TestFollowerFinishesTheRotatedFile(t *testing.T) {
	path := writeFile(t, "old-1", "old-2", "old-3")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	rotated := path + ".1"
	reader, err := openFollower(path, &filePosition{Inode: inodeOf(info), Offset: 0})
	if err != nil {
		t.Fatalf("openFollower() error = %v", err)
	}
	defer reader.close()
	// Read one line, then rotate the file the way logrotate does.
	text, _, err := reader.next()
	if err != nil || text != "old-1" {
		t.Fatalf("next() = %q, %v, want old-1", text, err)
	}
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rename: %v", err)
	}
	appendFile(t, path, "new-1", "new-2")
	lines := followerLines(t, reader)
	want := []string{"old-2", "old-3", "new-1", "new-2"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i, line := range want {
		if lines[i] != line {
			t.Errorf("line %d = %q, want %q", i, lines[i], line)
		}
	}
}

func TestFollowerDetectsTruncation(t *testing.T) {
	path := writeFile(t, "old-1", "old-2")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	reader, err := openFollower(path, &filePosition{Inode: inodeOf(info), Offset: 0})
	if err != nil {
		t.Fatalf("openFollower() error = %v", err)
	}
	defer reader.close()
	if lines := followerLines(t, reader); len(lines) != 2 {
		t.Fatalf("lines = %v, want 2", lines)
	}
	// Copy-truncate: the file shrinks while keeping its inode.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	appendFile(t, path, "new-1")
	lines := followerLines(t, reader)
	if len(lines) != 1 || lines[0] != "new-1" {
		t.Errorf("lines = %v, want the line written after the truncation", lines)
	}
}

func TestFollowerStartsAtTheEndWhenTheSavedInodeIsGone(t *testing.T) {
	path := writeFile(t, "a", "b")
	reader, err := openFollower(path, &filePosition{Inode: 12345, Offset: 0})
	if err != nil {
		t.Fatalf("openFollower() error = %v", err)
	}
	defer reader.close()
	if lines := followerLines(t, reader); len(lines) != 0 {
		t.Errorf("lines = %v, want none when the saved inode no longer exists", lines)
	}
}

func TestFollowerReportsAMissingFile(t *testing.T) {
	_, err := openFollower(filepath.Join(t.TempDir(), "missing.log"), nil)
	if err == nil {
		t.Fatal("openFollower() error = nil, want an error")
	}
}
