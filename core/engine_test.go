package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// memDisk is a fake disk in memory. It can fail reads on chosen sectors and
// remembers the order of writes.
type memDisk struct {
	mu       sync.Mutex
	data     []byte
	sector   int
	growable bool
	table    bool
	bad      map[int64]int // sector -> failures left (-1 = forever)
	fatalAt  int64
	failW    int64
	writes   []int64
	reads    int
}

func newMem(size, sector int) *memDisk {
	return &memDisk{data: make([]byte, size), sector: sector, bad: map[int64]int{}, fatalAt: -1, failW: -1, table: true}
}

func randomMem(size, sector int, seed int64) *memDisk {
	m := newMem(size, sector)
	rand.New(rand.NewSource(seed)).Read(m.data)
	return m
}

func (m *memDisk) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	if off+int64(len(p)) > int64(len(m.data)) {
		return 0, io.EOF
	}
	if m.fatalAt >= 0 && off <= m.fatalAt && m.fatalAt < off+int64(len(p)) {
		return 0, fatalError{syscall.ENODEV} // "device gone" on any OS
	}
	for s := off / int64(m.sector); s*int64(m.sector) < off+int64(len(p)); s++ {
		if left, ok := m.bad[s]; ok && left != 0 {
			if left > 0 {
				m.bad[s] = left - 1
			}
			return 0, syscall.EIO
		}
	}
	return copy(p, m.data[off:]), nil
}

func (m *memDisk) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if off == m.failW {
		return 0, syscall.EIO
	}
	m.writes = append(m.writes, off)
	if end := off + int64(len(p)); end > int64(len(m.data)) {
		if !m.growable {
			return 0, errors.New("write past the end")
		}
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}
	return copy(m.data[off:], p), nil
}
func (m *memDisk) Size() int64              { return int64(len(m.data)) }
func (m *memDisk) SectorSize() int          { return m.sector }
func (m *memDisk) Growable() bool           { return m.growable }
func (m *memDisk) PartitionTableLast() bool { return m.table }
func (m *memDisk) Sync() error              { return nil }
func (m *memDisk) Close() error             { return nil }

func testOpts(bs int) *Options {
	o := DefaultOptions()
	o.BlockSize, o.Verify = bs, true
	return o
}

func TestCopyIsExact(t *testing.T) {
	for _, size := range []int{64 << 10, 1 << 20, 3<<20 + 512, 5 << 20} {
		src := randomMem(size, 512, int64(size))
		dst := newMem(size+1<<20, 512)
		for i := range dst.data {
			dst.data[i] = 0xAB
		}
		res, err := Copy(context.Background(), src, dst, testOpts(64<<10), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(dst.data[:size], src.data) {
			t.Fatalf("size %d: copy differs", size)
		}
		for _, b := range dst.data[size:] {
			if b != 0xAB {
				t.Fatalf("size %d: wrote past the end of the source", size)
			}
		}
		sum := sha256.Sum256(src.data)
		if res.SHA256 != hex.EncodeToString(sum[:]) || !res.Verified || len(res.Mismatches) != 0 {
			t.Fatalf("bad result %+v", res)
		}
	}
}

func TestPartitionTableGoesLastOnDisks(t *testing.T) {
	src := randomMem(1<<20, 512, 1)
	dst := newMem(1<<20, 512)
	if _, err := Copy(context.Background(), src, dst, testOpts(64<<10), nil); err != nil {
		t.Fatal(err)
	}
	if len(dst.writes) != 16 || dst.writes[15] != 0 {
		t.Fatalf("first block should be written last, order was %v", dst.writes)
	}
	// ...but files get written in order (compressed images need that).
	img := newMem(0, 512)
	img.growable, img.table = true, false
	if _, err := Copy(context.Background(), src, img, testOpts(64<<10), nil); err != nil {
		t.Fatal(err)
	}
	if img.writes[0] != 0 {
		t.Fatalf("image writes should be in order, got %v", img.writes)
	}
}

func TestReadErrorStopsByDefault(t *testing.T) {
	src := randomMem(1<<20, 512, 3)
	src.bad[700] = -1
	dst := newMem(1<<20, 512)
	_, err := Copy(context.Background(), src, dst, testOpts(64<<10), nil)
	var ioe *IOError
	if !errors.As(err, &ioe) || ioe.Op != "read" {
		t.Fatalf("expected a read error, got %v", err)
	}
	if bytes.Equal(dst.data[:512], src.data[:512]) {
		t.Fatal("the partition table must not be written when the copy fails")
	}
}

func TestFlakySectorIsRetried(t *testing.T) {
	src := randomMem(1<<20, 512, 4)
	src.bad[100] = 1
	dst := newMem(1<<20, 512)
	res, err := Copy(context.Background(), src, dst, testOpts(64<<10), nil)
	if err != nil || len(res.Bad) != 0 || !bytes.Equal(dst.data, src.data) {
		t.Fatalf("flaky sector not recovered: %v %+v", err, res)
	}
}

func TestSkipBadOnlyZerosTheBadSectors(t *testing.T) {
	src := randomMem(2<<20, 512, 5)
	badSectors := []int64{0, 1, 2, 300, 2047, 4095}
	for _, s := range badSectors {
		src.bad[s] = -1
	}
	dst := newMem(2<<20, 512)
	for i := range dst.data {
		dst.data[i] = 0xEE
	}
	o := testOpts(256 << 10)
	o.SkipBad = true
	tr := &Tracker{}
	res, err := Copy(context.Background(), src, dst, o, tr)
	if err != nil {
		t.Fatal(err)
	}
	isBad := map[int64]bool{}
	for _, s := range badSectors {
		isBad[s] = true
	}
	for s := int64(0); s < 4096; s++ {
		got := dst.data[s*512 : (s+1)*512]
		if isBad[s] && !bytes.Equal(got, make([]byte, 512)) {
			t.Fatalf("bad sector %d wasn't zeroed", s)
		}
		if !isBad[s] && !bytes.Equal(got, src.data[s*512:(s+1)*512]) {
			t.Fatalf("good sector %d wasn't copied", s)
		}
	}
	want := []BadRange{{0, 1536}, {300 * 512, 512}, {2047 * 512, 512}, {4095 * 512, 512}}
	if len(res.Bad) != len(want) || res.BadBytes != 3072 || tr.Snapshot().Bad != 3072 {
		t.Fatalf("bad ranges %v (%d bytes)", res.Bad, res.BadBytes)
	}
	for i := range want {
		if res.Bad[i] != want[i] {
			t.Fatalf("bad ranges %v, want %v", res.Bad, want)
		}
	}
	if !res.Verified || len(res.Mismatches) != 0 {
		t.Fatal("verify should pass against the zero-filled data")
	}
}

func TestBadSectorSearchIsCheap(t *testing.T) {
	src := randomMem(4<<20, 512, 6)
	src.bad[5000] = -1
	o := testOpts(4 << 20)
	o.SkipBad, o.Verify = true, false
	if _, err := Copy(context.Background(), src, newMem(4<<20, 512), o, nil); err != nil {
		t.Fatal(err)
	}
	if src.reads > 60 {
		t.Fatalf("one bad sector out of 8192 took %d reads", src.reads)
	}
}

func TestDeviceGoneAbortsEvenWithSkipBad(t *testing.T) {
	src := randomMem(2<<20, 512, 8)
	src.fatalAt = 1 << 20
	o := testOpts(64 << 10)
	o.SkipBad = true
	if _, err := Copy(context.Background(), src, newMem(2<<20, 512), o, nil); !errors.Is(err, syscall.ENODEV) {
		t.Fatalf("expected ENODEV, got %v", err)
	}
}

func TestWriteErrorIsReported(t *testing.T) {
	src := randomMem(1<<20, 512, 9)
	dst := newMem(1<<20, 512)
	dst.failW = 512 << 10
	_, err := Copy(context.Background(), src, dst, testOpts(64<<10), nil)
	var ioe *IOError
	if !errors.As(err, &ioe) || ioe.Op != "write" || ioe.Offset != 512<<10 {
		t.Fatalf("expected a write error at 512K, got %v", err)
	}
}

func TestCancelKeepsOldPartitionTable(t *testing.T) {
	src := randomMem(8<<20, 512, 10)
	dst := newMem(8<<20, 512)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Copy(ctx, src, dst, testOpts(64<<10), nil); !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected cancel, got %v", err)
	}
	for _, w := range dst.writes {
		if w == 0 {
			t.Fatal("first block written despite cancel")
		}
	}
}

func TestPaddingFor4KTargets(t *testing.T) {
	src := randomMem(1<<20+512, 512, 11)
	dst := newMem(2<<20, 4096)
	for i := range dst.data {
		dst.data[i] = 0x77
	}
	res, err := Copy(context.Background(), src, dst, testOpts(64<<10), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 1<<20+4096 || !bytes.Equal(dst.data[:len(src.data)], src.data) ||
		!bytes.Equal(dst.data[len(src.data):1<<20+4096], make([]byte, 4096-512)) || dst.data[1<<20+4096] != 0x77 {
		t.Fatal("padding is wrong")
	}
}

type flipOnRead struct {
	*memDisk
	at int64
}

func (c *flipOnRead) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.memDisk.ReadAt(p, off)
	if c.at >= off && c.at < off+int64(n) {
		p[c.at-off] ^= 1
	}
	return n, err
}

func TestVerifyCatchesCorruption(t *testing.T) {
	src := randomMem(1<<20, 512, 12)
	dst := &flipOnRead{memDisk: newMem(1<<20, 512), at: 300 << 10}
	res, err := Copy(context.Background(), src, dst, testOpts(64<<10), nil)
	if err != nil || len(res.Mismatches) != 1 || res.Mismatches[0] != 256<<10 {
		t.Fatalf("expected one mismatch at 256K, got %v %v", res.Mismatches, err)
	}
}

func TestCompareHashScan(t *testing.T) {
	a := randomMem(1<<20, 512, 13)
	b := newMem(1<<20, 512)
	copy(b.data, a.data)
	b.data[12345] ^= 0xFF
	b.data[999999] ^= 1
	res, err := Compare(context.Background(), a, b, 64<<10, nil)
	if err != nil || res.DiffBytes != 2 || res.FirstDiffs[0] != 12345 || res.FirstDiffs[1] != 999999 {
		t.Fatalf("%+v %v", res, err)
	}
	sum, _ := Hash(context.Background(), a, 64<<10, nil)
	want := sha256.Sum256(a.data)
	if sum != hex.EncodeToString(want[:]) {
		t.Fatal("hash is wrong")
	}
	a.bad[10] = -1
	a.bad[11] = -1
	a.bad[2000] = -1
	sr, err := Scan(context.Background(), a, 256<<10, 0, nil)
	if err != nil || len(sr.Bad) != 2 || sr.BadBytes != 1536 || sr.Bad[0].Offset != 5120 {
		t.Fatalf("scan: %+v %v", sr, err)
	}
}

func TestWipe(t *testing.T) {
	for _, mode := range []WipeMode{WipeZeros, WipeRandom} {
		d := randomMem(3<<20+512, 512, 14)
		res, err := Wipe(context.Background(), d, mode, true, 1<<20, nil)
		if err != nil || !res.Verified || res.Bad != 0 {
			t.Fatalf("%s: %+v %v", mode, res, err)
		}
		zeros := bytes.Count(d.data, []byte{0})
		if mode == WipeZeros && zeros != len(d.data) {
			t.Fatal("zero wipe left data behind")
		}
		if mode == WipeRandom && zeros > len(d.data)/100 {
			t.Fatal("random wipe looks too zero-ish")
		}
	}
	// a disk that doesn't keep what we write must fail verify
	d := &flipOnRead{memDisk: newMem(1<<20, 512), at: 5}
	res, err := Wipe(context.Background(), d, WipeZeros, true, 256<<10, nil)
	if err != nil || res.Bad != 1 {
		t.Fatalf("expected 1 bad block, got %+v %v", res, err)
	}
}

func TestUnbufferedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.img")
	data := make([]byte, 64<<10+512)
	rand.Read(data)
	os.WriteFile(path, data, 0o644)
	d, err := openUnbuffered(path, int64(len(data)))
	if err != nil {
		t.Skip("no O_DIRECT here:", err)
	}
	defer d.Close()
	buf := alignedBuffer(64<<10, 4096)
	if err := readFull(d, buf[:512], 64<<10); err != nil || !bytes.Equal(buf[:512], data[64<<10:]) {
		t.Fatal("tail read wrong", err)
	}
}

func TestHelpers(t *testing.T) {
	if CleanName(`  "C:\My Images\a.img" `) != `C:\My Images\a.img` {
		t.Fatal("quotes not stripped")
	}
	if CommaInt(1234567) != "1,234,567" || HumanBytes(1536) != "1.5 KB" {
		t.Fatal("formatting")
	}
}
