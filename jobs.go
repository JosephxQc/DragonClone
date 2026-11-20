package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"dragonclone/core"
)

// JobRequest is what the UI sends for every operation.
type JobRequest struct {
	Kind          string `json:"kind"` // clone, backup, restore, compare, check, scan, wipe
	Source        string `json:"source"`
	Target        string `json:"target"`
	Verify        bool   `json:"verify"`
	SkipBad       bool   `json:"skipBad"`
	Retries       int    `json:"retries"`
	Level         int    `json:"level"`
	WipeMode      string `json:"wipeMode"`
	AllowLive     bool   `json:"allowLive"`
	ForceDismount bool   `json:"forceDismount"`
	AcceptRisks   bool   `json:"acceptRisks"`
	Confirm       string `json:"confirm"`
	Shutdown      bool   `json:"shutdown"`
}

type Row struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Tone  string `json:"tone,omitempty"` // good / bad / warn
}

// Plan is the "are you sure?" summary shown before anything happens.
type Plan struct {
	Kind        string         `json:"kind"`
	Title       string         `json:"title"`
	Source      *core.Endpoint `json:"source,omitempty"`
	Target      *core.Endpoint `json:"target,omitempty"`
	Rows        []Row          `json:"rows"`
	Warnings    []string       `json:"warnings"`
	Risks       []string       `json:"risks"`
	Erases      []string       `json:"erases"`
	Destructive bool           `json:"destructive"`
	ConfirmName string         `json:"confirmName,omitempty"`
}

var kindTitles = map[string]string{
	"clone": "Clone", "backup": "Back up to image", "restore": "Restore image",
	"compare": "Compare", "check": "Check image", "scan": "Surface scan", "wipe": "Wipe",
}

func makePlan(req *JobRequest) (*Plan, error) {
	if err := core.Refresh(); err != nil {
		return nil, err
	}
	p := &Plan{Kind: req.Kind, Title: kindTitles[req.Kind]}
	if p.Title == "" {
		return nil, fmt.Errorf("unknown operation %q", req.Kind)
	}
	var srcKind, dstKind core.Kind
	needTarget := true
	switch req.Kind {
	case "clone":
		srcKind, dstKind = core.KindDevice, core.KindDevice
	case "backup":
		srcKind, dstKind = core.KindDevice, core.KindFile
	case "restore":
		srcKind, dstKind = core.KindFile, core.KindDevice
	case "compare":
		srcKind, dstKind = core.KindAny, core.KindAny
	case "check":
		srcKind, needTarget = core.KindAny, false
	case "scan":
		srcKind, needTarget = core.KindDevice, false
	case "wipe":
		return planWipe(req, p)
	}

	src, err := core.Resolve(req.Source, core.RoleSource, srcKind)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	p.Source = src
	p.Rows = append(p.Rows, Row{Label: "Source", Value: src.Desc})
	if !needTarget {
		p.Rows = append(p.Rows, Row{Label: "Size", Value: fmt.Sprintf("%s (%s bytes)", core.HumanBytes(src.Size), core.CommaInt(src.Size))})
		return p, nil
	}

	role := core.RoleTarget
	if req.Kind == "compare" {
		role = core.RoleSource
	}
	dst, err := core.Resolve(req.Target, role, dstKind)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	p.Target = dst
	if req.Kind == "compare" {
		p.Rows = append(p.Rows, Row{Label: "Compare with", Value: dst.Desc})
		return p, nil
	}

	pc, err := core.CheckPair(src, dst)
	if err != nil {
		return nil, err
	}
	p.Warnings, p.Risks = pc.Warnings, pc.Risks
	p.Rows = append(p.Rows, Row{Label: "Target", Value: dst.Desc})
	if dst.IsFile {
		p.Rows = append(p.Rows, Row{Label: "Format", Value: dst.Format.Label()})
		if dst.Exists {
			p.Risks = append(p.Risks, "The file "+filepath.Base(dst.Path)+" already exists and will be replaced.")
		}
	} else {
		p.Destructive = true
		p.ConfirmName = dst.Name
		p.Erases = dst.Erases
	}
	p.Rows = append(p.Rows, Row{Label: "Data to copy", Value: fmt.Sprintf("%s (every sector)", core.HumanBytes(src.Size))})
	p.Rows = append(p.Rows, Row{Label: "Verify afterwards", Value: yesNo(req.Verify)})
	p.Rows = append(p.Rows, Row{Label: "Unreadable sectors", Value: pick(req.SkipBad, "Fill with zeros and continue", "Stop")})
	if src.IsFile && src.Meta != nil {
		m := src.Meta
		p.Rows = append(p.Rows, Row{Label: "Image made", Value: m.Created.Local().Format("2 Jan 2006 15:04") + " from " + m.Source})
	}
	return p, nil
}

func planWipe(req *JobRequest, p *Plan) (*Plan, error) {
	dst, err := core.Resolve(req.Target, core.RoleTarget, core.KindDevice)
	if err != nil {
		return nil, err
	}
	pc, err := core.CheckWipeTarget(dst)
	if err != nil {
		return nil, err
	}
	p.Target, p.Risks = dst, pc.Risks
	p.Destructive, p.ConfirmName, p.Erases = true, dst.Name, dst.Erases
	mode := "Zeros"
	if req.WipeMode == string(core.WipeRandom) {
		mode = "Random data"
	}
	p.Rows = []Row{
		{Label: "Wipe", Value: dst.Desc},
		{Label: "Overwrite with", Value: mode},
		{Label: "Verify afterwards", Value: yesNo(req.Verify)},
	}
	return p, nil
}

// ---- running jobs ----------------------------------------------------------

type LogLine struct {
	Time  string `json:"time"`
	Text  string `json:"text"`
	Level string `json:"level"` // info, warn, error, ok
}

type Job struct {
	ID       int       `json:"id"`
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	Source   string    `json:"source"`
	Target   string    `json:"target"`
	State    string    `json:"state"` // running, done, failed, cancelled
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended"`
	Error    string    `json:"error,omitempty"`
	Hint     string    `json:"hint,omitempty"`
	Summary  []Row     `json:"summary"`
	Shutdown bool      `json:"shutdown"`
	Output   string    `json:"output,omitempty"` // file we produced, for "show in folder"

	tr     *core.Tracker
	cancel context.CancelFunc
	logs   []LogLine
}

type JobSnapshot struct {
	Job
	Status core.Status `json:"status"`
	Logs   []LogLine   `json:"logs"`
	NLogs  int         `json:"nlogs"`
}

type jobManager struct {
	mu     sync.Mutex
	job    *Job
	nextID int
	logger *fileLogger
}

var errBusy = errors.New("another operation is still running")

func (m *jobManager) busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.job != nil && m.job.State == "running"
}

func (m *jobManager) snapshot(since int) *JobSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil {
		return nil
	}
	s := &JobSnapshot{Job: *m.job, Status: m.job.tr.Snapshot(), NLogs: len(m.job.logs)}
	if since < len(m.job.logs) && since >= 0 {
		s.Logs = append([]LogLine(nil), m.job.logs[since:]...)
	}
	return s
}

func (m *jobManager) logf(j *Job, level, format string, args ...any) {
	line := LogLine{Time: time.Now().Format("15:04:05"), Text: fmt.Sprintf(format, args...), Level: level}
	m.mu.Lock()
	j.logs = append(j.logs, line)
	if len(j.logs) > 5000 {
		j.logs = j.logs[len(j.logs)-4000:]
	}
	m.mu.Unlock()
	m.logger.Printf("[job %d] %s: %s", j.ID, strings.ToUpper(level), line.Text)
}

func (m *jobManager) cancelJob() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil && m.job.State == "running" {
		m.job.cancel()
	}
}

// StartResponse tells the UI if we need one more answer before starting.
type StartResponse struct {
	Started bool     `json:"started"`
	Ask     string   `json:"ask,omitempty"` // "live" or "inuse"
	Volumes []string `json:"volumes,omitempty"`
}

func (m *jobManager) start(req *JobRequest) (*StartResponse, error) {
	if m.busy() {
		return nil, errBusy
	}
	plan, err := makePlan(req)
	if err != nil {
		return nil, err
	}
	if plan.Destructive && !strings.EqualFold(strings.TrimSpace(req.Confirm), plan.ConfirmName) {
		return nil, fmt.Errorf("type %s to confirm", plan.ConfirmName)
	}
	if len(plan.Risks) > 0 && !req.AcceptRisks {
		return nil, errors.New("please confirm the warnings first")
	}

	opts := core.DefaultOptions()
	opts.Verify, opts.SkipBad = req.Verify, req.SkipBad
	opts.AllowLive, opts.ForceDismount = req.AllowLive, req.ForceDismount
	if req.Retries >= 0 && req.Retries <= 20 {
		opts.Retries = req.Retries
	}
	if req.Level >= 1 && req.Level <= 9 {
		opts.Level = req.Level
	}

	// Open everything up front, so lock problems come back as a question
	// instead of a failed job.
	var src, dst, other core.Device
	var meta *core.ImageMeta
	closeAll := func() {
		for _, d := range []core.Device{src, dst, other} {
			if d != nil {
				d.Close()
			}
		}
	}
	if plan.Source != nil {
		src, err = core.OpenSource(plan.Source, opts.AllowLive)
		var live *core.LiveSourceError
		if errors.As(err, &live) {
			return &StartResponse{Ask: "live", Volumes: live.Volumes}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("opening the source: %w", err)
		}
	}
	switch req.Kind {
	case "compare":
		other, err = core.OpenSource(plan.Target, true)
		if err != nil {
			closeAll()
			return nil, err
		}
	case "clone", "backup", "restore", "wipe":
		if req.Kind == "backup" {
			meta = &core.ImageMeta{App: "DragonClone", Version: version, Created: time.Now(), Format: plan.Target.Format,
				Source: plan.Source.Desc, SourceName: plan.Source.Name, Model: plan.Source.Model, Style: plan.Source.Style,
				Size: plan.Source.Size, SectorSize: plan.Source.Sector}
		}
		dst, err = core.OpenTarget(plan.Target, opts, meta)
		var inUse *core.InUseError
		if errors.As(err, &inUse) {
			closeAll()
			return &StartResponse{Ask: "inuse", Volumes: inUse.Volumes}, nil
		}
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("opening the target: %w", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.nextID++
	j := &Job{ID: m.nextID, Kind: req.Kind, Title: plan.Title, State: "running", Started: time.Now(),
		tr: &core.Tracker{}, cancel: cancel, Shutdown: req.Shutdown}
	if plan.Source != nil {
		j.Source = plan.Source.Desc
	}
	if plan.Target != nil {
		j.Target = plan.Target.Desc
	}
	m.job = j
	m.mu.Unlock()
	j.tr.Logf = func(f string, a ...any) { m.logf(j, "warn", f, a...) }

	m.logf(j, "info", "%s started", j.Title)
	if j.Source != "" {
		m.logf(j, "info", "Source: %s", j.Source)
	}
	if j.Target != "" {
		m.logf(j, "info", "Target: %s", j.Target)
	}
	if opts.AllowLive {
		m.logf(j, "warn", "Copying even though source volumes are in use")
	}

	go func() {
		defer cancel()
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					m.logger.Printf("PANIC in job %d: %v\n%s", j.ID, r, debug.Stack())
					err = fmt.Errorf("internal error: %v (details are in the log)", r)
				}
			}()
			return m.run(ctx, j, req, plan, opts, src, dst, other, meta)
		}()
		closeErr := error(nil)
		for _, d := range []core.Device{src, dst, other} {
			if d != nil {
				if e := d.Close(); e != nil && d == dst && closeErr == nil {
					closeErr = e
				}
			}
		}
		if err == nil && closeErr != nil {
			err = fmt.Errorf("couldn't finish writing the target: %w", closeErr)
		}
		m.finish(j, req, plan, err)
	}()
	return &StartResponse{Started: true}, nil
}

func (m *jobManager) run(ctx context.Context, j *Job, req *JobRequest, plan *Plan, opts *core.Options,
	src, dst, other core.Device, meta *core.ImageMeta) error {
	add := func(label, value, tone string) {
		m.mu.Lock()
		j.Summary = append(j.Summary, Row{label, value, tone})
		m.mu.Unlock()
	}

	switch req.Kind {
	case "clone", "backup", "restore":
		res, err := core.Copy(ctx, src, dst, opts, j.tr)
		if err != nil {
			return err
		}
		speed := float64(res.Written) / max(res.CopyTime, 0.001)
		add("Copied", fmt.Sprintf("%s in %s (%s/s)", core.HumanBytes(res.Written),
			core.HumanDuration(time.Duration(res.CopyTime*float64(time.Second))), core.HumanBytes(int64(speed))), "")
		add("SHA-256", res.SHA256, "")
		m.logf(j, "info", "Copied %s, SHA-256 %s", core.HumanBytes(res.Written), res.SHA256)
		if res.BadBytes > 0 {
			add("Unreadable", fmt.Sprintf("%s in %d place(s), written as zeros", core.HumanBytes(res.BadBytes), len(res.Bad)), "warn")
			for i, b := range res.Bad {
				if i == 200 {
					m.logf(j, "warn", "...and %d more bad areas", len(res.Bad)-200)
					break
				}
				m.logf(j, "warn", "Bad area at byte %s, %s", core.CommaInt(b.Offset), core.HumanBytes(b.Length))
			}
		}
		if req.Kind == "backup" {
			meta.SHA256, meta.BadBytes = res.SHA256, res.BadBytes
			if err := core.SaveMeta(plan.Target.Path, meta); err != nil {
				m.logf(j, "warn", "Couldn't save the image info file: %v", err)
			}
			m.mu.Lock()
			j.Output = plan.Target.Path
			m.mu.Unlock()
			if st, err := os.Stat(plan.Target.Path); err == nil {
				v := core.HumanBytes(st.Size())
				if plan.Target.Format == core.FormatGzip && res.Size > 0 {
					v += fmt.Sprintf(" (%.0f%% of the disk)", float64(st.Size())*100/float64(res.Size))
				}
				add("Image file", v, "")
			}
		}
		if req.Kind == "restore" && plan.Source.Meta != nil && plan.Source.Meta.SHA256 != "" {
			if plan.Source.Meta.SHA256 == res.SHA256 {
				add("Image checksum", "Matches the one saved when the image was made", "good")
			} else {
				add("Image checksum", "Does NOT match the one saved when the image was made - the image may be damaged", "bad")
				m.logf(j, "error", "Image content doesn't match its saved checksum")
			}
		}
		if res.Verified {
			if len(res.Mismatches) == 0 {
				add("Verify", "Passed - the target reads back exactly", "good")
				m.logf(j, "ok", "Verify passed")
			} else {
				add("Verify", fmt.Sprintf("FAILED - %d block(s) read back differently", len(res.Mismatches)), "bad")
				return fmt.Errorf("verify failed: %d block(s) read back differently, first at byte %s",
					len(res.Mismatches), core.CommaInt(res.Mismatches[0]))
			}
		}

	case "compare":
		res, err := core.Compare(ctx, src, other, opts.BlockSize, j.tr)
		if err != nil {
			return err
		}
		add("Compared", fmt.Sprintf("%s (%s bytes)", core.HumanBytes(res.Length), core.CommaInt(res.Length)), "")
		if res.SizeA != res.SizeB {
			add("Note", fmt.Sprintf("Sizes differ (%s vs %s) - only the shared part was compared",
				core.HumanBytes(res.SizeA), core.HumanBytes(res.SizeB)), "warn")
		}
		if res.DiffBytes == 0 {
			add("Result", "Identical", "good")
			m.logf(j, "ok", "Identical")
		} else {
			first := make([]string, len(res.FirstDiffs))
			for i, o := range res.FirstDiffs {
				first[i] = core.CommaInt(o)
			}
			add("Result", fmt.Sprintf("Different - %s byte(s) differ", core.CommaInt(res.DiffBytes)), "bad")
			add("First differences at byte", strings.Join(first, ", "), "")
			m.logf(j, "warn", "%s bytes differ", core.CommaInt(res.DiffBytes))
		}

	case "check":
		sum, err := core.Hash(ctx, src, opts.BlockSize, j.tr)
		if err != nil {
			return err
		}
		add("SHA-256", sum, "")
		if plan.Source.IsFile {
			switch {
			case plan.Source.Meta == nil || plan.Source.Meta.SHA256 == "":
				add("Result", "Image reads fine all the way through (no saved checksum to compare with)", "good")
			case plan.Source.Meta.SHA256 == sum:
				add("Result", "Image is intact - matches the checksum saved when it was made", "good")
			default:
				add("Result", "Image is DAMAGED - it doesn't match the checksum saved when it was made", "bad")
				return errors.New("the image doesn't match its saved checksum")
			}
		}
		m.logf(j, "ok", "SHA-256 %s", sum)

	case "scan":
		res, err := core.Scan(ctx, src, opts.BlockSize, opts.Retries, j.tr)
		if err != nil {
			return err
		}
		add("Scanned", fmt.Sprintf("%s in %s", core.HumanBytes(res.Size), core.HumanDuration(time.Duration(res.Seconds*float64(time.Second)))), "")
		add("Average speed", core.HumanBytes(int64(res.AvgRate))+"/s", "")
		if res.MinRate > 0 {
			add("Slowest stretch", core.HumanBytes(int64(res.MinRate))+"/s", "")
		}
		if res.BadBytes == 0 {
			add("Result", "No unreadable sectors found", "good")
			m.logf(j, "ok", "No bad sectors")
		} else {
			add("Result", fmt.Sprintf("%s unreadable in %d place(s)", core.HumanBytes(res.BadBytes), len(res.Bad)), "bad")
			for i, b := range res.Bad {
				if i == 200 {
					break
				}
				m.logf(j, "warn", "Bad area at byte %s, %s", core.CommaInt(b.Offset), core.HumanBytes(b.Length))
			}
		}

	case "wipe":
		res, err := core.Wipe(ctx, dst, core.WipeMode(req.WipeMode), req.Verify, opts.BlockSize, j.tr)
		if err != nil {
			return err
		}
		add("Wiped", core.HumanBytes(res.Size), "")
		if res.Verified {
			if res.Bad == 0 {
				add("Verify", "Passed", "good")
			} else {
				add("Verify", fmt.Sprintf("FAILED - %d block(s) didn't keep what was written", res.Bad), "bad")
				return errors.New("verify failed - the disk didn't keep what was written")
			}
		}
	}
	return nil
}

func (m *jobManager) finish(j *Job, req *JobRequest, plan *Plan, err error) {
	m.mu.Lock()
	j.Ended = time.Now()
	switch {
	case err == nil:
		j.State = "done"
	case errors.Is(err, core.ErrCancelled):
		j.State = "cancelled"
	default:
		j.State = "failed"
		j.Error = err.Error()
		j.Hint = core.ErrorHint(err)
	}
	state := j.State
	m.mu.Unlock()

	switch state {
	case "done":
		m.logf(j, "ok", "Finished in %s", core.HumanDuration(j.Ended.Sub(j.Started)))
	case "cancelled":
		m.logf(j, "warn", "Cancelled")
	default:
		m.logf(j, "error", "Failed: %s", j.Error)
	}
	if state != "done" && req.Kind == "backup" && plan.Target != nil {
		core.RemoveImage(plan.Target.Path)
		m.logf(j, "info", "Removed the unfinished image file")
		m.mu.Lock()
		j.Output = ""
		m.mu.Unlock()
	}
	if state != "done" && plan.Target != nil && !plan.Target.IsFile && (req.Kind == "clone" || req.Kind == "restore") {
		m.logf(j, "warn", "%s is only partly written. Its old partition table was kept; run the operation again before using it.", plan.Target.Name)
	}
	shutdown := state == "done" && j.Shutdown
	if shutdown {
		if err := core.ScheduleShutdown(); err != nil {
			m.logf(j, "error", "Couldn't schedule the shutdown: %v", err)
			shutdown = false
		} else {
			m.logf(j, "info", "Shutting down in one minute")
		}
	}
	m.mu.Lock()
	j.Shutdown = shutdown
	entry := *j
	m.mu.Unlock()
	appendHistory(&entry)
}

// ---- history -----------------------------------------------------------------

type HistoryEntry struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Title   string    `json:"title"`
	Source  string    `json:"source"`
	Target  string    `json:"target"`
	State   string    `json:"state"`
	Seconds float64   `json:"seconds"`
	Error   string    `json:"error,omitempty"`
}

var historyMu sync.Mutex

func historyPath() string { return filepath.Join(core.DataDir(), "history.jsonl") }

func appendHistory(j *Job) {
	historyMu.Lock()
	defer historyMu.Unlock()
	e := HistoryEntry{Time: j.Started, Kind: j.Kind, Title: j.Title, Source: j.Source, Target: j.Target,
		State: j.State, Seconds: j.Ended.Sub(j.Started).Seconds(), Error: j.Error}
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(historyPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

func readHistory() []HistoryEntry {
	historyMu.Lock()
	defer historyMu.Unlock()
	data, _ := os.ReadFile(historyPath())
	var out []HistoryEntry
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0 && len(out) < 100; i-- {
		var e HistoryEntry
		if json.Unmarshal([]byte(lines[i]), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func yesNo(b bool) string { return pick(b, "Yes", "No") }

func pick(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
