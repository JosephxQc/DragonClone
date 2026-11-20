package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Role says whether we read from an endpoint or write to it.
type Role int

const (
	RoleSource Role = iota
	RoleTarget
)

// Kind narrows down what a name is allowed to resolve to.
type Kind int

const (
	KindAny    Kind = iota
	KindDevice      // disks and partitions only
	KindFile        // image files only
)

// Endpoint is a source or target after we've figured out what it is.
type Endpoint struct {
	Name      string      `json:"name"` // what the user picks/types: disk1, disk1p2, E:, or a path
	Desc      string      `json:"desc"`
	IsFile    bool        `json:"isFile"`
	WholeDisk bool        `json:"wholeDisk"`
	Path      string      `json:"path"`
	Size      int64       `json:"size"` // for images: the size of the disk inside
	Sector    int         `json:"sector"`
	Exists    bool        `json:"exists"`
	Format    ImageFormat `json:"format,omitempty"`
	Erases    []string    `json:"erases,omitempty"` // volumes that get wiped when this is a target
	Model     string      `json:"model,omitempty"`
	Style     string      `json:"style,omitempty"`
	Meta      *ImageMeta  `json:"meta,omitempty"`
	sys       any         // platform specific bits
}

// Options for a copy.
type Options struct {
	Verify        bool
	SkipBad       bool
	Retries       int
	BlockSize     int
	AllowLive     bool // go ahead even if the source volumes are busy
	ForceDismount bool // kick other programs off the target volumes
	Level         int  // gzip level for compressed images (1 = fast)
}

func DefaultOptions() *Options {
	return &Options{Retries: 1, BlockSize: 4 << 20, Level: 1}
}

// LiveSourceError: volumes on the source are in use and might change mid-copy.
type LiveSourceError struct{ Volumes []string }

func (e *LiveSourceError) Error() string {
	return "these source volumes are in use by other programs: " + strings.Join(e.Volumes, ", ")
}

// InUseError: we couldn't lock the target volumes.
type InUseError struct{ Volumes []string }

func (e *InUseError) Error() string {
	return "couldn't lock these target volumes (close programs or Explorer windows using them): " + strings.Join(e.Volumes, "; ")
}

var reBareLetter = regexp.MustCompile(`^[A-Za-z]:$`)

// CleanName trims spaces and the quotes Explorer adds with "Copy as path".
func CleanName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// Resolve figures out what a name refers to.
func Resolve(name string, role Role, kind Kind) (*Endpoint, error) {
	s := CleanName(name)
	if s == "" {
		return nil, errors.New("nothing selected")
	}
	if kind != KindFile {
		ep, handled, err := resolveDevice(s, role)
		if handled {
			return ep, err
		}
		if kind == KindDevice {
			return nil, fmt.Errorf("%q isn't a disk or partition", s)
		}
	}
	return resolveFile(s, role)
}

func resolveFile(s string, role Role) (*Endpoint, error) {
	if reBareLetter.MatchString(s) {
		return nil, fmt.Errorf("%s is a drive - pick a file name on it", s)
	}
	abs, err := filepath.Abs(s)
	if err != nil {
		return nil, err
	}
	st, statErr := os.Stat(abs)
	if role == RoleSource {
		if statErr != nil {
			return nil, fmt.Errorf("can't open %s: %v", abs, statErr)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s isn't a file", abs)
		}
		return inspectImage(abs, st.Size())
	}

	// Writing a new image.
	if statErr == nil && st.IsDir() {
		return nil, fmt.Errorf("%s is a folder - add a file name", abs)
	}
	ep := &Endpoint{Name: abs, IsFile: true, Path: abs, Sector: 512, Format: FormatFromName(abs)}
	if statErr == nil {
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s exists and isn't a regular file", abs)
		}
		ep.Exists, ep.Size = true, st.Size()
		ep.Desc = fmt.Sprintf("%s (exists, will be replaced)", abs)
	} else {
		dir := filepath.Dir(abs)
		if dst, err := os.Stat(dir); err != nil || !dst.IsDir() {
			return nil, fmt.Errorf("folder %s doesn't exist", dir)
		}
		ep.Desc = abs
	}
	return ep, nil
}

// PairCheck is what the user should hear about before a copy starts.
type PairCheck struct {
	Warnings []string `json:"warnings"` // good to know
	Risks    []string `json:"risks"`    // needs an explicit "yes, I'm sure"
}

// CheckPair validates a source/target combination before anything is opened.
func CheckPair(src, dst *Endpoint) (*PairCheck, error) {
	pc := &PairCheck{}
	if src.Size <= 0 {
		return nil, fmt.Errorf("%s is empty or has no media", src.Name)
	}
	if src.IsFile && dst.IsFile && sameFile(src.Path, dst.Path) {
		return nil, errors.New("source and target are the same file")
	}
	if dst.IsFile {
		if dst.Format == FormatVHD {
			if src.Size > maxVHDSize {
				return nil, fmt.Errorf("VHD files can't be bigger than 2040 GB - pick the compressed or raw format")
			}
			if src.Sector != 512 {
				return nil, fmt.Errorf("this disk uses %d-byte sectors; VHD files only support 512-byte sectors - pick another format", src.Sector)
			}
		}
	} else {
		need := roundUp64(src.Size, int64(dst.Sector))
		if dst.Size < need {
			return nil, fmt.Errorf("%s is too small: it holds %s but %s is needed", dst.Name, HumanBytes(dst.Size), HumanBytes(need))
		}
		if src.Size%int64(dst.Sector) != 0 {
			pc.Warnings = append(pc.Warnings, fmt.Sprintf(
				"The source isn't a whole number of %d-byte sectors, so the last sector gets padded with zeros.", dst.Sector))
		}
		if extra := dst.Size - src.Size; extra >= 1<<20 {
			pc.Warnings = append(pc.Warnings, fmt.Sprintf(
				"The target is %s bigger than the source. That extra space stays unallocated - you can grow the partition later in Disk Management.", HumanBytes(extra)))
		}
		srcSector := src.Sector
		if src.IsFile && src.Meta != nil && src.Meta.SectorSize > 0 {
			srcSector = src.Meta.SectorSize
		}
		if (!src.IsFile || (src.Meta != nil && src.Meta.SectorSize > 0)) && srcSector != dst.Sector {
			pc.Risks = append(pc.Risks, fmt.Sprintf(
				"The source uses %d-byte sectors but the target uses %d-byte sectors. Partition tables count in sectors, so the copy most likely won't be readable.", srcSector, dst.Sector))
		} else if src.IsFile && src.Meta == nil && dst.Sector != 512 {
			pc.Warnings = append(pc.Warnings, fmt.Sprintf(
				"The target uses %d-byte sectors. If this image came from a normal 512-byte-sector disk, its partitions won't line up.", dst.Sector))
		}
	}
	if err := platformCheckPair(src, dst, pc); err != nil {
		return nil, err
	}
	return pc, nil
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	if err1 == nil && err2 == nil {
		return os.SameFile(sa, sb)
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
