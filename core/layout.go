package core

// Byte-level parsers for the structs Windows hands back from disk IOCTLs.
// They don't touch any Windows API, so the tests run anywhere. The offsets
// are double-checked against the real SDK headers in tools/layout_check.c.

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
)

const (
	sizeofDriveLayoutHeader = 48  // DRIVE_LAYOUT_INFORMATION_EX up to PartitionEntry
	sizeofPartitionInfoEx   = 144 // PARTITION_INFORMATION_EX
	sizeofDiskExtent        = 24  // DISK_EXTENT
)

type rawPart struct {
	num      int
	off      int64
	length   int64
	typeName string
	typeGUID string // GPT only
	mbrType  byte   // MBR only
	name     string // GPT partition name
	vol      *volInfo
}

type extent struct {
	disk   int
	off    int64
	length int64
}

var le = binary.LittleEndian

// parseDriveLayout parses DRIVE_LAYOUT_INFORMATION_EX.
func parseDriveLayout(b []byte) (style string, parts []rawPart, err error) {
	if len(b) < sizeofDriveLayoutHeader {
		return "", nil, fmt.Errorf("layout too short (%d bytes)", len(b))
	}
	switch le.Uint32(b[0:4]) {
	case 0:
		style = "MBR"
	case 1:
		style = "GPT"
	default:
		style = "RAW"
	}
	count := int(le.Uint32(b[4:8]))
	for i := 0; i < count; i++ {
		start := sizeofDriveLayoutHeader + i*sizeofPartitionInfoEx
		if start+sizeofPartitionInfoEx > len(b) {
			break
		}
		e := b[start : start+sizeofPartitionInfoEx]
		p := rawPart{
			off:    int64(le.Uint64(e[8:16])),
			length: int64(le.Uint64(e[16:24])),
			num:    int(le.Uint32(e[24:28])),
		}
		if p.length == 0 || p.num == 0 {
			continue // unused MBR slot or extended-partition container
		}
		switch le.Uint32(e[0:4]) {
		case 0: // MBR
			p.mbrType = e[32]
			p.typeName = mbrTypeName(p.mbrType)
		case 1: // GPT
			p.typeGUID = guidString(e[32:48])
			p.typeName = gptTypeName(p.typeGUID)
			p.name = utf16String(e[72:144])
		}
		parts = append(parts, p)
	}
	return style, parts, nil
}

// parseDiskExtents parses VOLUME_DISK_EXTENTS.
func parseDiskExtents(b []byte) []extent {
	if len(b) < 8 {
		return nil
	}
	n := int(le.Uint32(b[0:4]))
	var out []extent
	for i := 0; i < n; i++ {
		s := 8 + i*sizeofDiskExtent
		if s+sizeofDiskExtent > len(b) {
			break
		}
		out = append(out, extent{
			disk:   int(le.Uint32(b[s : s+4])),
			off:    int64(le.Uint64(b[s+8 : s+16])),
			length: int64(le.Uint64(b[s+16 : s+24])),
		})
	}
	return out
}

// parseStorageDescriptor parses STORAGE_DEVICE_DESCRIPTOR.
func parseStorageDescriptor(b []byte) (model, serial, bus string, removable bool) {
	if len(b) < 36 {
		return
	}
	removable = b[10] != 0
	bus = busTypeName(le.Uint32(b[28:32]))
	vendor := cStringAt(b, le.Uint32(b[12:16]))
	product := cStringAt(b, le.Uint32(b[16:20]))
	model = strings.Join(strings.Fields(vendor+" "+product), " ")
	serial = strings.Join(strings.Fields(cStringAt(b, le.Uint32(b[24:28]))), " ")
	return
}

// parseGeometrySector returns BytesPerSector from DISK_GEOMETRY(_EX).
func parseGeometrySector(b []byte) int {
	if len(b) < 24 {
		return 0
	}
	bps := int(le.Uint32(b[20:24]))
	if bps < 512 || bps > 65536 || bps&(bps-1) != 0 {
		return 0
	}
	return bps
}

func cStringAt(b []byte, off uint32) string {
	if off == 0 || int(off) >= len(b) {
		return ""
	}
	s := b[off:]
	for i, c := range s {
		if c == 0 {
			s = s[:i]
			break
		}
	}
	return strings.TrimSpace(string(s))
}

func utf16String(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := le.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

func guidString(b []byte) string {
	return fmt.Sprintf("%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		le.Uint32(b[0:4]), le.Uint16(b[4:6]), le.Uint16(b[6:8]),
		b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

const (
	guidEFI      = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	guidMSR      = "E3C9E316-0B5C-4DB8-817D-F92DF00215AE"
	guidBasic    = "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7"
	guidRecovery = "DE94BBA4-06D1-4D40-A16A-BFD50179D6AC"
)

func gptTypeName(g string) string {
	switch g {
	case guidEFI:
		return "EFI System"
	case guidMSR:
		return "MS Reserved"
	case guidBasic:
		return "Basic data"
	case guidRecovery:
		return "Recovery"
	case "5808C8AA-7E8F-42E0-85D2-E1E90434CFB3":
		return "LDM metadata"
	case "AF9B60A0-1431-4F62-BC68-3311714A69AD":
		return "LDM data"
	case "E75CAF8F-F680-4CEE-AFA3-B001E56EFC2D":
		return "Storage Spaces"
	case "0FC63DAF-8483-4772-8E79-3D69D8477DE4":
		return "Linux data"
	case "0657FD6D-A4AB-43C4-84E5-0933C84B4F4F":
		return "Linux swap"
	case "E6D6D379-F507-44C2-A23C-238F2A3DF928":
		return "Linux LVM"
	case "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709":
		return "Linux root"
	case "7C3457EF-0000-11AA-AA11-00306543ECAC":
		return "Apple APFS"
	case "48465300-0000-11AA-AA11-00306543ECAC":
		return "Apple HFS+"
	}
	return "GPT " + g[:8]
}

func mbrTypeName(t byte) string {
	switch t {
	case 0x01, 0x04, 0x06, 0x0E:
		return "FAT16/12"
	case 0x07:
		return "NTFS/exFAT"
	case 0x0B, 0x0C:
		return "FAT32"
	case 0x0F, 0x05, 0x85:
		return "Extended"
	case 0x27:
		return "Recovery"
	case 0x82:
		return "Linux swap"
	case 0x83:
		return "Linux"
	case 0x8E:
		return "Linux LVM"
	case 0xEE:
		return "GPT protective"
	case 0xEF:
		return "EFI System"
	}
	return fmt.Sprintf("type 0x%02X", t)
}

func busTypeName(b uint32) string {
	names := map[uint32]string{1: "SCSI", 2: "ATAPI", 3: "ATA", 4: "1394", 6: "Fibre", 7: "USB",
		8: "RAID", 9: "iSCSI", 10: "SAS", 11: "SATA", 12: "SD", 13: "MMC", 14: "Virtual",
		15: "VHD", 16: "Spaces", 17: "NVMe", 18: "SCM", 19: "UFS"}
	if n, ok := names[b]; ok {
		return n
	}
	return "?"
}

func (p *rawPart) isEFI() bool {
	return p.typeGUID == guidEFI || p.mbrType == 0xEF
}

type volInfo struct {
	guidPath string   // \\?\Volume{...}\  (with trailing backslash)
	mounts   []string // mount points such as E:\
	label    string
	fs       string
	total    int64 // -1 if unknown
	free     int64
	extents  []extent
}

// display gives a short name for messages, e.g. `E: NTFS "Backup"`.
func (v *volInfo) display() string {
	var parts []string
	if len(v.mounts) > 0 {
		var ms []string
		for _, m := range v.mounts {
			ms = append(ms, strings.TrimSuffix(m, `\`))
		}
		parts = append(parts, strings.Join(ms, " "))
	} else {
		parts = append(parts, "(no drive letter)")
	}
	if v.fs != "" {
		parts = append(parts, v.fs)
	}
	if v.label != "" {
		parts = append(parts, `"`+v.label+`"`)
	}
	return strings.Join(parts, " ")
}
