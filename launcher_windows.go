//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"dragonclone/core"
)

var (
	user32  = syscall.NewLazyDLL("user32.dll")
	shell32 = syscall.NewLazyDLL("shell32.dll")

	procMessageBoxW     = user32.NewProc("MessageBoxW")
	procShellExecuteExW = shell32.NewProc("ShellExecuteExW")
	procShellExecuteW   = shell32.NewProc("ShellExecuteW")
)

const (
	mbOK           = 0x0
	mbIconError    = 0x10
	mbIconInfo     = 0x40
	seeNoCloseProc = 0x40
	errCancelled   = syscall.Errno(1223)
)

// shellExecuteInfo mirrors SHELLEXECUTEINFOW (112 bytes on x64; checked in
// tools/layout_check.c).
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    uintptr
	dwHotKey     uint32
	hIcon        uintptr
	hProcess     syscall.Handle
}

func u16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func messageBox(text string, icon uint32) {
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16(text))), uintptr(unsafe.Pointer(u16("DragonClone"))), uintptr(mbOK|icon))
}

// launch is what runs when the user double-clicks DragonClone.exe.
func launch() {
	dir := core.DataDir()
	os.MkdirAll(dir, 0o755)

	// Already running (maybe with a job in progress)? Just reopen the window.
	if url := runningBackend(); url != "" {
		openWindow(url)
		return
	}

	exe, err := os.Executable()
	if err != nil {
		messageBox("Couldn't find DragonClone.exe: "+err.Error(), mbIconError)
		return
	}
	proc, err := startElevated(exe, "--backend --data "+syscall.EscapeArg(dir))
	if err != nil {
		if errors.Is(err, errCancelled) {
			messageBox("DragonClone needs administrator rights to read and write disks directly.\n\n"+
				"Start it again and choose Yes when Windows asks.", mbIconInfo)
		} else {
			messageBox("Couldn't start DragonClone: "+err.Error(), mbIconError)
		}
		return
	}
	defer syscall.CloseHandle(proc)

	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if url := runningBackend(); url != "" {
			openWindow(url)
			return
		}
		if ev, _ := syscall.WaitForSingleObject(proc, 0); ev == syscall.WAIT_OBJECT_0 {
			messageBox("DragonClone's engine stopped while starting.\n\nThe log files are in "+filepath.Join(dir, "logs"), mbIconError)
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	messageBox("DragonClone's engine didn't start in time.\n\nThe log files are in "+filepath.Join(dir, "logs"), mbIconError)
}

func startElevated(exe, params string) (syscall.Handle, error) {
	info := shellExecuteInfo{
		fMask:        seeNoCloseProc,
		lpVerb:       u16("runas"),
		lpFile:       u16(exe),
		lpParameters: u16(params),
		lpDirectory:  u16(filepath.Dir(exe)),
		nShow:        0, // SW_HIDE - the backend has no window anyway
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	r, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		if callErr != nil && callErr != syscall.Errno(0) {
			return 0, callErr
		}
		return 0, errors.New("ShellExecuteEx failed")
	}
	return info.hProcess, nil
}

// runningBackend returns the window URL if a backend from backend.json answers.
func runningBackend() string {
	b, err := os.ReadFile(handoffPath())
	if err != nil {
		return ""
	}
	var h handoff
	if json.Unmarshal(b, &h) != nil || h.Port == 0 || h.Token == "" {
		return ""
	}
	host := fmt.Sprintf("127.0.0.1:%d", h.Port)
	req, _ := http.NewRequest("GET", "http://"+host+"/api/info", nil)
	req.Header.Set("X-DragonClone-Token", h.Token)
	resp, err := (&http.Client{Timeout: 1500 * time.Millisecond}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var info struct {
		Version string `json:"version"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&info) != nil || info.Version == "" {
		return ""
	}
	return fmt.Sprintf("http://%s/?t=%s", host, h.Token)
}

// openWindow shows the UI in an Edge (or Chrome) app window - no tabs, no
// address bar - with its own profile so it never mixes with normal browsing.
func openWindow(url string) {
	profile := filepath.Join(core.DataDir(), "Window")
	args := []string{
		"--app=" + url,
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--window-size=1280,860",
	}
	for _, b := range browserCandidates() {
		if _, err := os.Stat(b); err != nil {
			continue
		}
		if exec.Command(b, args...).Start() == nil {
			return
		}
	}
	// No Edge or Chrome? Fall back to whatever the default browser is.
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(u16("open"))), uintptr(unsafe.Pointer(u16(url))), 0, 0, 1)
}

func browserCandidates() []string {
	var out []string
	for _, exe := range []string{"msedge.exe", "chrome.exe"} {
		for _, root := range []syscall.Handle{syscall.HKEY_LOCAL_MACHINE, syscall.HKEY_CURRENT_USER} {
			if p := appPath(root, exe); p != "" {
				out = append(out, p)
			}
		}
	}
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		out = append(out,
			filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(base, `Google\Chrome\Application\chrome.exe`))
	}
	return out
}

func appPath(root syscall.Handle, exe string) string {
	var k syscall.Handle
	key := `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\` + exe
	if syscall.RegOpenKeyEx(root, u16(key), 0, syscall.KEY_READ, &k) != nil {
		return ""
	}
	defer syscall.RegCloseKey(k)
	buf := make([]uint16, 1024)
	n := uint32(len(buf) * 2)
	var typ uint32
	if syscall.RegQueryValueEx(k, nil, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n) != nil {
		return ""
	}
	return syscall.UTF16ToString(buf)
}
