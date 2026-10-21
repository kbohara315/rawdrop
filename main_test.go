package main

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"rawdrop/internal/app"
)

// The welcome screen builds without a window, which catches layout and API
// mistakes in the common empty state before the app is launched.
func TestWelcomeViewRenders(t *testing.T) {
	a := app.New()
	tester := ui.NewTester(a.View, 1100, 760)
	tester.Frame()
	if !tester.HasText("Drop RAW photos here") {
		t.Fatal("welcome screen did not render its drop zone")
	}
}
