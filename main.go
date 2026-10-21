package main

import (
	"log"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"rawdrop/internal/app"
)

func main() {
	a := app.New()
	mygo.App.WhenReady(func() {
		win := mygo.NewWindow(mygo.WindowOptions{
			Title:     "RawDrop — RAW to full-resolution JPEG",
			Width:     1180,
			Height:    780,
			MinWidth:  880,
			MinHeight: 620,
			StateKey:  "main",
			Content:   ui.View(a.View),
		})
		a.Attach(win)
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
