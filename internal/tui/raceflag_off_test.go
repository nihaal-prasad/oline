//go:build !race

package tui

// raceDetectorOn is false in an ordinary (non -race) build, so the terminal-teardown
// tests run normally. See raceflag_on_test.go for why they skip under -race.
const raceDetectorOn = false
