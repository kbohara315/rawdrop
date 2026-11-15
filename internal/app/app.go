// Package app implements RawDrop's native user interface and job
// orchestration. The processing core lives in sibling packages and never
// depends on MyGo.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"rawdrop/internal/fsutil"
	"rawdrop/internal/journal"
	"rawdrop/internal/model"
	"rawdrop/internal/raw"
	"rawdrop/internal/scan"
	"rawdrop/internal/thumb"
)

type phase int

const (
	phaseEmpty phase = iota
	phaseReady
	phaseRunning
	phaseResults
)

// App is the state of the running window. Every field guarded by mu may be
// read by the view (on the main thread) and written by worker goroutines.
type App struct {
	mu sync.Mutex

	win   *mygo.Window
	store *journal.Store
	job   *model.Job

	inputs             []string
	outputDir          string
	correctOrientation bool
	preserveLocation   bool
	limitOutputSize    bool
	maxOutputBytes     int64

	phase  phase
	status string
	banner string
	flash  string

	running bool
	paused  bool
	cancel  context.CancelFunc
	wake    chan struct{}
	runID   int

	scanning     bool
	pendingPaths []string
	closed       bool

	// UI-thread-only state.
	grid      ui.GridState
	bitmaps   map[string]*ui.Bitmap
	thumbSize map[string]int
	selected  int
	icon      *ui.Bitmap
	iconDone  bool
}

// New creates an app with safe defaults. The previous job is loaded when Run
// is called, after MyGo has prepared its data directories.
func New() *App {
	return &App{
		wake:               make(chan struct{}, 1),
		bitmaps:            make(map[string]*ui.Bitmap),
		thumbSize:          make(map[string]int),
		correctOrientation: true,
		maxOutputBytes:     model.DefaultMaxOutputBytes,
		selected:           -1,
	}
}

// Attach connects the app to its window and loads any previous job.
func (a *App) Attach(win *mygo.Window) {
	a.mu.Lock()
	a.win = win
	a.mu.Unlock()
	a.icon = readIcon()
	a.iconDone = true

	win.OnFileDrop(func(e *mygo.FileDropEvent) {
		if len(e.Paths) > 0 {
			a.AddPaths(e.Paths)
		}
	})
	win.OnClose(func(*mygo.CloseEvent) {
		a.mu.Lock()
		a.closed = true
		if a.cancel != nil {
			a.cancel()
		}
		a.mu.Unlock()
	})

	a.loadPreviousLater()

	// Warn early when a required tool is missing instead of letting every
	// photo fail one by one.
	go func() {
		if err := raw.CheckTools(); err != nil {
			a.mu.Lock()
			a.banner = err.Error() + "  Install both with: sudo apt install libimage-exiftool-perl libjpeg-turbo-progs"
			a.mu.Unlock()
			a.redraw()
		}
	}()
}

// loadPreviousLater validates the previous job off the UI thread so the
// window appears immediately even when hundreds of outputs are checked.
func (a *App) loadPreviousLater() {
	go func() {
		a.loadPrevious()
		a.redraw()
	}()
}

func (a *App) loadPrevious() {
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return
	}
	a.loadPreviousFrom(dir)
}

// loadPreviousFrom loads the journal in dir. It is separate from
// loadPrevious so tests can run the recovery rules without MyGo's paths.
func (a *App) loadPreviousFrom(dir string) {
	store, err := journal.NewStore(dir)
	if err == nil {
		if err := store.Lock(); err == nil {
			a.mu.Lock()
			a.store = store
			a.mu.Unlock()
		}
	}
	if a.store == nil {
		return
	}
	job, err := a.store.Load(context.Background())
	if err != nil || job == nil || len(job.Items) == 0 {
		return
	}

	// Validate completed outputs without re-exporting work whose file was
	// removed or changed, and requeue anything interrupted mid-flight.
	for _, item := range job.Items {
		switch item.Status {
		case model.StatusDone:
			if item.OutputPath == "" || !validOutput(item) {
				item.Status = model.StatusQueued
				item.Message = "Output missing or modified; will export again"
				item.OutputPath = ""
				item.OutputSHA256 = ""
			}
		case model.StatusSkipped, model.StatusFailed, model.StatusCanceled:
			// Keep terminal outcomes visible; Export retries failed items.
		default:
			item.Status = model.StatusQueued
			item.Message = "Resuming after interruption"
		}
	}

	a.mu.Lock()
	a.job = job
	a.inputs = append([]string(nil), job.Inputs...)
	a.outputDir = job.Settings.OutputDir
	a.correctOrientation = job.Settings.CorrectOrientation
	a.preserveLocation = job.Settings.PreserveLocation
	if job.Settings.MaxOutputBytes > 0 {
		a.maxOutputBytes = job.Settings.MaxOutputBytes
	} else {
		a.maxOutputBytes = model.DefaultMaxOutputBytes
	}
	if a.maxOutputBytes < 500_000 {
		a.maxOutputBytes = 500_000
	} else if a.maxOutputBytes > 20_000_000 {
		a.maxOutputBytes = 20_000_000
	}
	job.Settings.MaxOutputBytes = a.maxOutputBytes
	a.limitOutputSize = job.Settings.LimitOutputSize || job.Settings.CompressUnder5MB
	if job.Settings.CompressUnder5MB && !job.Settings.LimitOutputSize {
		// Migrate the fixed 5 MB setting written by earlier releases.
		job.Settings.LimitOutputSize = true
		job.Settings.MaxOutputBytes = model.DefaultMaxOutputBytes
	}
	job.Settings.CompressUnder5MB = false
	c := job.Count()
	if job.Finished() {
		a.phase = phaseResults
		a.status = fmt.Sprintf("%d exported", c.Done)
	} else {
		a.phase = phaseReady
		a.status = fmt.Sprintf("%d of %d already exported", c.Done, c.Total)
		a.banner = "Previous export found. Start Export to resume where it stopped."
	}
	items := append([]*model.Item(nil), job.Items...)
	runID := a.runID
	a.mu.Unlock()
	go a.startThumbnails(items, runID)
}

func validOutput(item *model.Item) bool {
	st, err := os.Stat(item.OutputPath)
	if err != nil || st.IsDir() {
		return false
	}
	if item.OutputSHA256 == "" {
		return true
	}
	hash, err := hashFile(item.OutputPath)
	return err == nil && hash == item.OutputSHA256
}

// AddPaths adds dropped or selected files and folders to the batch. Scanning
// and thumbnail extraction happen off the UI thread.
func (a *App) AddPaths(paths []string) {
	if len(paths) == 0 {
		return
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	if a.running {
		a.mu.Unlock()
		a.toast("Wait for the current export to finish")
		return
	}
	if a.scanning {
		// A drop during a scan is remembered and handled after it.
		a.pendingPaths = append(a.pendingPaths, paths...)
		a.mu.Unlock()
		return
	}
	a.scanning = true
	defer func() {
		a.mu.Lock()
		pending := a.pendingPaths
		a.pendingPaths = nil
		a.scanning = false
		a.mu.Unlock()
		if len(pending) > 0 {
			a.AddPaths(pending)
		}
	}()
	a.phase = phaseReady
	a.status = "Looking for photos…"
	inputs := uniquePaths(append(a.inputs, paths...))
	if a.outputDir == "" {
		a.outputDir = defaultOutput(inputs)
	}
	current := a.job
	outDir := a.outputDir
	a.mu.Unlock()
	a.redraw()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := scan.Collect(ctx, inputs, outDir)
	if err != nil {
		a.setStatus("Could not read the dropped items: " + err.Error())
		return
	}

	a.mu.Lock()
	if current == nil {
		job := model.NewJob(inputs, model.DefaultSettings(outDir))
		job.Items = res.Items
		a.job = job
	} else {
		existing := make(map[string]bool, len(current.Items))
		for _, item := range current.Items {
			existing[item.ID] = true
		}
		for _, item := range res.Items {
			if !existing[item.ID] {
				current.Items = append(current.Items, item)
			}
		}
		current.Inputs = inputs
		current.Settings.OutputDir = outDir
	}
	a.inputs = inputs
	job := a.job
	total := len(job.Items)
	extra := ""
	if len(res.Unsupported) > 0 {
		extra = fmt.Sprintf(" • %d unsupported item(s) ignored", len(res.Unsupported))
	}
	a.status = fmt.Sprintf("%d photo(s) ready%s", total, extra)
	if total == 0 {
		a.banner = "No supported photos were found. Add RAW files or JPEG photos."
	} else {
		a.banner = ""
	}
	a.phase = phaseReady
	items := append([]*model.Item(nil), job.Items...)
	runID := a.runID
	a.saveLocked()
	a.mu.Unlock()
	a.redraw()
	go a.startThumbnails(items, runID)
}

// startThumbnails builds a bounded preview cache. It never blocks the UI and
// never writes files.
func (a *App) startThumbnails(items []*model.Item, runID int) {
	jobs := make(chan *model.Item)
	var wg sync.WaitGroup
	workers := 4
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				a.mu.Lock()
				stop := a.runID != runID
				has := len(item.Thumbnail) > 0
				source := item.Source
				a.mu.Unlock()
				if stop || has {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				b, err := thumb.Make(ctx, source)
				cancel()
				if err != nil {
					continue
				}
				a.mu.Lock()
				if a.runID == runID && len(item.Thumbnail) == 0 {
					item.Thumbnail = b
				}
				a.mu.Unlock()
				a.redraw()
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, item := range items {
			jobs <- item
		}
	}()
	wg.Wait()
}

// SetOutputDir records a destination chosen with the native folder dialog.
func (a *App) SetOutputDir(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	a.mu.Lock()
	a.outputDir = dir
	if a.job != nil {
		a.job.Settings.OutputDir = dir
	}
	a.mu.Unlock()
	a.redraw()
}

// ChoosePhotos opens the native multi-file picker.
func (a *App) ChoosePhotos() {
	a.mu.Lock()
	win := a.win
	a.mu.Unlock()
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:   win,
			Title:    "Add photos",
			Multiple: true,
			Filters: []mygo.FileFilter{{
				Name:       "RAW and JPEG photos",
				Extensions: []string{"cr2", "cr3", "nef", "nrw", "arw", "srf", "sr2", "dng", "raf", "rw2", "orf", "pef", "srw", "raw", "rwl", "3fr", "erf", "mef", "mos", "iiq", "jpg", "jpeg"},
			}},
		})
		if err != nil {
			a.setStatus("Could not open the file picker: " + err.Error())
			return
		}
		if len(paths) > 0 {
			a.AddPaths(paths)
		}
	}()
}

// ChooseFolder opens the native folder picker for input photos.
func (a *App) ChooseFolder() {
	a.mu.Lock()
	win := a.win
	a.mu.Unlock()
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:    win,
			Title:     "Add a folder of photos",
			Directory: true,
		})
		if err != nil {
			a.setStatus("Could not open the folder picker: " + err.Error())
			return
		}
		if len(paths) > 0 {
			a.AddPaths(paths)
		}
	}()
}

// ChooseOutput opens the native folder picker for the destination.
func (a *App) ChooseOutput() {
	a.mu.Lock()
	win := a.win
	a.mu.Unlock()
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:    win,
			Title:     "Choose an output folder",
			Directory: true,
			Multiple:  false,
		})
		if err != nil || len(paths) == 0 {
			return
		}
		a.SetOutputDir(paths[0])
	}()
}

// StartExport validates settings and begins a resumable export.
func (a *App) StartExport() {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return
	}
	if a.job == nil || len(a.job.Items) == 0 {
		a.mu.Unlock()
		a.toast("Add some photos first")
		return
	}
	if a.limitOutputSize {
		if err := raw.CheckCompressionTools(); err != nil {
			a.banner = "The JPEG size limit needs cjpeg and djpeg from libjpeg-turbo-progs."
			a.mu.Unlock()
			a.redraw()
			return
		}
	}
	if strings.TrimSpace(a.outputDir) == "" {
		a.mu.Unlock()
		a.ChooseOutput()
		return
	}
	// Only queued and failed items are attempted. Completed and skipped
	// items stay untouched, which makes Resume and Retry safe.
	pending := 0
	for _, item := range a.job.Items {
		if item.Status == model.StatusQueued || item.Status == model.StatusFailed ||
			item.Status == model.StatusCanceled {
			pending++
		}
	}
	if pending == 0 {
		a.phase = phaseResults
		a.mu.Unlock()
		a.redraw()
		return
	}
	a.job.Settings = model.Settings{
		OutputDir:          a.outputDir,
		CorrectOrientation: a.correctOrientation,
		PreserveLocation:   a.preserveLocation,
		LimitOutputSize:    a.limitOutputSize,
		MaxOutputBytes:     a.maxOutputBytes,
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.running = true
	a.paused = false
	a.runID++
	runID := a.runID
	a.phase = phaseRunning
	a.status = "Starting…"
	a.banner = ""
	a.saveLocked()
	a.mu.Unlock()
	a.redraw()
	go a.exportLoop(ctx, runID)
}

func (a *App) exportLoop(ctx context.Context, runID int) {
	for {
		item, ok := a.nextItem(ctx, runID)
		if !ok {
			break
		}
		if item == nil {
			break
		}
		a.exportOne(ctx, runID, item)
	}

	a.mu.Lock()
	if a.runID != runID {
		a.mu.Unlock()
		return
	}
	a.running = false
	a.paused = false
	cancel := a.cancel
	a.cancel = nil
	if ctx.Err() != nil {
		for _, item := range a.job.Items {
			if !item.Status.Terminal() {
				item.Status = model.StatusCanceled
				item.Message = "Canceled"
			}
		}
		a.phase = phaseReady
		a.status = "Export canceled. Completed photos were kept."
		a.banner = "Start Export to continue with the remaining photos."
	} else {
		c := a.job.Count()
		a.phase = phaseResults
		if c.Failed > 0 {
			a.status = fmt.Sprintf("Export finished: %d exported, %d skipped, %d failed", c.Done, c.Skipped, c.Failed)
		} else {
			a.status = fmt.Sprintf("Export complete: %d photo(s)", c.Done)
		}
	}
	a.saveLocked()
	job := a.job
	c := job.Count()
	win := a.win
	closed := a.closed
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.redraw()
	// Tell the desktop only when a window exists; without one (headless
	// tests, or a future windowless mode) there is nothing to notify.
	if ctx.Err() == nil && win != nil && !closed {
		n := mygo.NewNotification(mygo.NotificationOptions{
			Title: "RawDrop export finished",
			Body:  fmt.Sprintf("%d exported, %d skipped, %d failed", c.Done, c.Skipped, c.Failed),
		})
		_ = n.Show()
	}
}

// nextItem waits while paused and returns the next item to attempt.
func (a *App) nextItem(ctx context.Context, runID int) (*model.Item, bool) {
	for {
		select {
		case <-ctx.Done():
			return nil, false
		default:
		}
		a.mu.Lock()
		if a.runID != runID {
			a.mu.Unlock()
			return nil, false
		}
		if a.paused {
			a.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, false
			case <-a.wake:
				continue
			}
		}
		var next *model.Item
		for _, item := range a.job.Items {
			if item.Status == model.StatusQueued || item.Status == model.StatusFailed ||
				item.Status == model.StatusCanceled {
				next = item
				break
			}
		}
		a.mu.Unlock()
		if next == nil {
			return nil, true
		}
		return next, true
	}
}

func (a *App) exportOne(ctx context.Context, runID int, item *model.Item) {
	a.mu.Lock()
	outputDir := a.job.Settings.OutputDir
	opts := raw.Options{
		CorrectOrientation: a.job.Settings.CorrectOrientation,
		PreserveLocation:   a.job.Settings.PreserveLocation,
	}
	if a.job.Settings.LimitOutputSize {
		opts.MaxOutputBytes = a.job.Settings.MaxOutputBytes
		outputDir = filepath.Join(outputDir, sizeLimitSubdirectory(opts.MaxOutputBytes))
	}
	source := item.Source
	outputName := item.OutputName
	sourceName := item.Name
	a.mu.Unlock()

	if model.IsJPEGName(source) {
		a.setState(runID, item, model.StatusInspecting, "Reading JPEG metadata…")
	} else {
		a.setState(runID, item, model.StatusInspecting, "Reading camera metadata…")
	}
	info, err := raw.Inspect(ctx, source)
	if err != nil {
		a.finishItem(runID, item, err)
		return
	}
	a.mu.Lock()
	item.Width = info.Width
	item.Height = info.Height
	item.Orientation = info.Orientation
	item.Warning = ""
	a.mu.Unlock()

	if model.IsJPEGName(source) {
		a.setState(runID, item, model.StatusExtracting, "Preparing the JPEG…")
	} else {
		a.setState(runID, item, model.StatusExtracting, "Extracting the camera JPEG…")
	}
	dir := outputDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.finishItem(runID, item, fmt.Errorf("create output folder: %w", err))
		return
	}
	temp, err := os.CreateTemp(dir, ".rawdrop-*.jpg")
	if err != nil {
		a.finishItem(runID, item, err)
		return
	}
	tempPath := temp.Name()
	temp.Close()
	os.Remove(tempPath) // Export requires a path that does not exist yet.
	defer os.Remove(tempPath)

	if info.Orientation != 1 && opts.CorrectOrientation {
		a.setState(runID, item, model.StatusRotating, "Correcting orientation losslessly…")
	}
	result, err := raw.Export(ctx, source, tempPath, opts)
	if err != nil {
		a.finishItem(runID, item, err)
		return
	}

	// Pick the final name as late as possible so a file created while the
	// batch runs is never overwritten.
	name := outputName
	if name == "" {
		name = strings.TrimSuffix(sourceName, filepath.Ext(sourceName)) + ".jpg"
	}
	dest := fsutil.UniquePath(dir, name)
	if err := fsutil.CommitNew(tempPath, dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			dest = fsutil.UniquePath(dir, name)
			err = fsutil.CommitNew(tempPath, dest)
		}
		if err != nil {
			a.finishItem(runID, item, fmt.Errorf("save output: %w", err))
			return
		}
	}
	_ = fsutil.SyncDir(dir)

	// Temporary files are created with 0600; a finished export should be a
	// normal, readable photo.
	if err := os.Chmod(dest, 0o644); err != nil {
		a.finishItem(runID, item, fmt.Errorf("set output permissions: %w", err))
		return
	}

	hash, err := hashFile(dest)
	if err != nil {
		a.finishItem(runID, item, fmt.Errorf("verify output: %w", err))
		return
	}

	a.mu.Lock()
	if a.runID != runID {
		a.mu.Unlock()
		return
	}
	item.Status = model.StatusDone
	item.OutputPath = dest
	item.OutputName = filepath.Base(dest)
	item.OutputSHA256 = hash
	item.Width = result.Width
	item.Height = result.Height
	item.Orientation = result.Orientation
	item.Rotated = result.Rotated
	item.Compressed = result.Compressed
	item.Quality = result.Quality
	item.Warning = result.Warning
	item.Message = ""
	item.Thumbnail = nil
	a.saveLocked()
	a.mu.Unlock()
	a.redraw()
}

func (a *App) setState(runID int, item *model.Item, status model.Status, message string) {
	a.mu.Lock()
	if a.runID != runID {
		a.mu.Unlock()
		return
	}
	item.Status = status
	item.Message = message
	a.saveLocked()
	a.mu.Unlock()
	a.redraw()
}

func (a *App) finishItem(runID int, item *model.Item, err error) {
	a.mu.Lock()
	if a.runID != runID {
		a.mu.Unlock()
		return
	}
	if errors.Is(err, context.Canceled) {
		item.Status = model.StatusCanceled
		item.Message = "Canceled"
	} else if errors.Is(err, raw.ErrNoFullJPEG) {
		item.Status = model.StatusSkipped
		item.Message = err.Error()
	} else {
		item.Status = model.StatusFailed
		item.Message = err.Error()
	}
	a.saveLocked()
	a.mu.Unlock()
	a.redraw()
}

// Pause stops scheduling new files; the current file is allowed to finish.
func (a *App) Pause() {
	a.mu.Lock()
	if !a.running || a.paused {
		a.mu.Unlock()
		return
	}
	a.paused = true
	a.status = "Pausing after the current photo…"
	a.mu.Unlock()
	a.redraw()
}

// Resume continues a paused export.
func (a *App) Resume() {
	a.mu.Lock()
	if !a.running || !a.paused {
		a.mu.Unlock()
		return
	}
	a.paused = false
	a.status = "Exporting…"
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
	a.redraw()
}

// Cancel stops the export but keeps every committed file.
func (a *App) Cancel() {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return
	}
	a.status = "Canceling…"
	cancel := a.cancel
	paused := a.paused
	a.paused = false
	a.mu.Unlock()
	if paused {
		select {
		case a.wake <- struct{}{}:
		default:
		}
	}
	if cancel != nil {
		cancel()
	}
	a.redraw()
}

// RetryFailed queues failed items again and starts the export.
func (a *App) RetryFailed() {
	a.mu.Lock()
	if a.job == nil || a.running {
		a.mu.Unlock()
		return
	}
	for _, item := range a.job.Items {
		if item.Status == model.StatusFailed || item.Status == model.StatusCanceled {
			item.Status = model.StatusQueued
			item.Message = ""
		}
	}
	a.phase = phaseReady
	a.banner = "Retrying the photos that failed."
	a.mu.Unlock()
	a.StartExport()
}

// ClearExported removes successful entries from the batch journal and list.
// Exported JPEG files and the original photos are kept on disk.
func (a *App) ClearExported() {
	a.mu.Lock()
	if a.job == nil || a.running || a.scanning {
		a.mu.Unlock()
		return
	}
	remaining := make([]*model.Item, 0, len(a.job.Items))
	for _, item := range a.job.Items {
		if item.Status != model.StatusDone {
			remaining = append(remaining, item)
		}
	}
	removed := len(a.job.Items) - len(remaining)
	if removed == 0 {
		a.mu.Unlock()
		return
	}
	if len(remaining) == 0 {
		a.mu.Unlock()
		a.NewBatch()
		a.toast(fmt.Sprintf("Cleared %d exported photo(s) from the list", removed))
		return
	}

	inputs := make([]string, 0, len(remaining))
	for _, item := range remaining {
		inputs = append(inputs, item.Source)
	}
	previous := a.job
	updated := model.NewJob(inputs, previous.Settings)
	updated.Created = previous.Created
	updated.Items = remaining
	a.job = updated
	a.inputs = inputs
	a.runID++
	a.selected = -1
	a.grid = ui.GridState{}
	a.bitmaps = make(map[string]*ui.Bitmap)
	a.thumbSize = make(map[string]int)
	if updated.Finished() {
		a.phase = phaseResults
	} else {
		a.phase = phaseReady
	}
	a.status = fmt.Sprintf("Cleared %d exported photo(s) from the list", removed)
	a.banner = ""
	items := append([]*model.Item(nil), updated.Items...)
	runID := a.runID
	a.saveLocked()
	a.mu.Unlock()
	a.redraw()
	go a.startThumbnails(items, runID)
}

// confirmClearBatch clears the in-app batch and its resume journal after the
// user confirms. Source photos and exported files are never deleted.
func (a *App) confirmClearBatch() {
	a.mu.Lock()
	if a.job == nil || a.running || a.scanning || a.win == nil {
		a.mu.Unlock()
		return
	}
	win := a.win
	count := len(a.job.Items)
	a.mu.Unlock()

	result, err := mygo.Dialog.Message(mygo.MessageOptions{
		Parent:        win,
		Type:          mygo.MessageQuestion,
		Title:         "Clear photo list?",
		Message:       fmt.Sprintf("Remove all %d photos from this RawDrop batch?", count),
		Detail:        "This clears the saved batch and resume state. Source photos and exported JPEGs on disk will not be deleted.",
		Buttons:       []string{"Cancel", "Clear list"},
		DefaultButton: 0,
		CancelButton:  0,
	})
	if err != nil {
		a.toast("Could not open the clear confirmation: " + err.Error())
		return
	}
	if result.Button != 1 {
		return
	}
	a.NewBatch()
}

// NewBatch clears the current job. It never deletes exported files or
// originals; the journal is cleared so the next launch starts fresh.
func (a *App) NewBatch() {
	a.mu.Lock()
	if a.running || a.scanning {
		a.mu.Unlock()
		return
	}
	a.runID++
	a.job = nil
	a.inputs = nil
	a.outputDir = ""
	a.phase = phaseEmpty
	a.status = ""
	a.banner = ""
	a.selected = -1
	a.grid = ui.GridState{}
	a.bitmaps = make(map[string]*ui.Bitmap)
	a.thumbSize = make(map[string]int)
	store := a.store
	a.mu.Unlock()
	if store != nil {
		_ = store.Clear(context.Background())
	}
	a.redraw()
}

// OpenOutput opens the export directory in the desktop's file manager.
func (a *App) OpenOutput() {
	a.mu.Lock()
	dir := a.outputDir
	if dir != "" && a.limitOutputSize {
		dir = filepath.Join(dir, sizeLimitSubdirectory(a.maxOutputBytes))
	}
	a.mu.Unlock()
	if dir == "" {
		return
	}
	if err := mygo.Shell.OpenPath(dir); err != nil {
		a.toast("Could not open the output folder: " + err.Error())
	}
}

// OpenFile shows one exported photo in the default image viewer.
func (a *App) OpenFile(path string) {
	if path == "" {
		return
	}
	if err := mygo.Shell.OpenPath(path); err != nil {
		a.toast("Could not open the photo: " + err.Error())
	}
}

func sizeLimitSubdirectory(maxBytes int64) string {
	if maxBytes == model.MaxOutputBytesUnder5MB {
		return "Under 5 MB"
	}
	mb := strconv.FormatFloat(float64(maxBytes)/1_000_000, 'f', -1, 64)
	return "Up to " + mb + " MB"
}

func (a *App) saveLocked() {
	if a.store == nil || a.job == nil {
		return
	}
	_ = a.store.Save(context.Background(), a.job)
}

func (a *App) setStatus(status string) {
	a.mu.Lock()
	a.status = status
	a.mu.Unlock()
	a.redraw()
}

func (a *App) redraw() {
	a.mu.Lock()
	win := a.win
	closed := a.closed
	a.mu.Unlock()
	if win != nil && !closed {
		win.Update(func() {})
	}
}

func (a *App) toast(message string) {
	a.mu.Lock()
	win := a.win
	a.mu.Unlock()
	if win == nil {
		return
	}
	win.Update(func() {
		a.mu.Lock()
		a.flash = message
		a.mu.Unlock()
	})
}

func readIcon() *ui.Bitmap {
	dir, err := mygo.App.Path(mygo.PathResources)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "icon.png"))
	if err != nil {
		return nil
	}
	bmp, err := ui.DecodeBitmap(data)
	if err != nil {
		return nil
	}
	return bmp
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	sort.Strings(out)
	return out
}

func defaultOutput(inputs []string) string {
	home, _ := os.UserHomeDir()
	fallback := filepath.Join(home, "Pictures", "RawDrop Exports")
	if len(inputs) == 0 {
		return fallback
	}
	// Use the directory of every input (its parent for a file) and find the
	// deepest one that contains all the others, so dropping two folders side
	// by side exports beside them instead of forcing a choice.
	base := ""
	for _, in := range inputs {
		dir := filepath.Clean(in)
		if st, err := os.Stat(dir); err == nil && !st.IsDir() {
			dir = filepath.Dir(dir)
		}
		if base == "" {
			base = dir
			continue
		}
		for !within(base, dir) {
			parent := filepath.Dir(base)
			if parent == base {
				return fallback
			}
			base = parent
		}
	}
	root := filepath.VolumeName(base) + string(filepath.Separator)
	if base == "" || base == root {
		return fallback
	}
	return filepath.Join(base, "JPEG - Full Resolution")
}

// within reports whether dir is base itself or lies inside base.
func within(base, dir string) bool {
	rel, err := filepath.Rel(base, dir)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
