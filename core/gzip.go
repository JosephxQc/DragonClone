package core

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Compressed images are plain multi-member gzip files. Every 1 MiB block is
// compressed on its own, which lets all CPU cores work at once, and the
// result is still a normal .gz that gunzip / 7-Zip can unpack.

const gzBlock = 1 << 20

type gzResult struct {
	buf *bytes.Buffer
	err error
}

type gzJob struct {
	data []byte
	out  chan gzResult
}

type gzipTarget struct {
	f     *os.File
	path  string
	level int
	pos   int64 // uncompressed bytes accepted so far

	work  chan gzJob
	queue chan chan gzResult // results in file order
	wg    sync.WaitGroup
	wdone chan struct{}

	mu     sync.Mutex
	err    error
	packed int64 // compressed bytes written

	blocks sync.Pool
	bufs   sync.Pool
	closed bool
	done   bool
}

func newGzipTarget(f *os.File, path string, level int, meta *ImageMeta) (*gzipTarget, error) {
	if level < gzip.BestSpeed || level > gzip.BestCompression {
		level = gzip.BestSpeed
	}
	// First member: empty, but its header carries the metadata.
	var head bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&head, level)
	zw.Header.Name = strings.TrimSuffix(filepath.Base(path), ".gz")
	zw.Header.Comment = "DragonClone disk image"
	zw.Header.ModTime = time.Now()
	if meta != nil {
		zw.Header.Extra = encodeGzipExtra(meta)
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if _, err := f.Write(head.Bytes()); err != nil {
		f.Close()
		return nil, err
	}

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	t := &gzipTarget{
		f: f, path: path, level: level,
		work:   make(chan gzJob, workers),
		queue:  make(chan chan gzResult, workers*2),
		wdone:  make(chan struct{}),
		packed: int64(head.Len()),
	}
	t.blocks.New = func() any { return make([]byte, gzBlock) }
	t.bufs.New = func() any { return new(bytes.Buffer) }

	for i := 0; i < workers; i++ {
		t.wg.Add(1)
		go t.compressLoop()
	}
	go t.writeLoop()
	return t, nil
}

func (t *gzipTarget) compressLoop() {
	defer t.wg.Done()
	var zw *gzip.Writer
	for j := range t.work {
		buf := t.bufs.Get().(*bytes.Buffer)
		buf.Reset()
		if zw == nil {
			zw, _ = gzip.NewWriterLevel(buf, t.level)
		} else {
			zw.Reset(buf)
		}
		_, err := zw.Write(j.data)
		if err == nil {
			err = zw.Close()
		}
		t.blocks.Put(j.data[:cap(j.data)])
		j.out <- gzResult{buf, err}
	}
}

func (t *gzipTarget) writeLoop() {
	defer close(t.wdone)
	for ch := range t.queue {
		r := <-ch
		if r.err == nil && t.firstErr() == nil {
			n, err := t.f.Write(r.buf.Bytes())
			t.mu.Lock()
			t.packed += int64(n)
			t.mu.Unlock()
			r.err = err
		}
		if r.err != nil {
			t.setErr(r.err)
		}
		t.bufs.Put(r.buf)
	}
}

func (t *gzipTarget) setErr(err error) {
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.mu.Unlock()
}

func (t *gzipTarget) firstErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

func (t *gzipTarget) WriteAt(p []byte, off int64) (int, error) {
	if t.closed {
		return 0, errors.New("image already closed")
	}
	if off != t.pos {
		return 0, fatalError{fmt.Errorf("compressed images have to be written front to back (got offset %d, expected %d)", off, t.pos)}
	}
	if err := t.firstErr(); err != nil {
		return 0, err
	}
	for i := 0; i < len(p); i += gzBlock {
		n := min(gzBlock, len(p)-i)
		blk := t.blocks.Get().([]byte)[:n]
		copy(blk, p[i:i+n])
		out := make(chan gzResult, 1)
		t.queue <- out
		t.work <- gzJob{blk, out}
	}
	t.pos += int64(len(p))
	return len(p), nil
}

func (t *gzipTarget) ReadAt([]byte, int64) (int, error) {
	return 0, errors.New("can't read back while writing a compressed image")
}
func (t *gzipTarget) Size() int64     { return t.pos }
func (t *gzipTarget) SectorSize() int { return 512 }
func (t *gzipTarget) Growable() bool  { return true }

// CompressedSize is how big the .gz file is so far.
func (t *gzipTarget) CompressedSize() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.packed
}

func (t *gzipTarget) shutdown() {
	if t.closed {
		return
	}
	t.closed = true
	close(t.queue)
	<-t.wdone
	close(t.work)
	t.wg.Wait()
}

// Sync waits for every block to land in the file. After this the image is
// complete; no more writes.
func (t *gzipTarget) Sync() error {
	if t.done {
		return t.firstErr()
	}
	t.shutdown()
	t.done = true
	if err := t.firstErr(); err != nil {
		return err
	}
	return t.f.Sync()
}

func (t *gzipTarget) Close() error {
	t.shutdown()
	return t.f.Close()
}

func (t *gzipTarget) OpenForVerify() (Device, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, err
	}
	return newGzipSource(f, t.pos)
}

// gzipSource reads the disk out of a compressed image. It only goes forward,
// which is all a copy, compare or hash ever needs.
type gzipSource struct {
	f    *os.File
	zr   *gzip.Reader
	pos  int64
	size int64
}

func newGzipSource(f *os.File, size int64) (*gzipSource, error) {
	zr, err := gzip.NewReader(bufio.NewReaderSize(f, 4<<20))
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("not a valid compressed image: %w", err)
	}
	return &gzipSource{f: f, zr: zr, size: size}, nil
}

func (g *gzipSource) ReadAt(p []byte, off int64) (int, error) {
	if off < g.pos {
		return 0, fatalError{errors.New("compressed images can only be read front to back")}
	}
	if off > g.pos {
		if _, err := io.CopyN(io.Discard, g.zr, off-g.pos); err != nil {
			return 0, fatalError{fmt.Errorf("compressed image ended early or is damaged: %w", err)}
		}
		g.pos = off
	}
	if off+int64(len(p)) > g.size {
		return 0, io.EOF
	}
	n, err := io.ReadFull(g.zr, p)
	g.pos += int64(n)
	if err != nil {
		return n, fatalError{fmt.Errorf("compressed image is damaged or cut short: %w", err)}
	}
	if g.pos == g.size {
		// Read one more byte so gzip checks the CRC of the final block.
		var one [1]byte
		switch _, err := g.zr.Read(one[:]); {
		case err == io.EOF:
		case err == nil:
			return n, fatalError{errors.New("compressed image holds more data than its header says")}
		default:
			return n, fatalError{fmt.Errorf("compressed image is damaged: %w", err)}
		}
	}
	return n, nil
}

func (g *gzipSource) WriteAt([]byte, int64) (int, error) { return 0, errors.New("read-only") }
func (g *gzipSource) Size() int64                        { return g.size }
func (g *gzipSource) SectorSize() int                    { return 512 }
func (g *gzipSource) Growable() bool                     { return false }
func (g *gzipSource) Sync() error                        { return nil }
func (g *gzipSource) Close() error                       { return g.f.Close() }
