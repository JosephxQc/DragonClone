package core

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// makeDisk returns a fake disk that looks a bit like a real one: random
// data, long runs of zeros, and some repeated patterns.
func makeDisk(size int, seed int64) *memDisk {
	m := newMem(size, 512)
	r := rand.New(rand.NewSource(seed))
	for off := 0; off < size; off += 64 << 10 {
		end := min(off+64<<10, size)
		switch r.Intn(3) {
		case 0:
			r.Read(m.data[off:end])
		case 1: // zeros
		case 2:
			for i := off; i < end; i++ {
				m.data[i] = byte(i % 251)
			}
		}
	}
	return m
}

func backup(t *testing.T, disk *memDisk, path string, verify bool) *CopyResult {
	t.Helper()
	ep, err := Resolve(path, RoleTarget, KindFile)
	if err != nil {
		t.Fatal(err)
	}
	meta := &ImageMeta{App: "DragonClone", Format: ep.Format, Size: disk.Size(), SectorSize: 512, Created: time.Now()}
	dst, err := OpenTarget(ep, DefaultOptions(), meta)
	if err != nil {
		t.Fatal(err)
	}
	o := testOpts(1 << 20)
	o.Verify = verify
	res, err := Copy(context.Background(), disk, dst, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	meta.SHA256 = res.SHA256
	SaveMeta(path, meta)
	return res
}

func restore(t *testing.T, path string, size int) *memDisk {
	t.Helper()
	ep, err := Resolve(path, RoleSource, KindFile)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Size != int64(size) {
		t.Fatalf("%s: image says %d bytes, want %d", path, ep.Size, size)
	}
	src, err := OpenSource(ep, false)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	out := newMem(size+1<<20, 512)
	if _, err := Copy(context.Background(), src, out, testOpts(1<<20), nil); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestImageRoundTrips(t *testing.T) {
	dir := t.TempDir()
	const size = 9<<20 + 4096
	disk := makeDisk(size, 42)
	for _, name := range []string{"disk.img", "disk.img.gz", "disk.vhd"} {
		path := filepath.Join(dir, name)
		res := backup(t, disk, path, true)
		if !res.Verified || len(res.Mismatches) != 0 {
			t.Fatalf("%s: verify failed %+v", name, res)
		}
		got := restore(t, path, size)
		if !bytes.Equal(got.data[:size], disk.data) {
			t.Fatalf("%s: restored disk differs", name)
		}
	}
	st, _ := os.Stat(filepath.Join(dir, "disk.img.gz"))
	if st.Size() > size/2 {
		t.Fatalf("compressed image is %d bytes - compression isn't doing much", st.Size())
	}
}

func TestCompressedImageIsPlainGzip(t *testing.T) {
	dir := t.TempDir()
	disk := makeDisk(5<<20, 7)
	path := filepath.Join(dir, "x.img.gz")
	backup(t, disk, path, false)

	f, _ := os.Open(path)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeGzipExtra(zr.Header.Extra); m == nil || m.Size != 5<<20 {
		t.Fatal("metadata missing from the gzip header")
	}
	all, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(all, disk.data) {
		t.Fatal("stdlib gunzip doesn't give back the disk", err)
	}
	if gz, err := exec.LookPath("gzip"); err == nil && runtime.GOOS != "windows" {
		out, err := exec.Command(gz, "-dc", path).Output()
		if err != nil || !bytes.Equal(out, disk.data) {
			t.Fatal("system gzip can't unpack the image", err)
		}
	}
}

func TestForeignGzipWithoutMetadata(t *testing.T) {
	dir := t.TempDir()
	disk := makeDisk(3<<20, 9)
	path := filepath.Join(dir, "foreign.img.gz")
	f, _ := os.Create(path)
	zw := gzip.NewWriter(f)
	zw.Write(disk.data)
	zw.Close()
	f.Close()
	got := restore(t, path, 3<<20)
	if !bytes.Equal(got.data[:3<<20], disk.data) {
		t.Fatal("foreign gzip restore differs")
	}
}

func TestDamagedGzipIsCaught(t *testing.T) {
	dir := t.TempDir()
	disk := makeDisk(4<<20, 11)
	path := filepath.Join(dir, "bad.img.gz")
	backup(t, disk, path, false)
	b, _ := os.ReadFile(path)
	b[len(b)/2] ^= 0xFF
	os.WriteFile(path, b, 0o644)
	ep, err := Resolve(path, RoleSource, KindFile)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := OpenSource(ep, false)
	defer src.Close()
	o := testOpts(1 << 20)
	o.SkipBad = true // even then, a corrupt archive must stop the restore
	if _, err := Copy(context.Background(), src, newMem(5<<20, 512), o, nil); err == nil {
		t.Fatal("a damaged compressed image restored without complaint")
	}
}

func TestVHDFooter(t *testing.T) {
	ft := vhdFooter(10<<20, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if string(ft[:8]) != "conectix" || vhdChecksum(ft) != binary.BigEndian.Uint32(ft[64:68]) {
		t.Fatal("bad cookie or checksum")
	}
	// Spot-check the CHS maths against the spec's algorithm on known sizes.
	cases := map[int64][3]int{
		10 << 20:  {300, 4, 17},
		1 << 30:   {2080, 16, 63},
		128 << 30: {16383 * 4, 16, 255},
	}
	for size, want := range cases {
		c, h, s := vhdGeometry(size)
		if want[0] != 0 && (int(h) != want[1] || int(s) != want[2]) {
			t.Fatalf("geometry for %d: got %d/%d/%d, want heads %d spt %d", size, c, h, s, want[1], want[2])
		}
	}
}

// qemu-img knows VHD ("vpc") inside out, so if it's installed let it judge.
func TestVHDReadableByQemu(t *testing.T) {
	qemu, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img not installed")
	}
	dir := t.TempDir()
	disk := makeDisk(6<<20, 13)
	path := filepath.Join(dir, "x.vhd")
	backup(t, disk, path, false)
	out, err := exec.Command(qemu, "info", "-f", "vpc", path).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "6 MiB") {
		t.Fatalf("qemu-img info: %v\n%s", err, out)
	}
	raw := filepath.Join(dir, "x.raw")
	if out, err := exec.Command(qemu, "convert", "-f", "vpc", "-O", "raw", path, raw).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img convert: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(raw)
	if !bytes.Equal(b, disk.data) {
		t.Fatal("qemu reads different data out of our VHD")
	}
}

func TestResolveImageTargets(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]ImageFormat{"a.img": FormatRaw, "a.iso": FormatRaw, "a.img.gz": FormatGzip, "a.vhd": FormatVHD, "A.VHD": FormatVHD}
	for name, want := range cases {
		ep, err := Resolve(filepath.Join(dir, name), RoleTarget, KindFile)
		if err != nil || ep.Format != want {
			t.Fatalf("%s: %v %v", name, ep, err)
		}
	}
	if _, err := Resolve(filepath.Join(dir, "nope", "a.img"), RoleTarget, KindFile); err == nil {
		t.Fatal("missing folder should be an error")
	}
	if _, err := Resolve(dir, RoleTarget, KindFile); err == nil {
		t.Fatal("a folder isn't a valid image name")
	}
}

func TestParseGzipExtraIgnoresOtherFields(t *testing.T) {
	m := &ImageMeta{Size: 123, SectorSize: 512}
	extra := append([]byte{'X', 'Y', 2, 0, 1, 2}, encodeGzipExtra(m)...)
	if got := decodeGzipExtra(extra); got == nil || got.Size != 123 {
		t.Fatal("didn't find our field after a foreign one")
	}
}
