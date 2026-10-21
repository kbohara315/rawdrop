// Command rawdrop-cli exports full-resolution JPEGs from RAW photos without
// the desktop interface. It uses exactly the same processing core as the
// RawDrop app, which makes it useful for scripting and for acceptance tests.
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
	"syscall"
	"time"

	"rawdrop/internal/exporter"
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
		fmt.Printf("found %d RAW photo(s), %d unsupported item(s), %d skipped path(s)\n",
			len(res.Items), len(res.Unsupported), len(res.SkippedDirs))
	}
	if len(res.Items) == 0 {
		fmt.Fprintln(os.Stderr, "rawdrop-cli: nothing to export")
		return 1
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "rawdrop-cli:", err)
		return 1
	}

	var done, skipped, failed int
	for i, item := range res.Items {
		if err := ctx.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "rawdrop-cli: interrupted")
			break
		}
		start := time.Now()
		status, warning := exportOne(ctx, item.Source, exporter.Options{
			OutputDir:          dir,
			OutputName:         item.OutputName,
			CorrectOrientation: !*noRotate,
			PreserveLocation:   *keepGPS,
		})
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

	fmt.Printf("exported %d, skipped %d, failed %d into %s\n", done, skipped, failed, dir)
	if failed > 0 {
		return 1
	}
	return 0
}

// exportOne runs the shared per-photo exporter: inspect once, extract, verify,
// and commit without ever replacing an existing output. It processes items one
// at a time; the CLI stays sequential by default.
func exportOne(ctx context.Context, source string, opts exporter.Options) (model.Status, string) {
	result, err := exporter.Export(ctx, source, opts)
	if err == nil {
		return model.StatusDone, result.Warning
	}
	if errors.Is(err, raw.ErrNoFullJPEG) {
		fmt.Fprintf(os.Stderr, "  skipped %s: %v\n", filepath.Base(source), err)
		return model.StatusSkipped, ""
	}
	fmt.Fprintf(os.Stderr, "  failed %s: %v\n", filepath.Base(source), err)
	return model.StatusFailed, ""
}
