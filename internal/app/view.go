package app

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"

	"rawdrop/internal/model"
)

type viewState struct {
	phase              phase
	status             string
	banner             string
	flash              string
	outputDir          string
	correctOrientation bool
	preserveLocation   bool
	limitOutputSize    bool
	compressionLimitMB float64
	running            bool
	paused             bool
	scanning           bool
	items              []model.Item
	counts             model.Counts
}

func (a *App) snapshot() viewState {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := viewState{
		phase:              a.phase,
		status:             a.status,
		banner:             a.banner,
		flash:              a.flash,
		outputDir:          a.outputDir,
		correctOrientation: a.correctOrientation,
		preserveLocation:   a.preserveLocation,
		limitOutputSize:    a.limitOutputSize,
		compressionLimitMB: float64(a.maxOutputBytes) / 1_000_000,
		running:            a.running,
		paused:             a.paused,
		scanning:           a.scanning,
	}
	a.flash = ""
	if a.job != nil {
		st.counts = a.job.Count()
		st.items = make([]model.Item, len(a.job.Items))
		for i, item := range a.job.Items {
			st.items[i] = *item
		}
	}
	return st
}

// View builds the window's interface. All state is snapshotted before any
// element is built, so element callbacks may safely call App methods.
func (a *App) View(c *ui.Context) {
	st := a.snapshot()
	t := c.Theme()
	if st.flash != "" {
		c.Toast(st.flash)
	}
	if c.Shortcut(ui.Cmd, ui.KeyO) {
		a.ChoosePhotos()
	}
	if c.Shortcut(ui.Cmd|ui.Shift, ui.KeyO) {
		a.ChooseFolder()
	}
	if c.Shortcut(ui.Cmd, ui.KeyE) && !st.running {
		a.StartExport()
	}

	ui.Column(c).Fill().Background(t.Background).Children(func() {
		a.header(c, st)
		ui.Row(c).Fill().Grow(1).Children(func() {
			if st.phase == phaseEmpty || len(st.items) == 0 {
				a.welcome(c, st)
				return
			}
			a.sidebar(c, st)
			a.main(c, st)
		})
		a.footer(c, st)
	})
}

func (a *App) header(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Row(c).FillWidth().Height(76).Padding(24, 12).Gap(14).
		AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).
		Children(func() {
			if bmp := a.loadIcon(); bmp != nil {
				ui.Image(c, bmp).Size(40, 40).Radius(10)
			} else {
				ui.Box(c).Size(40, 40).Radius(10).Background(t.Accent)
			}
			ui.Column(c).Gap(2).Children(func() {
				ui.Text(c, "RawDrop").FontSize(20).Bold()
				ui.Text(c, "RAW and JPEG photos • offline").FontSize(12).TextColor(t.TextMuted)
			})
			ui.Spacer(c).Grow(1)
			if st.counts.Total > 0 {
				ui.Badge(c, fmt.Sprintf("%d photos", st.counts.Total))
			}
			if st.phase != phaseRunning {
				if ui.Button(c, "Add photos").Clicked() {
					a.ChoosePhotos()
				}
				if ui.Button(c, "Add folder").Clicked() {
					a.ChooseFolder()
				}
			}
			if st.counts.Total > 0 && !st.running && !st.scanning {
				if ui.Button(c, "Clear list").Clicked() {
					go a.confirmClearBatch()
				}
			}
		})
}

func (a *App) welcome(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Column(c).Fill().Grow(1).Padding(40).Center().Gap(18).Children(func() {
		zone := ui.Column(c).Width(620).Height(320).Center().Gap(14).Padding(36).
			Radius(18).Border(2, t.Border).BorderStyle(ui.BorderDashed)
		over := zone.FileDragOver()
		if over {
			zone.Border(2, t.Accent).Background(t.SurfaceHover)
		}
		if files := zone.DroppedFiles(); files != nil {
			a.AddPaths(files)
		}
		zone.Children(func() {
			ui.Text(c, "Drop RAW photos here").FontSize(28).Bold().TextAlign(ui.Center)
			ui.Text(c, "or drop a folder full of photos").FontSize(15).TextColor(t.TextMuted).TextAlign(ui.Center)
			ui.Row(c).Gap(10).Children(func() {
				if ui.PrimaryButton(c, "Choose photos").Clicked() {
					a.ChoosePhotos()
				}
				if ui.Button(c, "Choose folder").Clicked() {
					a.ChooseFolder()
				}
			})
			ui.Text(c, "Camera RAW files and regular .JPG/.JPEG photos are supported").
				FontSize(12).TextColor(t.TextMuted).TextAlign(ui.Center)
		})

		if st.status != "" {
			ui.Text(c, st.status).FontSize(13).TextColor(t.TextMuted).TextAlign(ui.Center)
		}
		if st.banner != "" {
			ui.Text(c, st.banner).FontSize(13).TextColor(t.Warning).TextAlign(ui.Center)
		}

		ui.Row(c).Gap(26).Children(func() {
			a.promise(c, "Originals stay untouched", "Read-only source access")
			a.promise(c, "Lossless by default", "JPEG image data is preserved")
			a.promise(c, "100% local", "No uploads, accounts or telemetry")
		})
	})
}

func (a *App) promise(c *ui.Context, title, detail string) {
	t := c.Theme()
	ui.Column(c).Gap(3).Width(180).Children(func() {
		ui.Text(c, title).FontSize(12).Bold()
		ui.Text(c, detail).FontSize(11).TextColor(t.TextMuted)
	})
}

func (a *App) sidebar(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Column(c).Width(310).HeightPercent(100).Padding(20).Gap(16).
		BorderWidth(0, 1, 0, 0).BorderColor(t.Border).Children(func() {
		ui.Column(c).FillWidth().Padding(16).Radius(12).Background(t.Surface).Gap(10).Children(func() {
			ui.Text(c, "Output folder").FontSize(12).Bold().TextColor(t.TextMuted)
			path := st.outputDir
			if path == "" {
				path = "Choose a folder for the JPEGs"
			}
			ui.Text(c, path).FontSize(13).SingleLine().Ellipsis("…")
			if st.limitOutputSize {
				folder := sizeLimitSubdirectory(int64(st.compressionLimitMB * 1_000_000))
				ui.Text(c, "Compressed copies go in /"+folder).FontSize(10).TextColor(t.TextMuted)
			}
			if st.phase != phaseRunning && ui.Button(c, "Change folder").Clicked() {
				a.ChooseOutput()
			}
		})

		ui.Column(c).FillWidth().Padding(16).Radius(12).Background(t.Surface).Gap(12).Children(func() {
			ui.Text(c, "Quality").FontSize(12).Bold().TextColor(t.TextMuted)
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(8, 8).Radius(4).Background(t.Success)
				ui.Text(c, "Lossless JPEG optimization").FontSize(13)
			})
			ui.Text(c, "Orientation and size optimization preserve image data by default.").
				FontSize(11).TextColor(t.TextMuted)

			correct := st.correctOrientation
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				if ui.Switch(c, &correct).Changed() && st.phase != phaseRunning {
					a.setCorrectOrientation(correct)
				}
				ui.Column(c).Gap(1).Children(func() {
					ui.Text(c, "Correct orientation").FontSize(13)
					ui.Text(c, "Lossless rotation when possible").FontSize(10).TextColor(t.TextMuted)
				})
			})

			limitOutput := st.limitOutputSize
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				if ui.Switch(c, &limitOutput).Changed() && st.phase != phaseRunning {
					a.setLimitOutputSize(limitOutput)
				}
				ui.Column(c).Gap(1).Children(func() {
					ui.Text(c, "Limit JPEG file size").FontSize(13)
					ui.Text(c, "Lossy recompression, full resolution").FontSize(10).TextColor(t.TextMuted)
				})
			})
			if st.limitOutputSize {
				limitMB := st.compressionLimitMB
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "Maximum size").FontSize(12).TextColor(t.TextMuted)
					ui.Spacer(c).Grow(1)
					input := ui.NumberInput(c, &limitMB, 0.5, 20, 0.1).Width(92).Label("Maximum JPEG size in megabytes")
					if input.Changed() && st.phase != phaseRunning {
						a.setCompressionLimitMB(limitMB)
					}
					ui.Text(c, "MB").FontSize(12).TextColor(t.TextMuted)
				})
				slider := ui.StepSlider(c, &limitMB, 0.5, 20, 0.1).FillWidth().Label("Maximum JPEG size in megabytes")
				if slider.Changed() && st.phase != phaseRunning {
					a.setCompressionLimitMB(limitMB)
				}
				ui.Text(c, "0.5 MB to 20 MB per photo").FontSize(10).TextColor(t.TextMuted)
			}

			gps := st.preserveLocation
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				if ui.Switch(c, &gps).Changed() && st.phase != phaseRunning {
					a.setPreserveLocation(gps)
				}
				ui.Column(c).Gap(1).Children(func() {
					ui.Text(c, "Keep location data").FontSize(13)
					ui.Text(c, "Off by default for privacy").FontSize(10).TextColor(t.TextMuted)
				})
			})
		})

		ui.Spacer(c).Grow(1)

		if st.phase == phaseRunning {
			ui.Column(c).Gap(10).Children(func() {
				ui.Progress(c, progressValue(st.counts)).FillWidth()
				ui.Text(c, st.status).FontSize(12).TextColor(t.TextMuted).SingleLine()
				ui.Row(c).Gap(8).Children(func() {
					if st.paused {
						if ui.PrimaryButton(c, "Resume").Clicked() {
							a.Resume()
						}
					} else if ui.Button(c, "Pause").Clicked() {
						a.Pause()
					}
					if ui.Button(c, "Cancel").Clicked() {
						a.Cancel()
					}
				})
			})
		} else {
			count := st.counts.Remaining + st.counts.Failed + st.counts.Canceled
			label := fmt.Sprintf("Export %d photo(s)", count)
			if count == 0 {
				label = "Export"
			}
			btn := ui.PrimaryButton(c, label).FillWidth()
			if count == 0 || st.outputDir == "" {
				btn.Disabled(true)
			}
			if btn.Clicked() {
				a.StartExport()
			}
			if st.counts.Done > 0 && count == 0 {
				ui.Text(c, fmt.Sprintf("%d photo(s) exported", st.counts.Done)).
					FontSize(12).TextColor(t.Success)
			}
		}
	})
}

func progressValue(c model.Counts) float64 {
	if c.Total == 0 {
		return 0
	}
	return float64(c.Done+c.Skipped+c.Failed+c.Canceled) / float64(c.Total)
}

func (a *App) main(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Column(c).Fill().Grow(1).Padding(20).Gap(12).Children(func() {
		if st.banner != "" {
			ui.Row(c).FillWidth().Padding(12, 14).Radius(10).Background(t.Surface).Gap(10).Children(func() {
				ui.Text(c, st.banner).FontSize(12).TextColor(t.Warning).Grow(1)
			})
		}
		if st.status != "" && st.phase != phaseRunning {
			ui.Row(c).FillWidth().Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, st.status).FontSize(13).TextColor(t.TextMuted).Grow(1)
				if st.counts.Done > 0 && !st.running && !st.scanning &&
					ui.Button(c, fmt.Sprintf("Clear exported (%d)", st.counts.Done)).Clicked() {
					a.ClearExported()
				}
			})
		}
		if st.phase == phaseResults {
			a.results(c, st)
		}
		a.photoGrid(c, st)
	})
}

func (a *App) results(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Row(c).FillWidth().Padding(14, 16).Radius(12).Background(t.Surface).
		Gap(18).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Gap(3).Children(func() {
			ui.Text(c, "Export finished").FontSize(16).Bold()
			ui.Text(c, fmt.Sprintf("%d exported • %d skipped • %d failed",
				st.counts.Done, st.counts.Skipped, st.counts.Failed)).
				FontSize(12).TextColor(t.TextMuted)
		})
		ui.Spacer(c).Grow(1)
		if st.counts.Failed > 0 || st.counts.Canceled > 0 {
			if ui.Button(c, "Retry failed").Clicked() {
				a.RetryFailed()
			}
		}
		if st.counts.Done > 0 && ui.Button(c, "Open folder").Clicked() {
			a.OpenOutput()
		}
		if ui.Button(c, "New batch").Clicked() {
			go a.confirmClearBatch()
		}
	})
}

func (a *App) photoGrid(c *ui.Context, st viewState) {
	t := c.Theme()
	if len(st.items) == 0 {
		ui.Column(c).Fill().Center().Children(func() {
			ui.Text(c, "No supported photos").FontSize(16).TextColor(t.TextMuted)
		})
		return
	}
	a.grid.Key = func(i int) any { return st.items[i].ID }
	a.grid.Label = func(i int) string { return st.items[i].Name }
	a.grid.Selected = &a.selected
	ui.GridView(c, &a.grid, len(st.items), 168, 190, func(i int) {
		item := st.items[i]
		card := ui.Column(c).Padding(8).Gap(6).Radius(10).Background(t.Surface).
			Border(1, t.Border)
		if i == a.selected {
			card.Border(2, t.Accent)
		}
		if card.Hovered() {
			card.Background(t.SurfaceHover)
		}
		card.Children(func() {
			thumb := ui.Box(c).FillWidth().Height(132).Radius(7).Background(t.Background).Clip()
			thumb.Children(func() {
				if bmp := a.bitmap(item); bmp != nil {
					ui.Image(c, bmp).Fill().Fit(ui.Contain)
				} else {
					ui.Column(c).Fill().Center().Children(func() {
						ui.Text(c, "RAW").FontSize(12).TextColor(t.TextMuted)
					})
				}
			})
			ui.Text(c, item.Name).FontSize(12).SingleLine().Ellipsis("…")
			ui.Text(c, statusLabel(item)).FontSize(10).TextColor(statusColor(t, item))
		})
		if card.DoubleClicked() {
			if item.OutputPath != "" {
				a.OpenFile(item.OutputPath)
			}
		}
	}).Grow(1)
}

func (a *App) bitmap(item model.Item) *ui.Bitmap {
	if len(item.Thumbnail) == 0 {
		return nil
	}
	if bmp := a.bitmaps[item.ID]; bmp != nil && a.thumbSize[item.ID] == len(item.Thumbnail) {
		return bmp
	}
	bmp, err := ui.DecodeBitmap(item.Thumbnail)
	if err != nil {
		return nil
	}
	a.bitmaps[item.ID] = bmp
	a.thumbSize[item.ID] = len(item.Thumbnail)
	return bmp
}

func statusLabel(item model.Item) string {
	switch item.Status {
	case model.StatusDone:
		if item.Compressed {
			if item.Rotated {
				return "Size limited • upright"
			}
			return "Size limited"
		}
		if item.Rotated {
			return "Exported • upright"
		}
		return "Exported"
	case model.StatusSkipped:
		return "Skipped • " + shortReason(item.Message)
	case model.StatusFailed:
		return "Failed • " + shortReason(item.Message)
	case model.StatusInspecting:
		return "Reading metadata…"
	case model.StatusExtracting:
		return "Extracting JPEG…"
	case model.StatusRotating:
		return "Rotating losslessly…"
	case model.StatusVerifying:
		return "Verifying…"
	case model.StatusCanceled:
		return "Canceled"
	default:
		return "Ready"
	}
}

func shortReason(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 46 {
		return s[:43] + "…"
	}
	return s
}

func statusColor(t *ui.Theme, item model.Item) ui.Color {
	switch item.Status {
	case model.StatusDone:
		return t.Success
	case model.StatusSkipped, model.StatusCanceled:
		return t.TextMuted
	case model.StatusFailed:
		return t.Danger
	default:
		return t.TextMuted
	}
}

func (a *App) footer(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Row(c).FillWidth().Padding(24, 8).BorderWidth(1, 0, 0, 0).BorderColor(t.Border).
		Children(func() {
			ui.Text(c, "Originals are read-only • lossless orientation by default • everything stays on this computer").
				FontSize(10).TextColor(t.TextMuted)
			ui.Spacer(c).Grow(1)
			if st.counts.Total > 0 {
				ui.Text(c, fmt.Sprintf("%d / %d complete", st.counts.Done+st.counts.Skipped+st.counts.Failed+st.counts.Canceled, st.counts.Total)).
					FontSize(10).TextColor(t.TextMuted)
			}
		})
}

func (a *App) loadIcon() *ui.Bitmap {
	if a.iconDone {
		return a.icon
	}
	return nil
}

func (a *App) setCorrectOrientation(v bool) {
	a.mu.Lock()
	a.correctOrientation = v
	if a.job != nil {
		a.job.Settings.CorrectOrientation = v
	}
	a.mu.Unlock()
}

func (a *App) setPreserveLocation(v bool) {
	a.mu.Lock()
	a.preserveLocation = v
	if a.job != nil {
		a.job.Settings.PreserveLocation = v
	}
	a.mu.Unlock()
}

func (a *App) setLimitOutputSize(v bool) {
	a.mu.Lock()
	a.limitOutputSize = v
	if a.job != nil {
		a.job.Settings.LimitOutputSize = v
	}
	a.mu.Unlock()
}

func (a *App) setCompressionLimitMB(v float64) {
	if v < 0.5 {
		v = 0.5
	} else if v > 20 {
		v = 20
	}
	a.mu.Lock()
	a.maxOutputBytes = int64(v*10+0.5) * 100_000
	if a.job != nil {
		a.job.Settings.MaxOutputBytes = a.maxOutputBytes
	}
	a.mu.Unlock()
}
