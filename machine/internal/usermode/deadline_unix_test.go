//go:build darwin || linux

package usermode

import "time"

func deadlineShortlyFromNow() time.Time { return time.Now().Add(2 * time.Second) }
