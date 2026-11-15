// Command rawdrop-cli exports JPEGs from RAW or JPEG photos without the
// desktop interface. It uses the same processing core as the RawDrop app.
//
// Usage:
//
//	rawdrop-cli -o OUTPUT [flags] FILE_OR_DIR...
//
// Flags:
//
//	-o, --output DIR   output directory (required)
//	--keep-gps         keep GPS tags (default strips them)
//	--no-rotate        only write EXIF orientation, never rotate pixels
//	--under-5mb        make lossy, full-resolution copies under 5 MB
//	--quiet            only print the final summary
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"rawdrop/internal/fsutil"
	"rawdrop/internal/model"
	"rawdrop/internal/raw"
	"rawdrop/internal/scan"
)

func main() {
	os.Exit(run())
}

func run() int {
	output := flag.String("o", "", "output directory (required)")
	outputLong := flag.String("output", "", "output directory (required)")
	keepGPS := flag.Bool("keep-gps", false, "keep GPS metadata in exported JPEGs")
	noRotate := flag.Bool("no-rotate", false, "do not physically rotate; write EXIF orientation only")
	under5MB := flag.Bool("under-5mb", false, "create lossy full-resolution JPEGs under 5 MB")
	quiet := flag.Bool("quiet", false, "only print the final summary")
	timeout := flag.Duration("timeout", 0, "overall timeout, for example 10m (0 means none)")
	flag.Parse()

	dir := *output
	if dir == "" {
		dir = *outputLong
	}
	inputs := flag.Args()
	if dir == "" || len(inputs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: rawdrop-cli -o OUTPUT [flags] FILE_OR_DIR...")
		flag.PrintDefaults()
		return 2
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rawdrop-cli:", err)
		return 1
	}
	dir = absDir

	if err := raw.CheckTools(); err != nil {
		fmt.Fprintln(os.Stderr, "rawdrop-cli:", err)
		return 1
	}
	if *under5MB {
		if err := raw.CheckCompressionTools(); err != nil {
			fmt.Fprintln(os.Stderr, "rawdrop-cli:", err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	res, err := scan.Collect(ctx, inputs, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rawdrop-cli:", err)
		return 1
	}
	if !*quiet {
		fmt.Printf("found %d photo(s), %d unsupported item(s), %d skipped path(s)\n",
			len(res.Items), len(res.Unsupported), len(res.SkippedDirs))
	}
	if len(res.Items) == 0 {
		fmt.Fprintln(os.Stderr, "rawdrop-cli: nothing to export")
		return 1
	}
	outputDir := dir
	if *under5MB {
		outputDir = filepath.Join(dir, "Under 5 MB")
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "rawdrop-cli:", err)
		return 1
	}

	var done, skipped, failed int
	for i, item := range res.Items {
		if err := ctx.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "rawdrop-cli: interrupted")
			break
		}
		name := item.OutputName
		if name == "" {
			name = strings.TrimSuffix(item.Name, filepath.Ext(item.Name)) + ".jpg"
		}
		start := time.Now()
		opts := raw.Options{
			CorrectOrientation: !*noRotate,
			PreserveLocation:   *keepGPS,
		}
		if *under5MB {
			opts.MaxOutputBytes = model.MaxOutputBytesUnder5MB
		}
		status, warning := exportOne(ctx, outputDir, item.Source, name, opts)
		switch status {
		case model.StatusDone:
			done++
		case model.StatusSkipped:
			skipped++
		default:
			failed++
		}
		if !*quiet {
			line := fmt.Sprintf("[%d/%d] %s %s (%s)", i+1, len(res.Items), status, item.Name, time.Since(start).Round(time.Millisecond))
			if warning != "" {
				line += " — " + warning
			}
			fmt.Println(line)
		}
	}

	fmt.Printf("exported %d, skipped %d, failed %d into %s\n", done, skipped, failed, outputDir)
	if failed > 0 {
		return 1
	}
	return 0
}

// exportOne mirrors the app's per-item pipeline: inspect, export to a
// temporary file, then commit the file without ever replacing an existing
// output.
func exportOne(ctx context.Context, dir, source, name string, opts raw.Options) (model.Status, string) {
	info, err := raw.Inspect(ctx, source)
	if err != nil {
		if errors.Is(err, raw.ErrNoFullJPEG) {
			fmt.Fprintf(os.Stderr, "  skipped %s: %v\n", filepath.Base(source), err)
			return model.StatusSkipped, ""
		}
		fmt.Fprintf(os.Stderr, "  failed %s: %v\n", filepath.Base(source), err)
		return model.StatusFailed, ""
	}
	_ = info

	temp, err := os.CreateTemp(dir, ".rawdrop-*.jpg")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  failed %s: %v\n", filepath.Base(source), err)
		return model.StatusFailed, ""
	}
	tempPath := temp.Name()
	temp.Close()
	os.Remove(tempPath)
	defer os.Remove(tempPath)

	result, err := raw.Export(ctx, source, tempPath, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  failed %s: %v\n", filepath.Base(source), err)
		return model.StatusFailed, ""
	}
	dest := fsutil.UniquePath(dir, name)
	if err := fsutil.CommitNew(tempPath, dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			dest = fsutil.UniquePath(dir, name)
			err = fsutil.CommitNew(tempPath, dest)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  failed %s: %v\n", filepath.Base(source), err)
			return model.StatusFailed, ""
		}
	}
	_ = fsutil.SyncDir(dir)
	// Temporary files are created with 0600; a finished export should be a
	// normal, readable photo.
	_ = os.Chmod(dest, 0o644)
	_ = result
	return model.StatusDone, result.Warning
}
