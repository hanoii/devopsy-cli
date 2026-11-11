//go:build !linux

package main

// endUIDKill runs only on a Linux server, in remote.EndUserArgs' container.
func endUIDKill(string) int {
	return fail("the end of a user's processes runs on Linux only")
}
