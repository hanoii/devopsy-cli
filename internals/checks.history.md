History, not rules: the root's Checks section and `internals/e2e.md` win where they differ.

- "Every change carries its test" came from the reviews of October 2026, which found bugs in code that worked when written and broke under a later change nobody re-tested; the accepted cost is that the fakes grow with the code.
- Running the checks in Docker on Linux measured about three times faster than on macOS (50 s against 150 s): macOS checks each new executable on its first run, and the tests write thousands of fake scripts.
- The 26 `sleep 1.1` between releases in the tests went on 2026-10-24, once the server picked release ids; no sleep is needed for ids, locks or files since.
- Unit tests went parallel on 2026-10-24: on Linux with 4 CPUs, as CI's runners, 200 s serial became 50 s with all CPUs busy; on macOS (10 CPUs) 410 s became 145 s, most of what is left being the first exec of each new script (macOS checks every new executable), so the next lever there is fewer new scripts, the setups' fakes written once and copied only for a test that changes them.
- `TestWaitBeatWithoutWatcher` takes 20 s by design: two beats past the longest quiet time.
- The race detector runs only over the packages that test in process because nothing builds otherwise with `-race`; under it the script tests of `cmd/devopsy` and `internal/remote` are several times slower and fail on time.
- The end-to-end lanes are processes, not `t.Parallel` (decided 2026-10-24): the tests keep their globals (`op`, `srv`, `server`) and everything they change (iptables, `tc`, the login shell, a Ctrl-C) belongs to their lane's containers, so no test needs to know about lanes; `resetLane` came when a Ctrl-C reached an earlier test's devopsy once the order changed.
- Measured on OrbStack (10 CPUs): 40 minutes serial, 11 with four lanes; the run takes about the larger of the slowest test (`TestSyncDestroy`, 144 s) and the tests' total over the lanes (about 2400 s over 4), so the slowest-first order keeps a long test from starting last.
- The test server's dash login shell found the watcher signalling only the script, where devopsy-server's bash hid it.
- The deploy user's home became a tmpfs on 2026-10-27: on OrbStack's btrfs the container's overlay cloned by reflinks, on CI's ext4 not, so a sync ran in another order locally than in CI and six tests failed only locally.
- The Docker Hub mirror came with the lanes, so four lanes pull from Docker Hub no more than one did.
