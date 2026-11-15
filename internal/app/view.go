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
	confirmClear       bool
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
		confirmClear:       a.confirmClear,
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
	if st.confirmClear && !st.running && !st.scanning && c.Shortcut(0, ui.KeyEscape) {
		a.cancelClearBatch()
	}
	if c.Shortcut(ui.Cmd, ui.KeyE) && !st.running {
		a.StartExport()
	}

	ui.Column(c).Fill().Background(t.Background).Children(func() {
		a.header(c, st)
		ui.Row(c).Fill().Grow(1).MinHeight(0).Children(func() {
			if st.phase == phaseEmpty || len(st.items) == 0 {
				a.welcome(c, st)
				return
			}
			a.sidebar(c, st)
			a.main(c, st)
		})
	})
}

func (a *App) header(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Row(c).FillWidth().Height(68).Padding(10, 20).Gap(12).
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
			if st.phase != phaseRunning && len(st.items) > 0 {
				if ui.Button(c, "Add photos").Clicked() {
					a.ChoosePhotos()
				}
				if ui.Button(c, "Add folder").Clicked() {
					a.ChooseFolder()
				}
			}
		})
}

func (a *App) welcome(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Column(c).Fill().Grow(1).Padding(24).Center().Gap(18).Children(func() {
		zone := ui.Column(c).Width(680).Height(380).Center().Gap(12).Padding(28).
			Radius(18).Border(2, t.Border).BorderStyle(ui.BorderDashed)
		over := zone.FileDragOver()
		if over {
			zone.Border(2, t.Accent).Background(t.SurfaceHover)
		}
		if files := zone.DroppedFiles(); files != nil {
			a.AddPaths(files)
		}
		zone.Children(func() {
			ui.Text(c, "Drop photos here").FontSize(28).Bold().TextAlign(ui.Center)
			ui.Text(c, "RAW or JPEG files, or a folder full of photos").FontSize(15).TextColor(t.TextMuted).TextAlign(ui.Center)
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
			if st.status != "" {
				ui.Text(c, st.status).FontSize(12).TextColor(t.TextMuted).TextAlign(ui.Center).SingleLine().Ellipsis("…")
			}
			if st.banner != "" {
				ui.Text(c, st.banner).FontSize(12).TextColor(t.Warning).TextAlign(ui.Center).SingleLine().Ellipsis("…")
			}
			ui.Row(c).FillWidth().Gap(16).Children(func() {
				a.promise(c, "Originals untouched", "Read-only access")
				a.promise(c, "Lossless by default", "JPEG data preserved")
				a.promise(c, "100% local", "No uploads or accounts")
			})
		})
	})
}

func (a *App) promise(c *ui.Context, title, detail string) {
	t := c.Theme()
	ui.Column(c).Grow(1).MinWidth(0).Gap(3).Children(func() {
		ui.Text(c, title).FontSize(12).Bold().SingleLine().Ellipsis("…")
		ui.Text(c, detail).FontSize(11).TextColor(t.TextMuted).SingleLine().Ellipsis("…")
	})
}

func (a *App) sidebar(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Column(c).Width(332).Shrink(0).HeightPercent(100).MinHeight(0).
		BorderWidth(0, 1, 0, 0).BorderColor(t.Border).Children(func() {
		ui.Scroll(c).Grow(1).MinHeight(0).ClipX().Padding(8, 16).Gap(8).Children(func() {
			ui.Column(c).FillWidth().Padding(10).Radius(12).Background(t.Surface).ClipX().Gap(10).Children(func() {
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "Output folder").FontSize(12).Bold().Grow(1).MinWidth(0)
					if !st.running && !st.scanning && ui.Button(c, "Change").Clicked() {
						a.ChooseOutput()
					}
				})
				path := st.outputDir
				if path == "" {
					path = "Choose a folder for the JPEGs"
				}
				ui.Text(c, path).FontSize(13).SingleLine().Ellipsis("…").MinWidth(0)
				if st.limitOutputSize {
					folder := sizeLimitSubdirectory(int64(st.compressionLimitMB * 1_000_000))
					ui.Text(c, "Compressed copies go in /"+folder).FontSize(10).TextColor(t.TextMuted).MinWidth(0)
				}
			})

			ui.Column(c).FillWidth().Padding(10).Radius(12).Background(t.Surface).ClipX().Gap(12).Children(func() {
				ui.Column(c).Gap(3).Children(func() {
					ui.Text(c, "Photo options").FontSize(13).Bold()
					ui.Text(c, "Lossless unless the size limit is on.").FontSize(10).TextColor(t.TextMuted)
				})

				correct := st.correctOrientation
				ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
					if ui.Switch(c, &correct).Changed() && !st.running {
						a.setCorrectOrientation(correct)
					}
					ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
						ui.Text(c, "Correct orientation").FontSize(13).SingleLine().Ellipsis("…")
						ui.Text(c, "Lossless rotation when possible").FontSize(10).TextColor(t.TextMuted).SingleLine().Ellipsis("…")
					})
				})

				limitOutput := st.limitOutputSize
				ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
					if ui.Switch(c, &limitOutput).Changed() && !st.running {
						a.setLimitOutputSize(limitOutput)
					}
					ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
						ui.Text(c, "Limit JPEG file size").FontSize(13).SingleLine().Ellipsis("…")
						ui.Text(c, "Lossy; original dimensions").FontSize(10).TextColor(t.TextMuted).SingleLine().Ellipsis("…")
					})
				})
				if st.limitOutputSize {
					limitMB := st.compressionLimitMB
					ui.Column(c).FillWidth().Padding(10).Radius(8).Background(t.Background).ClipX().Gap(8).Children(func() {
						ui.Text(c, "Maximum size (MB)").FontSize(11).TextColor(t.TextMuted)
						input := ui.NumberInput(c, &limitMB, 0.5, 20, 0.1).Label("Maximum JPEG size in megabytes")
						if input.Changed() && !st.running {
							a.setCompressionLimitMB(limitMB)
						}
						slider := ui.StepSlider(c, &limitMB, 0.5, 20, 0.1).FillWidth().Label("Maximum JPEG size in megabytes")
						if slider.Changed() && !st.running {
							a.setCompressionLimitMB(limitMB)
						}
						ui.Row(c).FillWidth().Children(func() {
							ui.Text(c, "0.5 MB").FontSize(10).TextColor(t.TextMuted)
							ui.Spacer(c).Grow(1)
							ui.Text(c, "20 MB").FontSize(10).TextColor(t.TextMuted)
						})
					})
				}

				gps := st.preserveLocation
				ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
					if ui.Switch(c, &gps).Changed() && !st.running {
						a.setPreserveLocation(gps)
					}
					ui.Column(c).Grow(1).MinWidth(0).Gap(1).Children(func() {
						ui.Text(c, "Keep location data").FontSize(13).SingleLine().Ellipsis("…")
						ui.Text(c, "Off by default for privacy").FontSize(10).TextColor(t.TextMuted).SingleLine().Ellipsis("…")
					})
				})
			})
		})

		a.actionDock(c, st)
	})
}

func (a *App) actionDock(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Column(c).FillWidth().Shrink(0).Padding(12, 16).Gap(8).
		BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Children(func() {
		if st.phase == phaseRunning {
			ui.Progress(c, progressValue(st.counts)).FillWidth()
			ui.Text(c, st.status).FontSize(11).TextColor(t.TextMuted).SingleLine().Ellipsis("…")
			ui.Row(c).FillWidth().Gap(8).Children(func() {
				if st.paused {
					if ui.PrimaryButton(c, "Resume").Grow(1).MinWidth(0).Clicked() {
						a.Resume()
					}
				} else if ui.Button(c, "Pause").Grow(1).MinWidth(0).Clicked() {
					a.Pause()
				}
				if ui.Button(c, "Cancel").Grow(1).MinWidth(0).Clicked() {
					a.Cancel()
				}
			})
			return
		}

		if st.phase == phaseResults {
			ui.Row(c).FillWidth().Gap(8).Children(func() {
				if st.counts.Done > 0 {
					if ui.Button(c, "Open folder").Grow(1).MinWidth(0).Clicked() {
						a.OpenOutput()
					}
				}
				if st.counts.Failed > 0 || st.counts.Canceled > 0 {
					if ui.PrimaryButton(c, "Retry failed").Grow(1).MinWidth(0).Clicked() {
						a.RetryFailed()
					}
				}
			})
			return
		}

		count := st.counts.Remaining + st.counts.Failed + st.counts.Canceled
		label := fmt.Sprintf("Export %d photos", count)
		if count == 0 {
			label = "Export photos"
		}
		button := ui.PrimaryButton(c, label).FillWidth()
		if count == 0 || st.outputDir == "" || st.scanning {
			button.Disabled(true)
		}
		if button.Clicked() {
			a.StartExport()
		}
		if st.scanning {
			ui.Text(c, "Scanning photos…").FontSize(10).TextColor(t.TextMuted)
		} else if st.outputDir == "" {
			ui.Text(c, "Choose an output folder to continue.").FontSize(10).TextColor(t.TextMuted)
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
	ui.Column(c).Fill().Grow(1).MinWidth(0).Padding(20).Gap(12).Children(func() {
		if st.banner != "" {
			ui.Row(c).FillWidth().Padding(10, 12).Radius(10).Background(t.Surface).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(7, 7).Radius(4).Background(t.Warning)
				ui.Text(c, st.banner).FontSize(12).TextColor(t.Warning).Grow(1).MinWidth(0)
			})
		}
		a.batchToolbar(c, st)
		if st.phase == phaseResults {
			a.results(c, st)
		}
		a.photoGrid(c, st)
	})
}

func (a *App) batchToolbar(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Row(c).FillWidth().Padding(9, 12).Radius(10).Background(t.Surface).
		Gap(8).AlignItems(ui.Center).Children(func() {
		if st.confirmClear && !st.running && !st.scanning {
			ui.Text(c, "Clear list? Files stay on disk.").
				FontSize(12).TextColor(t.TextMuted).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
			if ui.Button(c, "Cancel").Shrink(0).Clicked() {
				a.cancelClearBatch()
			}
			if ui.Button(c, "Clear batch").Shrink(0).Clicked() {
				a.confirmClearBatch()
			}
			return
		}
		status := st.status
		if st.running {
			completed := st.counts.Done + st.counts.Skipped + st.counts.Failed + st.counts.Canceled
			status = fmt.Sprintf("Exporting • %d / %d complete", completed, st.counts.Total)
		} else if status == "" && st.counts.Total > 0 {
			status = fmt.Sprintf("%d photos ready", st.counts.Total)
		}
		if st.phase == phaseResults {
			status = ""
		}
		if status == "" {
			ui.Spacer(c).Grow(1)
		} else {
			ui.Text(c, status).FontSize(12).TextColor(t.TextMuted).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		}
		if !st.running && !st.scanning {
			if st.counts.Done > 0 && ui.Button(c, fmt.Sprintf("Clear exported (%d)", st.counts.Done)).Shrink(0).Clicked() {
				a.ClearExported()
			}
			if st.counts.Total > 0 && ui.Button(c, "Clear list").Shrink(0).Clicked() {
				a.requestClearBatch()
			}
		}
	})
}

func (a *App) results(c *ui.Context, st viewState) {
	t := c.Theme()
	ui.Row(c).FillWidth().Padding(12, 14).Radius(10).Background(t.Surface).
		Gap(14).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Gap(3).Children(func() {
			ui.Text(c, "Export finished").FontSize(16).Bold()
			ui.Text(c, fmt.Sprintf("%d exported • %d skipped • %d failed",
				st.counts.Done, st.counts.Skipped, st.counts.Failed)).
				FontSize(12).TextColor(t.TextMuted)
		})
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
					format := "RAW"
					if model.IsJPEGName(item.Source) {
						format = "JPEG"
					}
					ui.Column(c).Fill().Center().Children(func() {
						ui.Text(c, format).FontSize(12).TextColor(t.TextMuted)
					})
				}
			})
			ui.Text(c, item.Name).FontSize(12).SingleLine().Ellipsis("…").MinWidth(0)
			ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(6, 6).Radius(3).Background(statusColor(t, item))
				ui.Text(c, statusLabel(item)).FontSize(10).TextColor(statusColor(t, item)).
					SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
			})
		})
		if card.DoubleClicked() {
			if item.OutputPath != "" {
				a.OpenFile(item.OutputPath)
			}
		}
	}).Grow(1).MinWidth(0)
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
