//go:build windows

package core

// How this works on Windows: every read and write goes through a
// \\.\PhysicalDriveN handle, and a partition is just a byte range on its disk.
// Volume handles are only opened to lock the file systems sitting on the
// range we touch (and dismount them for targets) - Windows refuses raw writes
// into a mounted volume otherwise.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	genericRead          = 0x80000000
	genericWrite         = 0x40000000
	fileShareRead        = 0x1
	fileShareWrite       = 0x2
	openExisting         = 3
	fileFlagNoBuffering  = 0x20000000
	fileFlagWriteThrough = 0x80000000
	createNoWindow       = 0x08000000

	ioctlDiskGetLengthInfo          = 0x0007405C
	ioctlDiskGetDriveGeometryEx     = 0x000700A0
	ioctlDiskGetDriveLayoutEx       = 0x00070050
	ioctlDiskUpdateProperties       = 0x00070140
	ioctlStorageQueryProperty       = 0x002D1400
	ioctlVolumeGetVolumeDiskExtents = 0x00560000
	fsctlLockVolume                 = 0x00090018
	fsctlUnlockVolume               = 0x0009001C
	fsctlDismountVolume             = 0x00090020

	errPathNotFound     = syscall.Errno(3)
	errInvalidHandle    = syscall.Errno(6)
	errWriteProtect     = syscall.Errno(19)
	errNotReady         = syscall.Errno(21)
	errCRC              = syscall.Errno(23)
	errSectorNotFound   = syscall.Errno(27)
	errReadFault        = syscall.Errno(30)
	errHandleEOF        = syscall.Errno(38)
	errDevNotExist      = syscall.Errno(55)
	errInvalidParameter = syscall.Errno(87)
	errDiskFull         = syscall.Errno(112)
	errInsufficientBuf  = syscall.Errno(122)
	errFileTooLarge     = syscall.Errno(223)
	errMoreData         = syscall.Errno(234)
	errNoSuchDevice     = syscall.Errno(433)
	errMediaChanged     = syscall.Errno(1110)
	errNoMedia          = syscall.Errno(1112)
	errIODevice         = syscall.Errno(1117)
	errDeviceNotConnect = syscall.Errno(1167)
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	procFindFirstVolumeW                  = kernel32.NewProc("FindFirstVolumeW")
	procFindNextVolumeW                   = kernel32.NewProc("FindNextVolumeW")
	procFindVolumeClose                   = kernel32.NewProc("FindVolumeClose")
	procGetVolumePathNamesForVolumeNameW  = kernel32.NewProc("GetVolumePathNamesForVolumeNameW")
	procGetVolumeInformationW             = kernel32.NewProc("GetVolumeInformationW")
	procGetVolumePathNameW                = kernel32.NewProc("GetVolumePathNameW")
	procGetVolumeNameForVolumeMountPointW = kernel32.NewProc("GetVolumeNameForVolumeMountPointW")
	procGetDiskFreeSpaceExW               = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetWindowsDirectoryW              = kernel32.NewProc("GetWindowsDirectoryW")
	procQueryDosDeviceW                   = kernel32.NewProc("QueryDosDeviceW")
	procGetLogicalDrives                  = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW                     = kernel32.NewProc("GetDriveTypeW")
	procIsUserAnAdmin                     = shell32.NewProc("IsUserAnAdmin")
)

func init() {
	// Don't let Windows pop up "There is no disk in the drive" boxes when we
	// poke at empty card readers.
	const semFailCriticalErrors, semNoOpenFileErrorBox = 0x0001, 0x8000
	kernel32.NewProc("SetErrorMode").Call(semFailCriticalErrors | semNoOpenFileErrorBox)
}

// IsAdmin reports whether we're running elevated.
func IsAdmin() bool {
	r, _, _ := procIsUserAnAdmin.Call()
	return r != 0
}

func utf16p(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		p, _ = syscall.UTF16PtrFromString("")
	}
	return p
}

func ptr(s string) uintptr { return uintptr(unsafe.Pointer(utf16p(s))) }

func openHandle(path string, access, flags uint32) (syscall.Handle, error) {
	return syscall.CreateFile(utf16p(path), access, fileShareRead|fileShareWrite, nil, openExisting, flags, 0)
}

func ioctl(h syscall.Handle, code uint32, in, out []byte) (int, error) {
	var inP, outP *byte
	if len(in) > 0 {
		inP = &in[0]
	}
	if len(out) > 0 {
		outP = &out[0]
	}
	var ret uint32
	err := syscall.DeviceIoControl(h, code, inP, uint32(len(in)), outP, uint32(len(out)), &ret, nil)
	return int(ret), err
}

// ioctlGrow keeps doubling the buffer until the driver is happy.
func ioctlGrow(h syscall.Handle, code uint32, size int) ([]byte, error) {
	for i := 0; i < 6; i++ {
		buf := make([]byte, size)
		n, err := ioctl(h, code, nil, buf)
		if err == errInsufficientBuf || err == errMoreData {
			size *= 4
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
	return nil, errInsufficientBuf
}

// ---- inventory -------------------------------------------------------------

type winDiskInfo struct {
	num       int
	path      string
	size      int64
	sector    int
	model     string
	serial    string
	bus       string
	removable bool
	style     string
	parts     []rawPart
	err       error // no media in a card reader etc.
}

func (d *winDiskInfo) partByNum(n int) *rawPart {
	for i := range d.parts {
		if d.parts[i].num == n {
			return &d.parts[i]
		}
	}
	return nil
}

func (d *winDiskInfo) partAt(off int64) *rawPart {
	for i := range d.parts {
		if d.parts[i].off == off {
			return &d.parts[i]
		}
	}
	return nil
}

func (d *winDiskInfo) label() string {
	s := d.model
	if s == "" {
		s = "Unknown disk"
	}
	if d.bus != "" && d.bus != "?" {
		s += " (" + d.bus + ")"
	}
	return s
}

type sysInfo struct {
	disks    []*winDiskInfo
	vols     []*volInfo
	winVol   *volInfo // the volume Windows runs from
	bootVol  *volInfo // the EFI / system partition the PC boots from
	sysDisks map[int]bool
}

var (
	sysMu  sync.Mutex
	cached *sysInfo
)

// Refresh re-reads the disk layout. Call it before resolving names.
func Refresh() error {
	si, err := loadSysInfo()
	if err != nil {
		return err
	}
	sysMu.Lock()
	cached = si
	sysMu.Unlock()
	return nil
}

func getSys() (*sysInfo, error) {
	sysMu.Lock()
	si := cached
	sysMu.Unlock()
	if si != nil {
		return si, nil
	}
	if err := Refresh(); err != nil {
		return nil, err
	}
	return getSys()
}

func (si *sysInfo) disk(n int) *winDiskInfo {
	for _, d := range si.disks {
		if d.num == n {
			return d
		}
	}
	return nil
}

func (si *sysInfo) volumeByLetter(letter string) *volInfo {
	want := strings.ToUpper(letter) + `:\`
	for _, v := range si.vols {
		for _, m := range v.mounts {
			if strings.EqualFold(m, want) {
				return v
			}
		}
	}
	return nil
}

// volumesIn lists the volumes that touch a byte range of a disk.
func (si *sysInfo) volumesIn(disk int, off, length int64) []*volInfo {
	var out []*volInfo
	for _, v := range si.vols {
		for _, e := range v.extents {
			if e.disk == disk && e.off < off+length && off < e.off+e.length {
				out = append(out, v)
				break
			}
		}
	}
	return out
}

func loadSysInfo() (*sysInfo, error) {
	si := &sysInfo{sysDisks: map[int]bool{}}
	si.vols = enumVolumes()
	misses := 0
	for n := 0; n < 128 && misses < 32; n++ {
		d, err := queryDisk(n)
		if err != nil {
			if err == syscall.ERROR_ACCESS_DENIED {
				return nil, errors.New("access denied while opening the disks - DragonClone needs to run as administrator")
			}
			misses++
			continue
		}
		misses = 0
		si.disks = append(si.disks, d)
	}
	for _, v := range si.vols {
		for _, e := range v.extents {
			if d := si.disk(e.disk); d != nil {
				if p := d.partAt(e.off); p != nil {
					p.vol = v
				}
			}
		}
	}
	// The partition the PC boots from (EFI or "System Reserved").
	if sp := systemPartitionDevice(); sp != "" {
		for _, v := range si.vols {
			if strings.EqualFold(ntDeviceName(v.guidPath), sp) {
				si.bootVol = v
				for _, e := range v.extents {
					si.sysDisks[e.disk] = true
				}
			}
		}
	}
	// The partition Windows itself lives on.
	if win := windowsVolumeName(); win != "" {
		for _, v := range si.vols {
			if strings.EqualFold(v.guidPath, win) {
				si.winVol = v
				for _, e := range v.extents {
					si.sysDisks[e.disk] = true
				}
			}
		}
	}
	return si, nil
}

func queryDisk(n int) (*winDiskInfo, error) {
	path := `\\.\PhysicalDrive` + strconv.Itoa(n)
	h, err := openHandle(path, genericRead, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(h)
	d := &winDiskInfo{num: n, path: path, sector: 512}

	query := make([]byte, 12) // StorageDeviceProperty + PropertyStandardQuery, both zero
	out := make([]byte, 1024)
	if n, err := ioctl(h, ioctlStorageQueryProperty, query, out); err == nil {
		d.model, d.serial, d.bus, d.removable = parseStorageDescriptor(out[:n])
	}
	if g, err := ioctlGrow(h, ioctlDiskGetDriveGeometryEx, 256); err == nil {
		if s := parseGeometrySector(g); s != 0 {
			d.sector = s
		}
	}
	lb := make([]byte, 8)
	if _, err := ioctl(h, ioctlDiskGetLengthInfo, nil, lb); err != nil {
		d.err = err
		return d, nil
	}
	d.size = int64(le.Uint64(lb))
	if l, err := ioctlGrow(h, ioctlDiskGetDriveLayoutEx, sizeofDriveLayoutHeader+sizeofPartitionInfoEx*128); err == nil {
		d.style, d.parts, _ = parseDriveLayout(l)
	}
	return d, nil
}

func enumVolumes() []*volInfo {
	buf := make([]uint16, 1024)
	r, _, _ := procFindFirstVolumeW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	h := syscall.Handle(r)
	if h == syscall.InvalidHandle {
		return nil
	}
	defer procFindVolumeClose.Call(uintptr(h))
	var vols []*volInfo
	for {
		vols = append(vols, describeVolume(syscall.UTF16ToString(buf)))
		if r, _, _ := procFindNextVolumeW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r == 0 {
			break
		}
	}
	return vols
}

func describeVolume(guid string) *volInfo {
	v := &volInfo{guidPath: guid, total: -1, free: -1}
	names := make([]uint16, 4096)
	var ret uint32
	if r, _, _ := procGetVolumePathNamesForVolumeNameW.Call(ptr(guid),
		uintptr(unsafe.Pointer(&names[0])), uintptr(len(names)), uintptr(unsafe.Pointer(&ret))); r != 0 {
		v.mounts = multiSZ(names)
	}
	label := make([]uint16, 261)
	fs := make([]uint16, 261)
	if r, _, _ := procGetVolumeInformationW.Call(ptr(guid),
		uintptr(unsafe.Pointer(&label[0])), uintptr(len(label)), 0, 0, 0,
		uintptr(unsafe.Pointer(&fs[0])), uintptr(len(fs))); r != 0 {
		v.label = syscall.UTF16ToString(label)
		v.fs = syscall.UTF16ToString(fs)
	}
	if len(v.mounts) > 0 {
		var free, total uint64
		if r, _, _ := procGetDiskFreeSpaceExW.Call(ptr(v.mounts[0]), 0,
			uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free))); r != 0 {
			v.total, v.free = int64(total), int64(free)
		}
	}
	if h, err := openHandle(strings.TrimSuffix(guid, `\`), 0, 0); err == nil {
		if b, err := ioctlGrow(h, ioctlVolumeGetVolumeDiskExtents, 8+sizeofDiskExtent*4); err == nil {
			v.extents = parseDiskExtents(b)
		}
		syscall.CloseHandle(h)
	}
	return v
}

func multiSZ(b []uint16) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i == start {
				break
			}
			out = append(out, syscall.UTF16ToString(b[start:i]))
			start = i + 1
		}
	}
	return out
}

func windowsVolumeName() string {
	buf := make([]uint16, 512)
	if r, _, _ := procGetWindowsDirectoryW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r == 0 {
		return ""
	}
	return volumeGUIDForPath(syscall.UTF16ToString(buf))
}

// systemPartitionDevice reads which partition the PC boots from, as Windows
// setup recorded it, e.g. \Device\HarddiskVolume1.
func systemPartitionDevice() string {
	var k syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, utf16p(`SYSTEM\Setup`), 0, syscall.KEY_READ, &k) != nil {
		return ""
	}
	defer syscall.RegCloseKey(k)
	buf := make([]uint16, 512)
	n := uint32(len(buf) * 2)
	var typ uint32
	if syscall.RegQueryValueEx(k, utf16p("SystemPartition"), nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n) != nil {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

// ntDeviceName turns \\?\Volume{...}\ into its NT name (\Device\HarddiskVolume3).
func ntDeviceName(guidPath string) string {
	name := strings.TrimSuffix(strings.TrimPrefix(guidPath, `\\?\`), `\`)
	buf := make([]uint16, 1024)
	if r, _, _ := procQueryDosDeviceW.Call(ptr(name), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

func volumeRootForPath(path string) string {
	buf := make([]uint16, 1024)
	if r, _, _ := procGetVolumePathNameW.Call(ptr(path), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

func volumeGUIDForPath(path string) string {
	root := volumeRootForPath(path)
	if root == "" {
		return ""
	}
	buf := make([]uint16, 128)
	if r, _, _ := procGetVolumeNameForVolumeMountPointW.Call(ptr(root), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

// Disks lists every disk with its partitions.
func Disks() ([]DiskInfo, error) {
	if err := Refresh(); err != nil {
		return nil, err
	}
	si, _ := getSys()
	var out []DiskInfo
	for _, d := range si.disks {
		di := DiskInfo{
			ID: fmt.Sprintf("disk%d", d.num), Number: d.num, Model: d.model, Serial: d.serial, Bus: d.bus,
			Size: d.size, SectorSize: d.sector, Style: d.style, Removable: d.removable,
			System: si.sysDisks[d.num], NoMedia: d.err != nil,
		}
		if di.Model == "" {
			di.Model = "Unknown disk"
		}
		for _, p := range d.parts {
			pi := PartInfo{ID: fmt.Sprintf("disk%dp%d", d.num, p.num), Number: p.num, Offset: p.off,
				Size: p.length, Type: p.typeName, Name: p.name, Used: -1}
			if v := p.vol; v != nil {
				pi.Label, pi.FS = v.label, v.fs
				if len(v.mounts) > 0 && len(v.mounts[0]) <= 3 {
					pi.Letter = strings.TrimSuffix(v.mounts[0], `\`)
				}
				if v.total > 0 {
					pi.Used = v.total - v.free
				}
				pi.System = v == si.winVol || v == si.bootVol
			}
			di.Partitions = append(di.Partitions, pi)
		}
		out = append(out, di)
	}
	return out, nil
}

// ---- names -> endpoints ----------------------------------------------------

var (
	reDisk     = regexp.MustCompile(`(?i)^(?:disk|\\\\\.\\physicaldrive)(\d+)$`)
	reDiskPart = regexp.MustCompile(`(?i)^disk(\d+)p(\d+)$`)
	reLetter   = regexp.MustCompile(`(?i)^(?:\\\\\.\\)?([a-z]):\\?$`)
)

type winEP struct {
	disk   *winDiskInfo
	part   *rawPart // nil means the whole disk
	vol    *volInfo // set when picked by drive letter
	off    int64
	length int64
}

func resolveDevice(s string, role Role) (*Endpoint, bool, error) {
	dn, pn, letter := -1, -1, ""
	if m := reDiskPart.FindStringSubmatch(s); m != nil {
		dn, _ = strconv.Atoi(m[1])
		pn, _ = strconv.Atoi(m[2])
	} else if m := reDisk.FindStringSubmatch(s); m != nil {
		dn, _ = strconv.Atoi(m[1])
	} else if m := reLetter.FindStringSubmatch(s); m != nil {
		letter = strings.ToUpper(m[1])
	} else {
		return nil, false, nil
	}
	si, err := getSys()
	if err != nil {
		return nil, true, err
	}
	we := &winEP{}
	var name string
	if letter != "" {
		v := si.volumeByLetter(letter)
		if v == nil {
			return nil, true, fmt.Errorf("there's no drive %s:", letter)
		}
		if len(v.extents) != 1 {
			return nil, true, fmt.Errorf("drive %s: is spread over several disks (dynamic disk, Storage Spaces or RAID) - clone the whole disks instead", letter)
		}
		e := v.extents[0]
		if we.disk = si.disk(e.disk); we.disk == nil {
			return nil, true, fmt.Errorf("couldn't find the disk that holds %s:", letter)
		}
		we.vol, we.off, we.length = v, e.off, e.length
		we.part = we.disk.partAt(e.off)
		name = letter + ":"
	} else {
		if we.disk = si.disk(dn); we.disk == nil {
			return nil, true, fmt.Errorf("there's no disk %d", dn)
		}
		if we.disk.err != nil {
			return nil, true, fmt.Errorf("disk %d has no media", dn)
		}
		if pn >= 0 {
			if we.part = we.disk.partByNum(pn); we.part == nil {
				return nil, true, fmt.Errorf("disk %d has no partition %d", dn, pn)
			}
			we.off, we.length = we.part.off, we.part.length
			name = fmt.Sprintf("disk%dp%d", dn, pn)
		} else {
			we.length = we.disk.size
			name = fmt.Sprintf("disk%d", dn)
		}
	}

	d := we.disk
	whole := we.part == nil && we.vol == nil
	var desc string
	if whole {
		desc = fmt.Sprintf("Disk %d - %s, %s", d.num, d.label(), HumanBytes(d.size))
	} else {
		v := we.vol
		if v == nil && we.part != nil {
			v = we.part.vol
		}
		what := "Partition"
		if we.part != nil {
			what = fmt.Sprintf("Partition %d", we.part.num)
		}
		desc = fmt.Sprintf("%s of disk %d", what, d.num)
		if v != nil {
			desc += " (" + v.display() + ")"
		}
		desc += ", " + HumanBytes(we.length)
	}
	ep := &Endpoint{Name: name, Desc: desc, Path: d.path, Size: we.length, Sector: d.sector,
		WholeDisk: whole, Model: d.model, Style: d.style, sys: we}
	if role == RoleTarget {
		for _, v := range si.volumesIn(d.num, we.off, we.length) {
			ep.Erases = append(ep.Erases, v.display())
		}
	}
	return ep, true, nil
}

func fileExtents(si *sysInfo, path string) []extent {
	g := volumeGUIDForPath(path)
	for _, v := range si.vols {
		if strings.EqualFold(v.guidPath, g) {
			return v.extents
		}
	}
	return nil
}

func overlaps(exts []extent, we *winEP) bool {
	for _, e := range exts {
		if e.disk == we.disk.num && e.off < we.off+we.length && we.off < e.off+e.length {
			return true
		}
	}
	return false
}

func platformCheckPair(src, dst *Endpoint, pc *PairCheck) error {
	si, err := getSys()
	if err != nil {
		return err
	}
	sw, _ := src.sys.(*winEP)
	dw, _ := dst.sys.(*winEP)
	if dw != nil {
		if err := checkWritable(si, dst, dw, pc); err != nil {
			return err
		}
	}
	if sw != nil && dw != nil && sw.disk.num == dw.disk.num && sw.off < dw.off+dw.length && dw.off < sw.off+sw.length {
		return errors.New("the source and the target overlap")
	}
	if src.IsFile && dw != nil && overlaps(fileExtents(si, src.Path), dw) {
		return errors.New("the image file is stored on the target itself - move it to another drive first")
	}
	if dst.IsFile {
		dir := filepath.Dir(dst.Path)
		if sw != nil && overlaps(fileExtents(si, dir), sw) {
			return errors.New("the image can't be saved on the disk you're backing up - pick a folder on another drive")
		}
		var free uint64
		if r, _, _ := procGetDiskFreeSpaceExW.Call(ptr(dir), uintptr(unsafe.Pointer(&free)), 0, 0); r != 0 {
			avail := int64(free)
			if dst.Exists {
				avail += dst.Size
			}
			need := src.Size
			if dst.Format == FormatGzip {
				need = 0 // can't know in advance; we just warn below
				if avail < src.Size/4 {
					pc.Warnings = append(pc.Warnings, fmt.Sprintf("Only %s free in %s. A compressed image is usually a lot smaller than the disk, but this may not be enough.", HumanBytes(avail), dir))
				}
			}
			if avail < need {
				return fmt.Errorf("not enough room in %s: the image needs %s, only %s is free", dir, HumanBytes(need), HumanBytes(avail))
			}
		}
		if root := volumeRootForPath(dir); root != "" {
			fs := make([]uint16, 64)
			if r, _, _ := procGetVolumeInformationW.Call(ptr(root), 0, 0, 0, 0, 0,
				uintptr(unsafe.Pointer(&fs[0])), uintptr(len(fs))); r != 0 {
				name := strings.ToUpper(syscall.UTF16ToString(fs))
				if (name == "FAT32" || name == "FAT") && src.Size > 4<<30-1 {
					msg := fmt.Sprintf("%s is formatted %s, which can't hold files over 4 GB", root, name)
					if dst.Format != FormatGzip {
						return errors.New(msg + " - save the image on an NTFS or exFAT drive")
					}
					pc.Risks = append(pc.Risks, msg+". If the compressed image ends up bigger than that, the backup will fail.")
				}
			}
		}
	}
	if sw != nil && si.sysDisks[sw.disk.num] {
		pc.Warnings = append(pc.Warnings, "Windows is running from the source disk, so files keep changing while we copy. "+
			"The copy can come out inconsistent - for a reliable one, start the PC from a WinPE USB stick and run DragonClone from there.")
	}
	return nil
}

func checkWritable(si *sysInfo, ep *Endpoint, w *winEP, pc *PairCheck) error {
	if !si.sysDisks[w.disk.num] {
		return nil
	}
	if ep.WholeDisk {
		return fmt.Errorf("disk %d is the one Windows runs or boots from - it can't be overwritten while Windows is running", w.disk.num)
	}
	v := w.vol
	if v == nil && w.part != nil {
		v = w.part.vol
	}
	switch {
	case v != nil && v == si.winVol:
		return errors.New("that partition holds the running Windows - it can't be overwritten")
	case v != nil && v == si.bootVol:
		return errors.New("that's the partition this PC boots from - not touching it")
	case w.part != nil && w.part.isEFI():
		return errors.New("that's the EFI boot partition of the Windows disk - not touching it")
	}
	pc.Risks = append(pc.Risks, "The target partition is on the disk Windows runs from.")
	return nil
}

// CheckWipeTarget runs the same safety checks as a copy target.
func CheckWipeTarget(ep *Endpoint) (*PairCheck, error) {
	pc := &PairCheck{}
	if ep.IsFile {
		return nil, errors.New("pick a disk or partition to wipe")
	}
	si, err := getSys()
	if err != nil {
		return nil, err
	}
	return pc, checkWritable(si, ep, ep.sys.(*winEP), pc)
}

// ---- opening ---------------------------------------------------------------

type volLock struct {
	h    syscall.Handle
	name string
}

func (l *volLock) release() {
	ioctl(l.h, fsctlUnlockVolume, nil, nil)
	syscall.CloseHandle(l.h)
}

func releaseAll(ls []*volLock) {
	for _, l := range ls {
		l.release()
	}
}

// lockVolume locks a volume so nobody else touches it while we work, and for
// targets also dismounts it so Windows lets us write its sectors.
func lockVolume(v *volInfo, dismount, force bool) (*volLock, error) {
	path := strings.TrimSuffix(v.guidPath, `\`)
	h, err := openHandle(path, genericRead|genericWrite, 0)
	if err != nil && !dismount {
		h, err = openHandle(path, genericRead, 0) // write-protected source media
	}
	if err != nil {
		return nil, err
	}
	var lerr error
	for i := 0; i < 10; i++ { // locks sometimes fail for a moment (indexer, AV)
		if _, lerr = ioctl(h, fsctlLockVolume, nil, nil); lerr == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lerr != nil {
		if !(dismount && force) {
			syscall.CloseHandle(h)
			return nil, errors.New("in use")
		}
		// Forced: a dismount cuts off everyone else's open handles.
		if _, err := ioctl(h, fsctlDismountVolume, nil, nil); err != nil {
			syscall.CloseHandle(h)
			return nil, fmt.Errorf("couldn't force it off: %w", err)
		}
		if _, lerr = ioctl(h, fsctlLockVolume, nil, nil); lerr != nil {
			syscall.CloseHandle(h)
			return nil, fmt.Errorf("couldn't lock it after dismounting: %w", lerr)
		}
	}
	if dismount {
		if _, err := ioctl(h, fsctlDismountVolume, nil, nil); err != nil {
			ioctl(h, fsctlUnlockVolume, nil, nil)
			syscall.CloseHandle(h)
			return nil, fmt.Errorf("couldn't dismount: %w", err)
		}
	}
	return &volLock{h: h, name: v.display()}, nil
}

// OpenSource opens a disk, partition or image for reading.
func OpenSource(ep *Endpoint, allowLive bool) (Device, error) {
	if ep.IsFile {
		return OpenImageSource(ep)
	}
	si, err := getSys()
	if err != nil {
		return nil, err
	}
	we := ep.sys.(*winEP)
	var locks []*volLock
	var busy []string
	for _, v := range si.volumesIn(we.disk.num, we.off, we.length) {
		l, err := lockVolume(v, false, false)
		if err != nil {
			busy = append(busy, v.display())
			continue
		}
		locks = append(locks, l)
	}
	if len(busy) > 0 && !allowLive {
		releaseAll(locks)
		return nil, &LiveSourceError{Volumes: busy}
	}
	h, err := openHandle(we.disk.path, genericRead, fileFlagNoBuffering)
	if err != nil {
		releaseAll(locks)
		return nil, fmt.Errorf("can't open %s: %w", we.disk.path, err)
	}
	return &winDisk{h: h, base: we.off, size: we.length, sector: we.disk.sector, locks: locks}, nil
}

// OpenTarget opens a disk or partition for writing (locking and dismounting
// whatever is on it), or creates a new image file.
func OpenTarget(ep *Endpoint, opts *Options, meta *ImageMeta) (Device, error) {
	if ep.IsFile {
		return CreateImageTarget(ep, opts, meta)
	}
	si, err := getSys()
	if err != nil {
		return nil, err
	}
	we := ep.sys.(*winEP)
	var locks []*volLock
	var busy []string
	for _, v := range si.volumesIn(we.disk.num, we.off, we.length) {
		l, err := lockVolume(v, true, opts.ForceDismount)
		if err != nil {
			busy = append(busy, v.display()+" ("+err.Error()+")")
			continue
		}
		locks = append(locks, l)
	}
	if len(busy) > 0 {
		releaseAll(locks)
		return nil, &InUseError{Volumes: busy}
	}
	h, err := openHandle(we.disk.path, genericRead|genericWrite, fileFlagNoBuffering|fileFlagWriteThrough)
	if err != nil {
		releaseAll(locks)
		return nil, fmt.Errorf("can't open %s for writing: %w", we.disk.path, err)
	}
	return &winDisk{h: h, base: we.off, size: we.length, sector: we.disk.sector, locks: locks,
		writable: true, refresh: ep.WholeDisk}, nil
}

// winDisk is the byte range [base, base+size) of a physical disk.
type winDisk struct {
	h        syscall.Handle
	base     int64
	size     int64
	sector   int
	locks    []*volLock
	writable bool
	refresh  bool
	closed   bool
}

func (d *winDisk) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > d.size {
		return 0, errors.New("read outside the device")
	}
	var done uint32
	pos := uint64(d.base + off)
	ov := syscall.Overlapped{Offset: uint32(pos), OffsetHigh: uint32(pos >> 32)}
	err := syscall.ReadFile(d.h, p, &done, &ov)
	if err == errHandleEOF {
		err = io.EOF
	}
	return int(done), err
}

func (d *winDisk) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > d.size {
		return 0, errors.New("write outside the device")
	}
	var done uint32
	pos := uint64(d.base + off)
	ov := syscall.Overlapped{Offset: uint32(pos), OffsetHigh: uint32(pos >> 32)}
	err := syscall.WriteFile(d.h, p, &done, &ov)
	return int(done), err
}

func (d *winDisk) Size() int64              { return d.size }
func (d *winDisk) SectorSize() int          { return d.sector }
func (d *winDisk) Growable() bool           { return false }
func (d *winDisk) PartitionTableLast() bool { return d.writable }

func (d *winDisk) Sync() error {
	if !d.writable {
		return nil
	}
	return syscall.FlushFileBuffers(d.h)
}

func (d *winDisk) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	var err error
	if d.writable {
		err = syscall.FlushFileBuffers(d.h)
		if d.refresh {
			ioctl(d.h, ioctlDiskUpdateProperties, nil, nil) // have Windows re-read the partition table
		}
	}
	syscall.CloseHandle(d.h)
	releaseAll(d.locks)
	return err
}

// openUnbuffered opens an image bypassing the file cache, so verify really
// reads what landed on the disk.
func openUnbuffered(path string, size int64) (Device, error) {
	h, err := openHandle(path, genericRead, fileFlagNoBuffering)
	if err != nil {
		return nil, err
	}
	return &unbufferedFile{
		readOnce: func(p []byte, off int64) (int, error) {
			var done uint32
			ov := syscall.Overlapped{Offset: uint32(off), OffsetHigh: uint32(uint64(off) >> 32)}
			err := syscall.ReadFile(h, p, &done, &ov)
			if err == errHandleEOF || (err == nil && done == 0) {
				err = io.EOF
			}
			return int(done), err
		},
		closer: func() error { return syscall.CloseHandle(h) },
		size:   size,
	}, nil
}

// ---- errors ----------------------------------------------------------------

func isFatalIOError(err error) bool {
	var en syscall.Errno
	if errors.As(err, &en) {
		switch en {
		case errNotReady, errInvalidHandle, errDevNotExist, errInvalidParameter, errNoSuchDevice,
			errMediaChanged, errNoMedia, errDeviceNotConnect, syscall.ERROR_ACCESS_DENIED,
			syscall.ERROR_FILE_NOT_FOUND, errPathNotFound, errWriteProtect, errDiskFull:
			return true
		}
	}
	return false
}

// ErrorHint adds a plain-English tip for common Windows errors.
func ErrorHint(err error) string {
	var en syscall.Errno
	if !errors.As(err, &en) {
		return ""
	}
	switch en {
	case syscall.ERROR_ACCESS_DENIED:
		return "Windows blocked raw disk access. Close any program or Explorer window using the target. If Windows Security's \"Controlled folder access\" is on, allow DragonClone there."
	case errWriteProtect:
		return "The target is write-protected - check the lock switch on SD cards and USB sticks."
	case errDiskFull:
		return "The drive is full."
	case errFileTooLarge:
		return "That drive can't hold such a big file (FAT32 stops at 4 GB). Use an NTFS or exFAT drive."
	case errDeviceNotConnect, errNoSuchDevice, errNotReady:
		return "The disk went away - check the cable or USB connection."
	case errCRC, errSectorNotFound, errIODevice, errReadFault:
		return "The source has unreadable sectors. Turn on \"Skip unreadable sectors\" to fill them with zeros and keep going."
	}
	return ""
}

// ---- odds and ends used by the UI -----------------------------------------

// Roots lists the drive letters for the file browser.
func Roots() []Root {
	mask, _, _ := procGetLogicalDrives.Call()
	var out []Root
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		t, _, _ := procGetDriveTypeW.Call(ptr(root))
		if t == 5 || t == 1 { // CD-ROM or "no root dir"
			continue
		}
		r := Root{Path: root, Free: -1, Total: -1}
		label := make([]uint16, 261)
		if ok, _, _ := procGetVolumeInformationW.Call(ptr(root), uintptr(unsafe.Pointer(&label[0])), 261, 0, 0, 0, 0, 0); ok != 0 {
			r.Label = syscall.UTF16ToString(label)
		}
		var free, total uint64
		if ok, _, _ := procGetDiskFreeSpaceExW.Call(ptr(root), 0, uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free))); ok != 0 {
			r.Free, r.Total = int64(free), int64(total)
		}
		out = append(out, r)
	}
	return out
}

func powershell(script string, env ...string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// MountImage attaches a .vhd or .iso read-only and returns its drive letters.
// The path goes in through an environment variable so it never gets parsed
// as PowerShell code.
func MountImage(path string) (string, error) {
	return powershell(`$i = Mount-DiskImage -ImagePath $env:DC_IMAGE -Access ReadOnly -PassThru; `+
		`Start-Sleep -Milliseconds 800; `+
		`$l = ($i | Get-Disk -ErrorAction SilentlyContinue | Get-Partition -ErrorAction SilentlyContinue | Where-Object DriveLetter | ForEach-Object { "$($_.DriveLetter):" }); `+
		`if (-not $l) { $l = ($i | Get-Volume -ErrorAction SilentlyContinue | Where-Object DriveLetter | ForEach-Object { "$($_.DriveLetter):" }) }; `+
		`$l -join ' '`, "DC_IMAGE="+path)
}

// UnmountImage detaches an image mounted with MountImage.
func UnmountImage(path string) error {
	_, err := powershell(`Dismount-DiskImage -ImagePath $env:DC_IMAGE | Out-Null`, "DC_IMAGE="+path)
	return err
}

// ScheduleShutdown turns the PC off in a minute (can be cancelled).
func ScheduleShutdown() error {
	cmd := exec.Command("shutdown.exe", "/s", "/t", "60", "/c", "DragonClone finished. Shutting down in one minute.")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd.Run()
}

// CancelShutdown aborts a scheduled shutdown.
func CancelShutdown() error {
	cmd := exec.Command("shutdown.exe", "/a")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd.Run()
}

// ShowInFolder opens Explorer with the file selected.
func ShowInFolder(path string) error {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	return cmd.Start()
}
