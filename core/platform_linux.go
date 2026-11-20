//go:build linux

package core

// Linux support: block devices (/dev/sdX, /dev/nvme0n1p2, /dev/loopN) and
// image files. Windows is the main target - this is mostly here so the whole
// app can be developed and tested on Linux with loop devices.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func IsAdmin() bool  { return os.Geteuid() == 0 }
func Refresh() error { return nil }
func ErrorHint(err error) string {
	switch {
	case errors.Is(err, syscall.EBUSY):
		return "The target is in use (mounted, or part of RAID/LVM). Unmount it first."
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return "Run DragonClone as root."
	case errors.Is(err, syscall.EIO):
		return "The source has unreadable sectors. Turn on \"Skip unreadable sectors\" to fill them with zeros and keep going."
	}
	return ""
}

type linuxDev struct{ name string } // kernel name, e.g. sda1

func sysRead(name, file string) string {
	b, err := os.ReadFile("/sys/class/block/" + name + "/" + file)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func sysSize(name string) int64 {
	n, _ := strconv.ParseInt(sysRead(name, "size"), 10, 64)
	return n * 512
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func partitionsOf(disk string) []string {
	ents, _ := os.ReadDir("/sys/class/block/" + disk)
	var out []string
	for _, e := range ents {
		if exists("/sys/class/block/" + disk + "/" + e.Name() + "/partition") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func parentOf(name string) string {
	if !exists("/sys/class/block/" + name + "/partition") {
		return name
	}
	real, err := filepath.EvalSymlinks("/sys/class/block/" + name)
	if err != nil {
		return name
	}
	return filepath.Base(filepath.Dir(real))
}

func overlapsLinux(a, b string) bool {
	return a == b || parentOf(a) == b || parentOf(b) == a
}

type mountEntry struct{ dev, dir, fs string }

func mounts() []mountEntry {
	data, _ := os.ReadFile("/proc/mounts")
	var out []mountEntry
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		dev := f[0]
		if r, err := filepath.EvalSymlinks(dev); err == nil {
			dev = r
		}
		out = append(out, mountEntry{dev, f[1], f[2]})
	}
	return out
}

func mountOf(dev string) *mountEntry {
	for _, m := range mounts() {
		if m.dev == dev {
			return &m
		}
	}
	return nil
}

func sectorSizeOf(f *os.File) int {
	var v int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x1268 /* BLKSSZGET */, uintptr(unsafe.Pointer(&v))); e == 0 && v >= 512 {
		return int(v)
	}
	return 512
}

// Disks lists block devices from /sys/block.
func Disks() ([]DiskInfo, error) {
	ents, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}
	var out []DiskInfo
	for i, e := range ents {
		n := e.Name()
		size := sysSize(n)
		if size == 0 || strings.HasPrefix(n, "ram") || strings.HasPrefix(n, "zram") {
			continue
		}
		d := DiskInfo{ID: "/dev/" + n, Number: i, Model: sysRead(n, "device/model"), Serial: sysRead(n, "device/serial"),
			Size: size, SectorSize: 512, Style: "-", Removable: sysRead(n, "removable") == "1"}
		if s, err := strconv.Atoi(sysRead(n, "queue/logical_block_size")); err == nil && s >= 512 {
			d.SectorSize = s
		}
		switch {
		case strings.HasPrefix(n, "loop"):
			d.Bus = "Loop"
		case strings.HasPrefix(n, "nvme"):
			d.Bus = "NVMe"
		case strings.HasPrefix(n, "vd"):
			d.Bus = "Virtual"
		default:
			d.Bus = "SCSI"
		}
		if d.Model == "" {
			d.Model = strings.ToUpper(n[:1]) + n[1:]
		}
		if m := mountOf("/dev/" + n); m != nil && m.dir == "/" {
			d.System = true
		}
		for _, p := range partitionsOf(n) {
			num, _ := strconv.Atoi(sysRead(p, "partition"))
			start, _ := strconv.ParseInt(sysRead(p, "start"), 10, 64)
			pi := PartInfo{ID: "/dev/" + p, Number: num, Offset: start * 512, Size: sysSize(p), Type: "Partition", Used: -1}
			if m := mountOf("/dev/" + p); m != nil {
				pi.Letter, pi.FS = m.dir, m.fs
				var st syscall.Statfs_t
				if syscall.Statfs(m.dir, &st) == nil {
					pi.Used = int64(st.Blocks-st.Bfree) * int64(st.Bsize)
				}
				if m.dir == "/" || m.dir == "/boot" || m.dir == "/boot/efi" {
					pi.System, d.System = true, true
				}
			}
			d.Partitions = append(d.Partitions, pi)
		}
		out = append(out, d)
	}
	return out, nil
}

func resolveDevice(s string, role Role) (*Endpoint, bool, error) {
	if !strings.HasPrefix(s, "/dev/") {
		return nil, false, nil
	}
	real, err := filepath.EvalSymlinks(s)
	if err != nil {
		return nil, true, err
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, true, err
	}
	if st.Mode()&os.ModeDevice == 0 || st.Mode()&os.ModeCharDevice != 0 {
		return nil, true, fmt.Errorf("%s isn't a block device", real)
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, true, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, true, err
	}
	name := filepath.Base(real)
	whole := !exists("/sys/class/block/" + name + "/partition")
	model := sysRead(parentOf(name), "device/model")
	desc := fmt.Sprintf("%s, %s", real, HumanBytes(size))
	if model != "" {
		desc = fmt.Sprintf("%s (%s), %s", real, model, HumanBytes(size))
	}
	ep := &Endpoint{Name: real, Desc: desc, Path: real, Size: size, Sector: sectorSizeOf(f),
		WholeDisk: whole, Model: model, sys: &linuxDev{name}}
	if role == RoleTarget {
		for _, p := range append([]string{name}, partitionsOf(name)...) {
			if m := mountOf("/dev/" + p); m != nil {
				ep.Erases = append(ep.Erases, "/dev/"+p+" (mounted on "+m.dir+")")
			}
		}
	}
	return ep, true, nil
}

func isMounted(name string) bool {
	for _, p := range append([]string{name}, partitionsOf(name)...) {
		if mountOf("/dev/"+p) != nil {
			return true
		}
	}
	return mountOf("/dev/"+parentOf(name)) != nil
}

func platformCheckPair(src, dst *Endpoint, pc *PairCheck) error {
	sd, _ := src.sys.(*linuxDev)
	dd, _ := dst.sys.(*linuxDev)
	if dd != nil && isMounted(dd.name) {
		return fmt.Errorf("%s is mounted - unmount it first", dst.Name)
	}
	if sd != nil && dd != nil && overlapsLinux(sd.name, dd.name) {
		return errors.New("the source and the target overlap")
	}
	if dd != nil && src.IsFile && fileOnDevice(src.Path, dd.name) {
		return errors.New("the image file is stored on the target itself")
	}
	if sd != nil && dst.IsFile && fileOnDevice(filepath.Dir(dst.Path), sd.name) {
		return errors.New("the image can't be saved on the disk you're backing up")
	}
	if dst.IsFile {
		var fs syscall.Statfs_t
		if syscall.Statfs(filepath.Dir(dst.Path), &fs) == nil {
			free := int64(fs.Bavail) * int64(fs.Bsize)
			if dst.Exists {
				free += dst.Size
			}
			if dst.Format != FormatGzip && free < src.Size {
				return fmt.Errorf("not enough room: the image needs %s, only %s is free", HumanBytes(src.Size), HumanBytes(free))
			}
		}
	}
	return nil
}

// CheckWipeTarget runs the same safety checks as a copy target.
func CheckWipeTarget(ep *Endpoint) (*PairCheck, error) {
	if ep.IsFile {
		return nil, errors.New("pick a disk or partition to wipe")
	}
	if d, ok := ep.sys.(*linuxDev); ok && isMounted(d.name) {
		return nil, fmt.Errorf("%s is mounted - unmount it first", ep.Name)
	}
	return &PairCheck{}, nil
}

// fileOnDevice: does path live on block device `name` (or a partition of it)?
func fileOnDevice(path, name string) bool {
	var st syscall.Stat_t
	if syscall.Stat(path, &st) != nil {
		return false
	}
	dev := uint64(st.Dev)
	maj := (dev>>8)&0xfff | (dev>>32)&^0xfff
	mnr := dev&0xff | (dev>>12)&^0xff
	real, err := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/block/%d:%d", maj, mnr))
	if err != nil {
		return false
	}
	return overlapsLinux(filepath.Base(real), name)
}

func OpenSource(ep *Endpoint, allowLive bool) (Device, error) {
	if ep.IsFile {
		return OpenImageSource(ep)
	}
	f, err := os.Open(ep.Path)
	if err != nil {
		return nil, err
	}
	return &fileDevice{f: f, path: ep.Path, size: ep.Size, sector: ep.Sector}, nil
}

func OpenTarget(ep *Endpoint, opts *Options, meta *ImageMeta) (Device, error) {
	if ep.IsFile {
		return CreateImageTarget(ep, opts, meta)
	}
	// O_EXCL on a block device fails if anything has it mounted or claimed.
	f, err := os.OpenFile(ep.Path, os.O_RDWR|os.O_EXCL, 0)
	if err != nil {
		return nil, err
	}
	return &fileDevice{f: f, path: ep.Path, size: ep.Size, sector: ep.Sector, table: true}, nil
}

func openUnbuffered(path string, size int64) (Device, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECT|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &unbufferedFile{
		readOnce: func(p []byte, off int64) (int, error) {
			n, err := syscall.Pread(fd, p, off)
			if n < 0 {
				n = 0
			}
			if err == nil && n == 0 {
				err = io.EOF
			}
			return n, err
		},
		closer: func() error { return syscall.Close(fd) },
		size:   size,
	}, nil
}

func isFatalIOError(err error) bool {
	var en syscall.Errno
	if errors.As(err, &en) {
		switch en {
		case syscall.ENODEV, syscall.ENXIO, syscall.EBADF, syscall.EINVAL, syscall.EACCES,
			syscall.EPERM, syscall.EROFS, syscall.ENOSPC, syscall.ENOMEDIUM:
			return true
		}
	}
	return false
}

func Roots() []Root {
	out := []Root{{Path: "/", Label: "Root", Free: -1, Total: -1}}
	if h, err := os.UserHomeDir(); err == nil {
		out = append(out, Root{Path: h, Label: "Home", Free: -1, Total: -1})
	}
	for i := range out {
		var st syscall.Statfs_t
		if syscall.Statfs(out[i].Path, &st) == nil {
			out[i].Free = int64(st.Bavail) * int64(st.Bsize)
			out[i].Total = int64(st.Blocks) * int64(st.Bsize)
		}
	}
	return out
}

func MountImage(string) (string, error) {
	return "", errors.New("mounting images is only available on Windows")
}
func UnmountImage(string) error { return errors.New("only available on Windows") }
func ScheduleShutdown() error {
	return exec.Command("shutdown", "-h", "+1", "DragonClone finished").Run()
}
func CancelShutdown() error { return exec.Command("shutdown", "-c").Run() }
func ShowInFolder(path string) error {
	return exec.Command("xdg-open", filepath.Dir(path)).Start()
}
