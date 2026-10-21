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
// They need Docker (Docker Desktop, OrbStack, Linux, GitHub's runners)
// and the network (the image, then images the test projects pull).
// E2E_KEEP=1 leaves the containers for a look afterwards.
package e2e
