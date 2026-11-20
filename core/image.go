package core

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ImageFormat is how an image file stores the disk.
type ImageFormat string

const (
	FormatRaw  ImageFormat = "raw"  // .img / .iso - byte for byte
	FormatGzip ImageFormat = "gzip" // .img.gz - compressed, any gunzip can unpack it
	FormatVHD  ImageFormat = "vhd"  // fixed .vhd - raw + footer, Windows can mount it
)

// AppVersion ends up in image metadata. main sets it.
var AppVersion = "dev"

// FormatFromName guesses the format from a file name.
func FormatFromName(path string) ImageFormat {
	p := strings.ToLower(path)
	switch {
	case strings.HasSuffix(p, ".gz"):
		return FormatGzip
	case strings.HasSuffix(p, ".vhd"):
		return FormatVHD
	}
	return FormatRaw
}

// Extension is the usual file ending for a format.
func (f ImageFormat) Extension() string {
	switch f {
	case FormatGzip:
		return ".img.gz"
	case FormatVHD:
		return ".vhd"
	}
	return ".img"
}

func (f ImageFormat) Label() string {
	switch f {
	case FormatGzip:
		return "compressed image"
	case FormatVHD:
		return "virtual disk (VHD)"
	}
	return "raw image"
}

// ImageMeta is saved next to every image we make (<image>.json) and inside
// compressed images, so a restore knows where the image came from.
type ImageMeta struct {
	App        string      `json:"app"`
	Version    string      `json:"version"`
	Created    time.Time   `json:"created"`
	Format     ImageFormat `json:"format"`
	Source     string      `json:"source"`
	SourceName string      `json:"sourceName"`
	Model      string      `json:"model,omitempty"`
	Style      string      `json:"style,omitempty"`
	Size       int64       `json:"size"`
	SectorSize int         `json:"sectorSize"`
	SHA256     string      `json:"sha256,omitempty"`
	BadBytes   int64       `json:"badBytes,omitempty"`
}

func metaPath(image string) string { return image + ".json" }

func loadMeta(image string) *ImageMeta {
	b, err := os.ReadFile(metaPath(image))
	if err != nil {
		return nil
	}
	var m ImageMeta
	if json.Unmarshal(b, &m) != nil || m.Size <= 0 {
		return nil
	}
	return &m
}

// SaveMeta writes <image>.json, and <image>.sha256 for raw images so the
// usual checksum tools can check them too.
func SaveMeta(image string, m *ImageMeta) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(metaPath(image), b, 0o666); err != nil {
		return err
	}
	if m.Format == FormatRaw && m.SHA256 != "" {
		line := m.SHA256 + " *" + filepath.Base(image) + "\n"
		return os.WriteFile(image+".sha256", []byte(line), 0o666)
	}
	return nil
}

// RemoveImage deletes an unfinished image and its side files.
func RemoveImage(path string) {
	os.Remove(path)
	os.Remove(metaPath(path))
	os.Remove(path + ".sha256")
}

// inspectImage works out the format and the disk size of an existing image.
func inspectImage(path string, fileSize int64) (*Endpoint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ep := &Endpoint{Name: path, IsFile: true, Path: path, Sector: 512, Exists: true, Format: FormatRaw, Size: fileSize}
	ep.Meta = loadMeta(path)

	head := make([]byte, 2)
	io.ReadFull(f, head)
	switch {
	case fileSize >= 1024 && hasVHDFooter(f, fileSize):
		size, err := readVHDFooter(f, fileSize)
		if err != nil {
			return nil, err
		}
		ep.Format, ep.Size = FormatVHD, size
	case head[0] == 0x1f && head[1] == 0x8b:
		ep.Format = FormatGzip
		size, m, err := gzipImageSize(f, ep.Meta)
		if err != nil {
			return nil, err
		}
		ep.Size = size
		if ep.Meta == nil {
			ep.Meta = m
		}
	}
	if ep.Meta != nil {
		ep.Model, ep.Style = ep.Meta.Model, ep.Meta.Style
	}
	ep.Desc = fmt.Sprintf("%s (%s, %s disk)", filepath.Base(path), ep.Format.Label(), HumanBytes(ep.Size))
	return ep, nil
}

// OpenImageSource opens an image for reading the disk inside it.
func OpenImageSource(ep *Endpoint) (Device, error) {
	f, err := os.Open(ep.Path)
	if err != nil {
		return nil, err
	}
	switch ep.Format {
	case FormatVHD:
		return &window{Device: &fileDevice{f: f, path: ep.Path, size: ep.Size + 512, sector: 512}, size: ep.Size}, nil
	case FormatGzip:
		return newGzipSource(f, ep.Size)
	}
	return &fileDevice{f: f, path: ep.Path, size: ep.Size, sector: 512}, nil
}

// CreateImageTarget creates a new image file in the endpoint's format.
func CreateImageTarget(ep *Endpoint, opts *Options, meta *ImageMeta) (Device, error) {
	f, err := os.OpenFile(ep.Path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return nil, err
	}
	fd := &fileDevice{f: f, path: ep.Path, sector: 512, growable: true}
	switch ep.Format {
	case FormatVHD:
		return &vhdTarget{fileDevice: fd, created: time.Now()}, nil
	case FormatGzip:
		return newGzipTarget(f, ep.Path, opts.Level, meta)
	}
	return fd, nil
}

// ---- gzip helpers ----------------------------------------------------------

// Our compressed images start with an empty gzip member whose header carries
// the metadata in an extra field tagged "DC".
func encodeGzipExtra(m *ImageMeta) []byte {
	data, _ := json.Marshal(m)
	if len(data) > 60000 {
		data = data[:0]
	}
	out := []byte{'D', 'C', 0, 0}
	binary.LittleEndian.PutUint16(out[2:], uint16(len(data)))
	return append(out, data...)
}

func decodeGzipExtra(extra []byte) *ImageMeta {
	for len(extra) >= 4 {
		n := int(binary.LittleEndian.Uint16(extra[2:4]))
		if 4+n > len(extra) {
			return nil
		}
		if extra[0] == 'D' && extra[1] == 'C' {
			var m ImageMeta
			if json.Unmarshal(extra[4:4+n], &m) == nil && m.Size > 0 {
				return &m
			}
		}
		extra = extra[4+n:]
	}
	return nil
}

// gzipImageSize finds out how big the disk inside a .gz is: from our own
// header if it's there, else from the .json next to it, else by unpacking
// the whole thing once and counting (slow, but only for foreign images).
func gzipImageSize(f *os.File, side *ImageMeta) (int64, *ImageMeta, error) {
	f.Seek(0, io.SeekStart)
	zr, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("not a valid gzip file: %w", err)
	}
	if m := decodeGzipExtra(zr.Header.Extra); m != nil {
		return m.Size, m, nil
	}
	if side != nil {
		return side.Size, side, nil
	}
	n, err := io.Copy(io.Discard, zr)
	if err != nil {
		return 0, nil, fmt.Errorf("the compressed image is damaged: %w", err)
	}
	return n, nil, nil
}

// ---- VHD -----------------------------------------------------------------

// Fixed VHDs top out at 2040 GB (binary).
const maxVHDSize = 2040 << 30

func hasVHDFooter(f *os.File, fileSize int64) bool {
	cookie := make([]byte, 8)
	if _, err := f.ReadAt(cookie, fileSize-512); err != nil {
		return false
	}
	return bytes.Equal(cookie, []byte("conectix"))
}

func readVHDFooter(f *os.File, fileSize int64) (int64, error) {
	ft := make([]byte, 512)
	if _, err := f.ReadAt(ft, fileSize-512); err != nil {
		return 0, err
	}
	stored := binary.BigEndian.Uint32(ft[64:68])
	if vhdChecksum(ft) != stored {
		return 0, errors.New("the VHD footer is damaged (bad checksum)")
	}
	if t := binary.BigEndian.Uint32(ft[60:64]); t != 2 {
		return 0, errors.New("only fixed-size VHDs are supported (this one is dynamic or differencing)")
	}
	size := int64(binary.BigEndian.Uint64(ft[48:56]))
	if size <= 0 || size > fileSize-512 {
		return 0, errors.New("the VHD footer gives an impossible disk size")
	}
	return size, nil
}

// vhdTarget is a raw image that gets a VHD footer once it's complete.
type vhdTarget struct {
	*fileDevice
	created  time.Time
	dataSize int64
	done     bool
}

func (v *vhdTarget) Size() int64 {
	if v.done {
		return v.dataSize
	}
	return v.fileDevice.Size()
}

func (v *vhdTarget) Sync() error {
	if v.done {
		return v.f.Sync()
	}
	size := roundUp64(v.fileDevice.size, 512)
	if size > v.fileDevice.size {
		if _, err := v.f.WriteAt(make([]byte, size-v.fileDevice.size), v.fileDevice.size); err != nil {
			return err
		}
	}
	if _, err := v.f.WriteAt(vhdFooter(size, v.created), size); err != nil {
		return err
	}
	v.dataSize, v.done = size, true
	return v.f.Sync()
}

func (v *vhdTarget) OpenForVerify() (Device, error) {
	d, err := openUnbuffered(v.path, v.dataSize+512)
	if err != nil {
		return nil, err
	}
	return &window{Device: d, size: v.dataSize}, nil
}

// vhdFooter builds the 512-byte footer of a fixed VHD (spec: "Virtual Hard
// Disk Image Format Specification", hard disk footer format).
func vhdFooter(size int64, created time.Time) []byte {
	ft := make([]byte, 512)
	be := binary.BigEndian
	copy(ft[0:], "conectix")
	be.PutUint32(ft[8:], 2)           // features: reserved bit, always set
	be.PutUint32(ft[12:], 0x00010000) // format version 1.0
	be.PutUint64(ft[16:], ^uint64(0)) // data offset: none for fixed disks
	epoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	be.PutUint32(ft[24:], uint32(created.UTC().Sub(epoch)/time.Second))
	// Creator app. Readers like qemu use the CHS geometry (which rounds the
	// size down) unless the creator is one known to trust "current size", so
	// we use the same tag Windows' own VHD code writes.
	copy(ft[28:], "win ")
	be.PutUint32(ft[32:], 0x00010000) // creator version
	be.PutUint32(ft[36:], 0x5769326B) // "Wi2k"
	be.PutUint64(ft[40:], uint64(size))
	be.PutUint64(ft[48:], uint64(size))
	c, h, s := vhdGeometry(size)
	be.PutUint16(ft[56:], c)
	ft[58], ft[59] = h, s
	be.PutUint32(ft[60:], 2) // fixed disk
	rand.Read(ft[68:84])     // unique id
	be.PutUint32(ft[64:], vhdChecksum(ft))
	return ft
}

func vhdChecksum(ft []byte) uint32 {
	var sum uint32
	for i, b := range ft {
		if i >= 64 && i < 68 {
			continue
		}
		sum += uint32(b)
	}
	return ^sum
}

// vhdGeometry is the CHS calculation from the VHD spec appendix.
func vhdGeometry(size int64) (cyl uint16, heads, spt uint8) {
	total := size / 512
	if total > 65535*16*255 {
		total = 65535 * 16 * 255
	}
	var s, h, cth int64
	if total >= 65535*16*63 {
		s, h = 255, 16
		cth = total / s
	} else {
		s = 17
		cth = total / s
		h = (cth + 1023) / 1024
		if h < 4 {
			h = 4
		}
		if cth >= h*1024 || h > 16 {
			s, h = 31, 16
			cth = total / s
		}
		if cth >= h*1024 {
			s, h = 63, 16
			cth = total / s
		}
	}
	return uint16(cth / h), uint8(h), uint8(s)
}
