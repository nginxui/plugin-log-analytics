//go:build !race

package service

// raceEnabled reports whether the test binary was built with the race
// detector, so performance-oriented tests can scale their workloads down.
const raceEnabled = false
