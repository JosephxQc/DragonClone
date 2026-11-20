package core

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ErrCancelled comes back when the user hits Cancel.
var ErrCancelled = errors.New("cancelled")

// BadRange is a stretch of the source we couldn't read (written as zeros).
type BadRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// IOError is a read or write failure at a known spot.
type IOError struct {
	Op     string
	Offset int64
	Length int
	Err    error
}

func (e *IOError) Error() string {
	return fmt.Sprintf("%s error at byte %s (%s in): %v", e.Op, CommaInt(e.Offset), HumanBytes(e.Offset), e.Err)
}
func (e *IOError) Unwrap() error { return e.Err }

func readFull(d Device, p []byte, off int64) error {
	for len(p) > 0 {
		n, err := d.ReadAt(p, off)
		p, off = p[n:], off+int64(n)
		if len(p) == 0 {
			return nil
		}
		if err != nil {
			if err == io.EOF {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

func writeFull(d Device, p []byte, off int64) error {
	for len(p) > 0 {
		n, err := d.WriteAt(p, off)
		p, off = p[n:], off+int64(n)
		if len(p) == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// CopyResult describes a finished copy.
type CopyResult struct {
	Size       int64      `json:"size"`    // source bytes
	Written    int64      `json:"written"` // incl. padding up to the target's sector size
	SHA256     string     `json:"sha256"`
	Bad        []BadRange `json:"bad"`
	BadBytes   int64      `json:"badBytes"`
	Verified   bool       `json:"verified"`
	Mismatches []int64    `json:"mismatches"`
	CopyTime   float64    `json:"copyTime"`
	VerifyTime float64    `json:"verifyTime"`
}

// Copy clones src onto dst, sector for sector.
//
// Reads and writes overlap (two goroutines, a few buffers in between). On real
// disks the first block - where the MBR/GPT lives - is written last, so if the
// copy dies halfway the target still has its old partition table instead of
// one pointing at half-copied partitions.
func Copy(parent context.Context, src, dst Device, opts *Options, tr *Tracker) (*CopyResult, error) {
	size := src.Size()
	if size <= 0 {
		return nil, errors.New("the source is empty")
	}
	rs, ws := src.SectorSize(), dst.SectorSize()
	bs := int64(opts.BlockSize)
	if bs < 64<<10 || bs%4096 != 0 || bs%int64(rs) != 0 || bs%int64(ws) != 0 {
		return nil, fmt.Errorf("block size %d doesn't fit the sector sizes", bs)
	}
	pad := !dst.Growable()
	written := size
	if pad {
		written = roundUp64(size, int64(ws))
		if dst.Size() < written {
			return nil, fmt.Errorf("the target holds %s but %s is needed", HumanBytes(dst.Size()), HumanBytes(written))
		}
	}
	holdHead := false
	if tl, ok := dst.(tableLast); ok {
		holdHead = tl.PartitionTableLast()
	}

	nBlocks := int((size + bs - 1) / bs)
	blockLen := func(i int) (srcLen, outLen int) {
		srcLen = int(min64(bs, size-int64(i)*bs))
		outLen = srcLen
		if pad && i == nBlocks-1 {
			outLen = roundUp(srcLen, ws)
		}
		return
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	const nbuf = 4
	free := make(chan []byte, nbuf)
	for i := 0; i < nbuf; i++ {
		free <- alignedBuffer(int(bs), 4096)
	}
	type chunk struct {
		idx int
		off int64
		n   int
		buf []byte
	}
	work := make(chan chunk, nbuf)

	hashes := make([][32]byte, nBlocks)
	whole := sha256.New()
	var bad []BadRange
	var readErr, writeErr error
	tr.Begin("Copying", written)
	start := time.Now()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // reader
		defer wg.Done()
		defer close(work)
		for i := 0; i < nBlocks; i++ {
			var buf []byte
			select {
			case <-ctx.Done():
				return
			case buf = <-free:
			}
			off := int64(i) * bs
			srcLen, outLen := blockLen(i)
			nb, err := readBlock(ctx, src, buf[:srcLen], off, opts.SkipBad, opts.Retries, tr)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					readErr = err
				}
				cancel()
				return
			}
			for _, r := range nb {
				bad = appendBad(bad, r.Offset, r.Length)
			}
			clear(buf[srcLen:outLen])
			whole.Write(buf[:srcLen])
			hashes[i] = sha256.Sum256(buf[:outLen])
			select {
			case work <- chunk{i, off, outLen, buf}:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { // writer
		defer wg.Done()
		var head []byte
		for c := range work {
			if holdHead && c.idx == 0 && nBlocks > 1 {
				head = alignedBuffer(c.n, 4096)
				copy(head, c.buf[:c.n])
				free <- c.buf
				tr.Add(int64(c.n))
				continue
			}
			if err := writeFull(dst, c.buf[:c.n], c.off); err != nil {
				writeErr = &IOError{"write", c.off, c.n, err}
				cancel()
				return
			}
			free <- c.buf
			tr.Add(int64(c.n))
		}
		if ctx.Err() != nil || head == nil {
			return
		}
		if err := writeFull(dst, head, 0); err != nil {
			writeErr = &IOError{"write", 0, len(head), err}
		}
	}()
	wg.Wait()

	switch {
	case writeErr != nil:
		return nil, writeErr
	case readErr != nil:
		return nil, readErr
	case parent.Err() != nil:
		return nil, ErrCancelled
	}
	tr.Begin("Finishing", 0)
	if err := dst.Sync(); err != nil {
		return nil, fmt.Errorf("couldn't finish writing the target: %w", err)
	}
	res := &CopyResult{Size: size, Written: written, SHA256: hex.EncodeToString(whole.Sum(nil)),
		Bad: bad, CopyTime: time.Since(start).Seconds()}
	for _, b := range bad {
		res.BadBytes += b.Length
	}
	if !opts.Verify {
		return res, nil
	}

	// Verify: read the target back and compare block hashes.
	vstart := time.Now()
	vdev := dst
	if vo, ok := dst.(verifyOpener); ok {
		if d, err := vo.OpenForVerify(); err == nil {
			vdev = d
			defer d.Close()
		}
	}
	buf := alignedBuffer(int(bs), 4096)
	tr.Begin("Verifying", written)
	for i := 0; i < nBlocks; i++ {
		if parent.Err() != nil {
			return res, ErrCancelled
		}
		off := int64(i) * bs
		_, outLen := blockLen(i)
		if err := readFull(vdev, buf[:outLen], off); err != nil {
			return res, fmt.Errorf("verify: %w", &IOError{"read-back", off, outLen, err})
		}
		if sha256.Sum256(buf[:outLen]) != hashes[i] {
			res.Mismatches = append(res.Mismatches, off)
			tr.log("Verify mismatch in the block at %s", CommaInt(off))
		}
		tr.Add(int64(outLen))
	}
	res.Verified = true
	res.VerifyTime = time.Since(vstart).Seconds()
	return res, nil
}

// readBlock reads one block. If that fails it either gives up (default) or,
// with skipBad, saves every sector it can and zero-fills the rest.
func readBlock(ctx context.Context, src Device, buf []byte, off int64, skipBad bool, retries int, tr *Tracker) ([]BadRange, error) {
	err := readFull(src, buf, off)
	if err == nil {
		return nil, nil
	}
	if isFatal(err) {
		return nil, &IOError{"read", off, len(buf), err}
	}
	if !skipBad {
		for a := 0; a < retries; a++ {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err = readFull(src, buf, off); err == nil {
				return nil, nil
			}
		}
		return nil, &IOError{"read", off, len(buf), err}
	}
	tr.log("Read error around byte %s - looking for the bad sectors", CommaInt(off))
	var bad []BadRange
	if err := rescue(ctx, src, buf, off, retries, &bad, tr); err != nil {
		return nil, err
	}
	return bad, nil
}

// rescue deals with a range that already failed to read. It splits it in
// half again and again until it's down to single sectors, so a couple of
// bad sectors in a 4 MiB block cost a few dozen reads, not thousands.
func rescue(ctx context.Context, src Device, buf []byte, off int64, retries int, bad *[]BadRange, tr *Tracker) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ss := src.SectorSize()
	if len(buf) <= ss {
		for a := 0; a < retries; a++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			err := readFull(src, buf, off)
			if err == nil {
				return nil
			}
			if isFatal(err) {
				return &IOError{"read", off, len(buf), err}
			}
		}
		clear(buf)
		*bad = appendBad(*bad, off, int64(len(buf)))
		tr.BadAt(off, int64(len(buf)))
		tr.log("Unreadable sector at byte %s - filled with zeros", CommaInt(off))
		return nil
	}
	half := (len(buf) / ss / 2) * ss
	if half == 0 {
		half = ss
	}
	parts := [2]struct {
		b []byte
		o int64
	}{{buf[:half], off}, {buf[half:], off + int64(half)}}
	for _, p := range parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := readFull(src, p.b, p.o)
		if err == nil {
			continue
		}
		if isFatal(err) {
			return &IOError{"read", p.o, len(p.b), err}
		}
		if err := rescue(ctx, src, p.b, p.o, retries, bad, tr); err != nil {
			return err
		}
	}
	return nil
}

func appendBad(list []BadRange, off, n int64) []BadRange {
	if k := len(list); k > 0 && list[k-1].Offset+list[k-1].Length == off {
		list[k-1].Length += n
		return list
	}
	return append(list, BadRange{off, n})
}

// CompareResult is the outcome of a byte-by-byte comparison.
type CompareResult struct {
	Length     int64   `json:"length"`
	SizeA      int64   `json:"sizeA"`
	SizeB      int64   `json:"sizeB"`
	DiffBytes  int64   `json:"diffBytes"`
	FirstDiffs []int64 `json:"firstDiffs"`
}

// Compare checks the first `length` bytes of a and b.
func Compare(ctx context.Context, a, b Device, bs int, tr *Tracker) (*CompareResult, error) {
	length := min64(a.Size(), b.Size())
	res := &CompareResult{Length: length, SizeA: a.Size(), SizeB: b.Size()}
	ba, bb := alignedBuffer(bs, 4096), alignedBuffer(bs, 4096)
	tr.Begin("Comparing", length)
	for off := int64(0); off < length; off += int64(bs) {
		if ctx.Err() != nil {
			return res, ErrCancelled
		}
		n := int(min64(int64(bs), length-off))
		errB := make(chan error, 1)
		go func() { errB <- readFull(b, bb[:n], off) }() // both sides read at once
		errA := readFull(a, ba[:n], off)
		eb := <-errB
		if errA != nil {
			return res, &IOError{"read (first)", off, n, errA}
		}
		if eb != nil {
			return res, &IOError{"read (second)", off, n, eb}
		}
		if !bytes.Equal(ba[:n], bb[:n]) {
			for i := 0; i < n; i++ {
				if ba[i] != bb[i] {
					res.DiffBytes++
					if len(res.FirstDiffs) < 10 {
						res.FirstDiffs = append(res.FirstDiffs, off+int64(i))
					}
				}
			}
		}
		tr.Add(int64(n))
	}
	return res, nil
}

// Hash returns the SHA-256 of everything on a device (or inside an image).
func Hash(ctx context.Context, d Device, bs int, tr *Tracker) (string, error) {
	h := sha256.New()
	buf := alignedBuffer(bs, 4096)
	size := d.Size()
	tr.Begin("Checking", size)
	for off := int64(0); off < size; off += int64(bs) {
		if ctx.Err() != nil {
			return "", ErrCancelled
		}
		n := int(min64(int64(bs), size-off))
		if err := readFull(d, buf[:n], off); err != nil {
			return "", &IOError{"read", off, n, err}
		}
		h.Write(buf[:n])
		tr.Add(int64(n))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ScanResult is what a surface scan found.
type ScanResult struct {
	Size     int64      `json:"size"`
	Bad      []BadRange `json:"bad"`
	BadBytes int64      `json:"badBytes"`
	Seconds  float64    `json:"seconds"`
	AvgRate  float64    `json:"avgRate"` // bytes/s
	MinRate  float64    `json:"minRate"` // slowest 64 MiB stretch
}

// Scan reads the whole device and maps out every sector it can't read.
func Scan(ctx context.Context, d Device, bs, retries int, tr *Tracker) (*ScanResult, error) {
	size := d.Size()
	res := &ScanResult{Size: size}
	buf := alignedBuffer(bs, 4096)
	tr.Begin("Scanning", size)
	start := time.Now()
	winStart, winBytes := time.Now(), int64(0)
	for off := int64(0); off < size; off += int64(bs) {
		if ctx.Err() != nil {
			return res, ErrCancelled
		}
		n := int(min64(int64(bs), size-off))
		nb, err := readBlock(ctx, d, buf[:n], off, true, retries, tr)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return res, ErrCancelled
			}
			return res, err
		}
		for _, r := range nb {
			res.Bad = appendBad(res.Bad, r.Offset, r.Length)
			res.BadBytes += r.Length
		}
		tr.Add(int64(n))
		if winBytes += int64(n); winBytes >= 64<<20 {
			rate := float64(winBytes) / time.Since(winStart).Seconds()
			if res.MinRate == 0 || rate < res.MinRate {
				res.MinRate = rate
			}
			winStart, winBytes = time.Now(), 0
		}
	}
	res.Seconds = time.Since(start).Seconds()
	if res.Seconds > 0 {
		res.AvgRate = float64(size) / res.Seconds
	}
	return res, nil
}

// WipeMode picks what Wipe writes.
type WipeMode string

const (
	WipeZeros  WipeMode = "zeros"
	WipeRandom WipeMode = "random"
)

// WipeResult describes a finished wipe.
type WipeResult struct {
	Size     int64   `json:"size"`
	Verified bool    `json:"verified"`
	Bad      int64   `json:"badBlocks"` // blocks that read back wrong
	Seconds  float64 `json:"seconds"`
}

// Wipe overwrites a whole device with zeros or random data. Random data
// comes from AES-CTR with a throwaway key, which is fast and, since we keep
// the key until we're done, lets the verify pass regenerate the same stream.
func Wipe(ctx context.Context, d Device, mode WipeMode, verify bool, bs int, tr *Tracker) (*WipeResult, error) {
	size := d.Size()
	res := &WipeResult{Size: size}
	var key, iv [16]byte
	rand.Read(key[:])
	rand.Read(iv[:])
	stream := func() cipher.Stream {
		blk, _ := aes.NewCipher(key[:])
		return cipher.NewCTR(blk, iv[:])
	}
	fill := func(s cipher.Stream, b []byte) {
		clear(b)
		if mode == WipeRandom {
			s.XORKeyStream(b, b)
		}
	}

	start := time.Now()
	buf := alignedBuffer(bs, 4096)
	s := stream()
	tr.Begin("Wiping", size)
	for off := int64(0); off < size; off += int64(bs) {
		if ctx.Err() != nil {
			return res, ErrCancelled
		}
		n := int(min64(int64(bs), size-off))
		fill(s, buf[:n])
		if err := writeFull(d, buf[:n], off); err != nil {
			return res, &IOError{"write", off, n, err}
		}
		tr.Add(int64(n))
	}
	if err := d.Sync(); err != nil {
		return res, err
	}
	if verify {
		want := alignedBuffer(bs, 4096)
		s = stream()
		tr.Begin("Verifying", size)
		for off := int64(0); off < size; off += int64(bs) {
			if ctx.Err() != nil {
				return res, ErrCancelled
			}
			n := int(min64(int64(bs), size-off))
			fill(s, want[:n])
			if err := readFull(d, buf[:n], off); err != nil {
				return res, &IOError{"read-back", off, n, err}
			}
			if !bytes.Equal(buf[:n], want[:n]) {
				res.Bad++
			}
			tr.Add(int64(n))
		}
		res.Verified = true
	}
	res.Seconds = time.Since(start).Seconds()
	return res, nil
}
