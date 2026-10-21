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
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"rawdrop/internal/exporter"
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

	phase  phase
	status string
	banner string
	flash  string

	running   bool
	paused    bool
	cancel    context.CancelFunc
	pauseWait chan struct{}
	runID     int
	canceling bool

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
		pauseWait:          make(chan struct{}),
		bitmaps:            make(map[string]*ui.Bitmap),
		thumbSize:          make(map[string]int),
		correctOrientation: true,
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
	a.status = "Looking for RAW photos…"
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
		extra = fmt.Sprintf(" • %d non-RAW item(s) ignored", len(res.Unsupported))
	}
	a.status = fmt.Sprintf("%d photo(s) ready%s", total, extra)
	if total == 0 {
		a.banner = "No supported RAW photos were found. Drop .CR2 files or a folder containing them."
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

// SetOutputDir records a destination chosen with the native folder dialog. The
// path is normalized, applied to the app and the job, and written to the
// journal immediately so the choice survives a crash before Export starts.
//
// A change is refused, with an explanation, when the export is running or when
// the batch has already committed an output: once any photo is written, the
// destination is fixed for that job and a new batch is required. A destination
// that exists and is a regular file is refused too, as is any target that
// stats with an error other than "does not exist" (a permission, not-a-
// directory, or I/O error), so an unusable path is surfaced instead of being
// mistaken for a folder that will be created. A genuinely missing directory is
// allowed; the export creates it.
func (a *App) SetOutputDir(dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		a.refuseDestination("That destination could not be used: " + err.Error())
		return
	}
	abs = filepath.Clean(abs)

	a.mu.Lock()
	switch {
	case a.running:
		a.banner = "The destination is fixed while an export is running."
	case a.job != nil && a.committedOutputLocked():
		a.banner = "This batch already exported photos. Start a new batch to change the destination."
	default:
		switch st, statErr := os.Stat(abs); {
		case statErr != nil && !errors.Is(statErr, os.ErrNotExist):
			// A permission, not-a-directory, or I/O error must be surfaced
			// rather than mistaken for a folder that will be created later.
			a.banner = "That destination could not be used: " + statErr.Error()
		case statErr == nil && !st.IsDir():
			a.banner = "That destination is a file, not a folder: " + abs
		default:
			a.outputDir = abs
			if a.job != nil {
				a.job.Settings.OutputDir = abs
				a.saveLocked()
			}
			a.banner = ""
		}
	}
	a.mu.Unlock()
	a.redraw()
}

// committedOutputLocked reports whether any item has a committed output. It
// must be called with a.mu held.
func (a *App) committedOutputLocked() bool {
	if a.job == nil {
		return false
	}
	for _, item := range a.job.Items {
		if item.OutputPath != "" {
			return true
		}
	}
	return false
}

// refuseDestination surfaces a rejected destination without changing state.
func (a *App) refuseDestination(message string) {
	a.mu.Lock()
	a.banner = message
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
			Title:    "Add RAW photos",
			Multiple: true,
			Filters: []mygo.FileFilter{{
				Name:       "RAW photos",
				Extensions: model.RawExtensionList(),
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
			Title:     "Add a folder of RAW photos",
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
		a.toast("Add some RAW photos first")
		return
	}
	if strings.TrimSpace(a.outputDir) == "" {
		a.mu.Unlock()
		a.ChooseOutput()
		return
	}
	// Only queued items are attempted from here on. Failed and canceled items
	// are requeued once so this Export/Retry picks them up; completed and
	// skipped items stay untouched. Requeuing up front, rather than selecting
	// failed items inside the worker loop, keeps a repeatedly failing item from
	// being retried forever inside a single run.
	pending := 0
	for _, item := range a.job.Items {
		switch item.Status {
		case model.StatusFailed, model.StatusCanceled:
			item.Status = model.StatusQueued
			item.Message = ""
		}
		if item.Status == model.StatusQueued {
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
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.running = true
	a.paused = false
	a.canceling = false
	a.pauseWait = make(chan struct{})
	a.runID++
	runID := a.runID
	a.phase = phaseRunning
	a.status = "Exporting…"
	a.banner = ""
	a.saveLocked()
	a.mu.Unlock()
	a.redraw()
	go a.exportLoop(ctx, runID)
}

// exportWorkers bounds how many photos are exported at once. A small pool keeps
// ExifTool processes, memory, and disk contention bounded while overlapping the
// I/O and process time of independent photos. It is an arbitrary safety bound,
// not a measured optimum.
const exportWorkers = 3

// exportLoop runs a bounded pool of export workers for one run and finalizes
// the job only after every worker has stopped, so the final state and the
// journal save never race with a late worker.
func (a *App) exportLoop(ctx context.Context, runID int) {
	var wg sync.WaitGroup
	for i := 0; i < exportWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.exportWorker(ctx, runID)
		}()
	}
	wg.Wait()
	a.finishExport(ctx, runID)
}

// exportWorker reserves and exports items until the run ends: the context is
// canceled, the run is superseded, or no queued item remains.
func (a *App) exportWorker(ctx context.Context, runID int) {
	for {
		item := a.reserveItem(ctx, runID)
		if item == nil {
			return
		}
		a.exportItem(ctx, runID, item)
	}
}

// reserveItem atomically claims the next queued item by marking it inspecting
// under the app mutex, so two workers can never take the same photo. It blocks
// while paused without claiming anything, and returns nil when the run is over.
// Pause therefore stops new reservations while work already reserved finishes.
func (a *App) reserveItem(ctx context.Context, runID int) *model.Item {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		a.mu.Lock()
		if a.runID != runID {
			a.mu.Unlock()
			return nil
		}
		if a.paused {
			wait := a.pauseWait
			a.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil
			case <-wait:
				continue
			}
		}
		var next *model.Item
		for _, item := range a.job.Items {
			if item.Status == model.StatusQueued {
				item.Status = model.StatusInspecting
				item.Message = "Reading camera metadata…"
				next = item
				break
			}
		}
		if next != nil {
			a.saveLocked()
		}
		a.mu.Unlock()
		if next != nil {
			a.redraw()
		}
		return next
	}
}

// exportItem runs one photo through the shared exporter and records the
// outcome on the item. Progress callbacks run on this worker goroutine and only
// mutate shared state under App.mu.
func (a *App) exportItem(ctx context.Context, runID int, item *model.Item) {
	a.mu.Lock()
	if a.runID != runID {
		a.mu.Unlock()
		return
	}
	opts := exporter.Options{
		OutputDir:          a.job.Settings.OutputDir,
		OutputName:         item.OutputName,
		CorrectOrientation: a.job.Settings.CorrectOrientation,
		PreserveLocation:   a.job.Settings.PreserveLocation,
	}
	opts.Progress = func(p exporter.Progress) {
		a.mu.Lock()
		if a.runID != runID {
			a.mu.Unlock()
			return
		}
		item.Status = stageStatus(p.Stage)
		item.Message = stageMessage(p.Stage)
		if p.Info.Width > 0 || p.Info.Height > 0 {
			item.Width, item.Height = p.Info.Width, p.Info.Height
		}
		if p.Info.Orientation >= 1 {
			item.Orientation = p.Info.Orientation
		}
		a.saveLocked()
		a.mu.Unlock()
		a.redraw()
	}
	source := item.Source
	a.mu.Unlock()

	result, err := exporter.Export(ctx, source, opts)
	if err != nil {
		a.finishItem(runID, item, err)
		return
	}

	a.mu.Lock()
	if a.runID != runID {
		a.mu.Unlock()
		return
	}
	item.Status = model.StatusDone
	item.OutputPath = result.OutputPath
	item.OutputName = result.OutputName
	item.OutputSHA256 = result.SHA256
	item.Width = result.Width
	item.Height = result.Height
	item.Orientation = result.Orientation
	item.Rotated = result.Rotated
	item.Warning = result.Warning
	item.Message = ""
	item.Thumbnail = nil
	a.saveLocked()
	a.mu.Unlock()
	a.redraw()
}

// stageStatus maps an exporter stage to the model status the view shows.
func stageStatus(stage exporter.Stage) model.Status {
	switch stage {
	case exporter.StageExtract:
		return model.StatusExtracting
	case exporter.StageRotate:
		return model.StatusRotating
	default:
		return model.StatusInspecting
	}
}

// stageMessage is the human-readable text for an exporter stage.
func stageMessage(stage exporter.Stage) string {
	switch stage {
	case exporter.StageExtract:
		return "Extracting the camera JPEG…"
	case exporter.StageRotate:
		return "Correcting orientation losslessly…"
	default:
		return "Reading camera metadata…"
	}
}

// finishExport finalizes a run after every worker has stopped: it leaves the
// running phase, marks anything still unfinished canceled, saves the journal,
// and notifies the desktop. A superseded run (runID changed) does nothing.
func (a *App) finishExport(ctx context.Context, runID int) {
	a.mu.Lock()
	if a.runID != runID {
		a.mu.Unlock()
		return
	}
	a.running = false
	a.paused = false
	a.canceling = false
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

// Pause stops scheduling new files; photos already in flight are allowed to
// finish. Workers wait on pauseWait instead of claiming another item.
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

// Resume continues a paused export. Closing pauseWait wakes every waiting
// worker at once; a fresh channel is left for the next pause.
func (a *App) Resume() {
	a.mu.Lock()
	if !a.running || !a.paused {
		a.mu.Unlock()
		return
	}
	a.paused = false
	close(a.pauseWait)
	a.pauseWait = make(chan struct{})
	a.status = "Exporting…"
	a.mu.Unlock()
	a.redraw()
}

// Cancel stops the export but keeps every committed file. Canceling the
// context stops in-flight helpers and also wakes any workers waiting on
// pauseWait, so a paused export still shuts down.
func (a *App) Cancel() {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return
	}
	a.status = "Canceling…"
	a.canceling = true
	cancel := a.cancel
	a.paused = false
	a.mu.Unlock()
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

// NewBatch clears the current job. It never deletes exported files or
// originals; the journal is cleared so the next launch starts fresh.
func (a *App) NewBatch() {
	a.mu.Lock()
	if a.running {
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
