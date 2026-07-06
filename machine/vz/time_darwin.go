//go:build darwin

package vz

import "time"

// timerSeconds returns a short-lived timer; extracted so waitStopped stays
// readable.
func timerSeconds(seconds int) *time.Timer {
	return time.NewTimer(time.Duration(seconds) * time.Second)
}
