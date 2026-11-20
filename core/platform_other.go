//go:build !windows && !linux

package core

// Other systems: image files only. Enough for the code to build and for the
// image tools to work; raw disks need Windows or Linux.

import (
	"errors"
	"io"
	"os"
	"strings"
)

func IsAdmin() bool              { return os.Geteuid() == 0 }
func Refresh() error             { return nil }
func ErrorHint(error) string     { return "" }
func Disks() ([]DiskInfo, error) { return nil, nil }
func Roots() []Root {
	h, _ := os.UserHomeDir()
	return []Root{{Path: "/", Label: "Root", Free: -1, Total: -1}, {Path: h, Label: "Home", Free: -1, Total: -1}}
}

func resolveDevice(s string, role Role) (*Endpoint, bool, error) {
	if strings.HasPrefix(s, "/dev/") {
		return nil, true, errors.New("raw disks are only supported on Windows and Linux")
	}
	return nil, false, nil
}

func platformCheckPair(src, dst *Endpoint, pc *PairCheck) error { return nil }
func CheckWipeTarget(ep *Endpoint) (*PairCheck, error) {
	return nil, errors.New("raw disks are only supported on Windows and Linux")
}
func OpenSource(ep *Endpoint, allowLive bool) (Device, error) { return OpenImageSource(ep) }
func OpenTarget(ep *Endpoint, opts *Options, meta *ImageMeta) (Device, error) {
	return CreateImageTarget(ep, opts, meta)
}
func openUnbuffered(string, int64) (Device, error) { return nil, errors.New("unsupported") }
func isFatalIOError(error) bool                    { return false }
func MountImage(string) (string, error)            { return "", errors.New("only on Windows") }
func UnmountImage(string) error                    { return errors.New("only on Windows") }
func ScheduleShutdown() error                      { return errors.New("only on Windows") }
func CancelShutdown() error                        { return nil }
func ShowInFolder(string) error                    { return nil }

var _ = io.EOF
