package main

import (
	"errors"
	"strconv"
	"syscall"
)

// endUIDKill takes uid as its effective uid, its real and saved one noProcessUID, which drops
// its capabilities, and kills every process it may then signal: those whose real or saved uid
// is the user's (or noProcessUID's, which none has), never itself.
func endUIDKill(arg string) int {
	uid, err := strconv.Atoi(arg)
	if err != nil || uid <= 0 {
		return 2
	}
	if err := syscall.Setresuid(noProcessUID, uid, noProcessUID); err != nil {
		return failErr(err)
	}
	if err := syscall.Kill(-1, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return failErr(err)
	}
	return 0
}
