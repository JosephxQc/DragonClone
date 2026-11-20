package core

import (
	"os"
	"path/filepath"
)

// DiskInfo is one physical disk as the UI shows it.
type DiskInfo struct {
	ID         string     `json:"id"` // disk1 on Windows, /dev/sda on Linux
	Number     int        `json:"number"`
	Model      string     `json:"model"`
	Serial     string     `json:"serial,omitempty"`
	Bus        string     `json:"bus"`
	Size       int64      `json:"size"`
	SectorSize int        `json:"sectorSize"`
	Style      string     `json:"style"` // GPT / MBR / RAW
	Removable  bool       `json:"removable"`
	System     bool       `json:"system"` // Windows runs or boots from it
	NoMedia    bool       `json:"noMedia"`
	Partitions []PartInfo `json:"partitions"`
}

// PartInfo is one partition on a disk.
type PartInfo struct {
	ID     string `json:"id"` // disk1p2
	Number int    `json:"number"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
	Letter string `json:"letter,omitempty"`
	Label  string `json:"label,omitempty"`
	FS     string `json:"fs,omitempty"`
	Used   int64  `json:"used"` // -1 if we can't tell
	System bool   `json:"system"`
}

// Root is a place the file browser can start from (a drive on Windows).
type Root struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Free  int64  `json:"free"`
	Total int64  `json:"total"`
}

var dataDir string

// SetDataDir overrides where logs and history go.
func SetDataDir(dir string) { dataDir = dir }

// DataDir is %LOCALAPPDATA%\DragonClone on Windows, ~/.local/share/dragonclone elsewhere.
func DataDir() string {
	if dataDir != "" {
		return dataDir
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "DragonClone")
	}
	if d, err := os.UserHomeDir(); err == nil {
		return filepath.Join(d, ".local", "share", "dragonclone")
	}
	return filepath.Join(os.TempDir(), "dragonclone")
}
