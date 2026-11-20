package core

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

// Device is anything we can read sectors from or write sectors to: a whole
// disk, a partition (just a window onto a disk), or an image file.
type Device interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Size() int64
	SectorSize() int
	Growable() bool // image files grow as we write, disks don't
	Sync() error    // for image targets this also finalizes the file
	Close() error
}

// Optional extras a Device can implement.
type (
	// verifyOpener gives the verify pass a fresh reader that doesn't come out
	// of a cache (or that decompresses the image we just wrote).
	verifyOpener interface{ OpenForVerify() (Device, error) }

	// tableLast is implemented by real disks. On those we hold back the first
	// block (MBR/GPT) and write it at the very end, so a half-finished copy
	// never looks like a valid disk.
	tableLast interface{ PartitionTableLast() bool }
)

// fatalError marks errors that retrying won't fix (corrupt archive, device
// gone...). The bad-sector logic gives up immediately on these.
type fatalError struct{ err error }

func (e fatalError) Error() string { return e.err.Error() }
func (e fatalError) Unwrap() error { return e.err }

func isFatal(err error) bool {
	var f fatalError
	return errors.As(err, &f) || errors.Is(err, io.ErrUnexpectedEOF) || isFatalIOError(err)
}

// fileDevice is a plain file: raw .img / .iso images, or a block device on Linux.
type fileDevice struct {
	f        *os.File
	path     string
	size     int64
	sector   int
	growable bool
	table    bool
}

func (d *fileDevice) ReadAt(p []byte, off int64) (int, error) { return d.f.ReadAt(p, off) }
func (d *fileDevice) WriteAt(p []byte, off int64) (int, error) {
	n, err := d.f.WriteAt(p, off)
	if end := off + int64(n); end > d.size {
		d.size = end
	}
	return n, err
}
func (d *fileDevice) Size() int64              { return d.size }
func (d *fileDevice) SectorSize() int          { return d.sector }
func (d *fileDevice) Growable() bool           { return d.growable }
func (d *fileDevice) Sync() error              { return d.f.Sync() }
func (d *fileDevice) Close() error             { return d.f.Close() }
func (d *fileDevice) PartitionTableLast() bool { return d.table }

func (d *fileDevice) OpenForVerify() (Device, error) {
	if !d.growable && !d.table {
		return nil, errors.New("nothing to reopen")
	}
	return openUnbuffered(d.path, d.size)
}

// window exposes [base, base+size) of another device as its own device.
// Used for fixed VHDs (data first, footer after).
type window struct {
	Device
	base, size int64
}

func (w *window) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > w.size {
		return 0, io.EOF
	}
	return w.Device.ReadAt(p, w.base+off)
}
func (w *window) WriteAt([]byte, int64) (int, error) { return 0, errors.New("read-only") }
func (w *window) Size() int64                        { return w.size }
func (w *window) Growable() bool                     { return false }

// unbufferedFile reads through a handle that skips the OS cache
// (O_DIRECT / FILE_FLAG_NO_BUFFERING). Those want sector-aligned lengths, so
// we round reads up into the spare room of the (aligned) buffer and accept a
// short read at the end of the file. readOnce must do exactly one OS read.
type unbufferedFile struct {
	readOnce func(p []byte, off int64) (int, error)
	closer   func() error
	size     int64
}

func (u *unbufferedFile) ReadAt(p []byte, off int64) (int, error) {
	want := len(p)
	n := roundUp(want, 4096)
	if n > cap(p) {
		return 0, errors.New("buffer too small for an unbuffered read")
	}
	got, err := u.readOnce(p[:n], off)
	if got >= want {
		return want, nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return got, err
}
func (u *unbufferedFile) WriteAt([]byte, int64) (int, error) { return 0, errors.New("read-only") }
func (u *unbufferedFile) Size() int64                        { return u.size }
func (u *unbufferedFile) SectorSize() int                    { return 512 }
func (u *unbufferedFile) Growable() bool                     { return false }
func (u *unbufferedFile) Sync() error                        { return nil }
func (u *unbufferedFile) Close() error                       { return u.closer() }

// ---- small helpers -------------------------------------------------------

// alignedBuffer returns a zeroed buffer starting on an `align` boundary,
// which unbuffered disk I/O on Windows insists on.
func alignedBuffer(size, align int) []byte {
	raw := make([]byte, size+align)
	off := 0
	if r := int(uintptr(unsafe.Pointer(&raw[0])) % uintptr(align)); r != 0 {
		off = align - r
	}
	return raw[off : off+size : off+size]
}

func roundUp(n, m int) int       { return (n + m - 1) / m * m }
func roundUp64(n, m int64) int64 { return (n + m - 1) / m * m }

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// HumanBytes formats a byte count like "476.9 GB". Like Explorer and Disk
// Management, "GB" here means 1024^3 bytes.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// CommaInt formats 1234567 as "1,234,567".
func CommaInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// HumanDuration formats durations like 1h02m03s / 4m05s / 7s.
func HumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
