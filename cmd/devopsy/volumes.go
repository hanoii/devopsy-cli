package main

import (
	"cmp"
	"context"
	"errors"
	"os/exec"
	"slices"
	"sort"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
)

// A service with data's named volumes are its data as its entries of mnt
// are (cli.SyncService.Volumes): a copy writes into them as files, as
// root in devopsy's util image, mounted by name. Docker makes a volume a
// container mounts when it is missing, unlabeled, which compose then
// warns about on every up ("not created by Docker Compose"), so devopsy
// makes a missing one itself first, as compose would (ensureVolumes).

// volumeExists reports whether Docker has the volume name.
func volumeExists(ctx context.Context, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "volume", "inspect", "--format", "{{.Name}}", name)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// "Error: No such volume" (the CLI), "no such volume" (the daemon).
		if strings.Contains(strings.ToLower(stderr.String()), "no such volume") {
			return false, nil
		}
		return false, errors.New("docker volume inspect " + name + ": " + cmp.Or(strings.TrimSpace(stderr.String()), err.Error()))
	}
	return true, nil
}

// copiedVolumes are the volumes of services a copy carries as files
// (cli.CopyVolumes), each once, by key.
func copiedVolumes(services []cli.SyncService) []cli.Volume {
	var vols []cli.Volume
	for _, s := range services {
		if s.Copy != cli.CopyVolumes {
			continue
		}
		for _, v := range s.Volumes {
			if !slices.ContainsFunc(vols, func(x cli.Volume) bool { return x.Key == v.Key }) {
				vols = append(vols, v)
			}
		}
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Key < vols[j].Key })
	return vols
}

// ensureVolumes makes each of vols that Docker lacks, as compose would:
// its driver and labels, and compose's own two, the project's and the
// volume's key, so compose takes it for its own (checked on compose
// v5.5.0: no warning, where an unlabeled one gets one on every up).
// project is the environment's compose project, at cwd. One with options
// (driver_opts: a share's, its password in them) is made by the project's
// own compose, for the services that mount it (create, which starts none),
// through Docker's API: on docker volume create's command line, which
// every user of the server reads in /proc, its options would show.
func ensureVolumes(ctx context.Context, cwd, project string, vols []cli.Volume, services []cli.SyncService) error {
	for _, v := range vols {
		ok, err := volumeExists(ctx, v.Name)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		if len(v.Opts) > 0 {
			var mounts []string
			for _, s := range services {
				// Its mounts, never s.Volumes, which leaves out what devopsy.data.exclude
				// names: an owned volume excluded from copies is made all the same.
				if slices.ContainsFunc(s.VolumeMounts, func(m cli.VolumeMount) bool { return m.Key == v.Key }) {
					mounts = append(mounts, s.Name)
				}
			}
			if err := composeContext(ctx, cwd, errOut, append([]string{"create", "--no-build", "--no-recreate"}, mounts...)...); err != nil {
				return errors.New("making the volume " + v.Name + ": " + err.Error())
			}
			if ok, err := volumeExists(ctx, v.Name); err != nil || !ok {
				return errors.New("making the volume " + v.Name + ": compose made no such volume")
			}
			continue
		}
		args := []string{"volume", "create", "--label", "com.docker.compose.project=" + project, "--label", "com.docker.compose.volume=" + v.Key}
		if v.Driver != "" {
			args = append(args, "--driver", v.Driver)
		}
		for _, k := range sortedKeys(v.Labels) {
			args = append(args, "--label", k+"="+v.Labels[k])
		}
		if _, err := dockerOut(ctx, append(args, v.Name)...); err != nil {
			return errors.New("making the volume " + v.Name + ": " + err.Error())
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
