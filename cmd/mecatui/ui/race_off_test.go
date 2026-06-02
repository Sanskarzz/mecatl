//go:build !race

package ui

// raceEnabled is false in a normal (non-race) test build, so the teatest cases
// keep their tight wall-clock deadlines. See race_on_test.go for the rationale
// behind scaling them up under `-race`.
const raceEnabled = false
