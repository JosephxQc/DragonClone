package core

import (
	"os"
	"testing"
)

// testdata/*.bin are real Windows SDK structs dumped by tools/gen_testdata.sh.

func readTD(t *testing.T, name string) []byte {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseGPTLayout(t *testing.T) {
	style, parts, err := parseDriveLayout(readTD(t, "gpt.bin"))
	if err != nil || style != "GPT" || len(parts) != 2 {
		t.Fatalf("style=%s parts=%+v err=%v", style, parts, err)
	}
	p := parts[0]
	if p.num != 1 || p.off != 1048576 || p.length != 104857600 || p.typeName != "EFI System" || !p.isEFI() || p.name != "EFI system partition" {
		t.Fatalf("%+v", p)
	}
	p = parts[1]
	if p.num != 3 || p.off != 122683392 || p.length != 0x123456789A00 || p.typeName != "Basic data" || p.name != "Basic data partition" {
		t.Fatalf("%+v", p)
	}
}

func TestParseMBRLayout(t *testing.T) {
	style, parts, err := parseDriveLayout(readTD(t, "mbr.bin"))
	if err != nil || style != "MBR" || len(parts) != 1 {
		t.Fatalf("style=%s parts=%+v err=%v", style, parts, err)
	}
	if p := parts[0]; p.num != 1 || p.off != 4194304 || p.length != 61991813120 || p.typeName != "FAT32" || p.isEFI() {
		t.Fatalf("%+v", p)
	}
}

func TestParseExtents(t *testing.T) {
	e := parseDiskExtents(readTD(t, "ext.bin"))
	if len(e) != 2 || e[0] != (extent{2, 1048576, 5000000000}) || e[1] != (extent{7, 0x100000000, 4096}) {
		t.Fatalf("%+v", e)
	}
}

func TestParseStorageDescriptor(t *testing.T) {
	model, _, bus, removable := parseStorageDescriptor(readTD(t, "sdd.bin"))
	if model != "SanDisk Ultra" || bus != "USB" || !removable {
		t.Fatalf("model=%q bus=%q removable=%v", model, bus, removable)
	}
}

func TestParseGeometry(t *testing.T) {
	if s := parseGeometrySector(readTD(t, "geo.bin")); s != 4096 {
		t.Fatalf("sector=%d", s)
	}
}
