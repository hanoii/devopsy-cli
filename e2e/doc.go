// Package e2e holds devopsy's end-to-end tests: real servers (privileged
// Debian containers with their own Docker and sshd, Dockerfile) and an
// operator container on one Docker network, devopsy built from this
// checkout in both. Behind the e2e build tag, so plain go test skips
// them:
//
//	go test -tags e2e -count=1 -v -timeout 40m ./e2e
//
// Drops count in heartbeat beats of E2E_BEAT seconds (default 1);
// E2E_BEAT=5 runs them with the real timeouts.
//
// E2E_LANES (default 4) servers run tests at once: each lane is a
// network, an operator and a server (a second one when a test needs it),
// and runs one test at a time, each in a process of its own, the
// slowest first by their last times (in the user's cache directory).
// E2E_LANES=1 runs them all in one process, one after another.
// E2E_SHARD=<k>/<n> runs the k-th of n shares of the tests, by name,
// for CI's jobs. The servers pull through a mirror of Docker Hub whose
// cache, the volume devopsy-e2e-mirror, outlives the run.
//
// They need Docker (Docker Desktop, OrbStack, Linux, GitHub's runners)
// and the network (the image, then images the test projects pull).
// E2E_KEEP=1 leaves the containers for a look afterwards, all labeled
// devopsy-e2e=<the run's process id>.
package e2e
