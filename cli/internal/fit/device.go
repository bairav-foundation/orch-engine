// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package fit

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Device is where a model would run and how much memory it can use.
type Device struct {
	Kind   string // "nvidia", "apple", "cpu", "cluster" or "manual"
	Name   string // "NVIDIA GeForce RTX 4090", "Apple M3 Pro (36 GB unified)", ...
	Total  int64  // physical memory of the device (per GPU)
	Usable int64  // what a model can realistically have
	Count  int    // GPUs of this kind (1 for Macs and CPUs)
}

// Describe is a one-line summary for the CLI.
func (d Device) Describe() string {
	switch d.Kind {
	case "manual":
		return fmt.Sprintf("%s (from --memory)", GB(d.Usable))
	case "apple":
		return fmt.Sprintf("%s, about %s usable by a model (macOS keeps the rest)", d.Name, GB(d.Usable))
	case "cpu":
		return fmt.Sprintf("%s, no GPU found: CPU only, about %s RAM for a model (slow above ~8B)", d.Name, GB(d.Usable))
	}
	n := ""
	if d.Count > 1 {
		n = fmt.Sprintf("%d × ", d.Count)
	}
	return fmt.Sprintf("%s%s (%s each)", n, d.Name, GB(d.Usable))
}

// Manual is a device from a size the user typed (--fits 24GB).
func Manual(size string) (Device, error) {
	b, err := ParseSize(size)
	if err != nil {
		return Device{}, err
	}
	return Device{Kind: "manual", Name: GB(b), Total: b, Usable: b, Count: 1}, nil
}

// runner runs a command and returns its stdout; replaced in tests.
type runner func(ctx context.Context, name string, args ...string) (string, error)

func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // fixed tool names and arguments
	return string(out), err
}

// Detect finds the local device: an NVIDIA GPU (nvidia-smi), else an
// Apple Silicon Mac (unified memory), else the CPU and system RAM.
func Detect(ctx context.Context) Device {
	return detect(ctx, execRunner, runtime.GOOS, runtime.GOARCH, readMemInfo)
}

func detect(ctx context.Context, run runner, goos, goarch string, memInfo func() int64) Device {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if out, err := run(ctx, "nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits"); err == nil {
		if d, ok := parseNvidiaSMI(out); ok {
			return d
		}
	}
	if goos == "darwin" {
		mem := int64(0)
		if out, err := run(ctx, "sysctl", "-n", "hw.memsize"); err == nil {
			mem, _ = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		}
		name := "Mac"
		if out, err := run(ctx, "sysctl", "-n", "machdep.cpu.brand_string"); err == nil && strings.TrimSpace(out) != "" {
			name = strings.TrimSpace(out)
		}
		if goarch == "arm64" && mem > 0 {
			// Metal lets the GPU use about two thirds of unified memory on
			// small Macs and three quarters on 64 GB and up.
			share := 0.67
			if mem >= 64*gib {
				share = 0.75
			}
			return Device{Kind: "apple", Name: fmt.Sprintf("%s (%s unified)", name, GB(mem)), Total: mem, Usable: int64(float64(mem) * share), Count: 1}
		}
		return cpuDevice(name, mem)
	}
	return cpuDevice(cpuName(), memInfo())
}

// cpuDevice leaves a quarter of RAM for the OS and everything else.
func cpuDevice(name string, mem int64) Device {
	return Device{Kind: "cpu", Name: fmt.Sprintf("%s (%s RAM)", name, GB(mem)), Total: mem, Usable: mem * 3 / 4, Count: 1}
}

// parseNvidiaSMI reads "NVIDIA GeForce RTX 4090, 24564" lines (MiB). With
// several GPUs it reports the smallest, since a model runs on one by
// default; the count says how many there are.
func parseNvidiaSMI(out string) (Device, bool) {
	var d Device
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, mib, ok := strings.Cut(line, ",")
		if !ok {
			continue
		}
		m, err := strconv.ParseInt(strings.TrimSpace(mib), 10, 64)
		if err != nil || m <= 0 {
			continue
		}
		b := m << 20
		if d.Count == 0 || b < d.Total {
			d.Name, d.Total = strings.TrimSpace(name), b
		}
		d.Count++
	}
	if d.Count == 0 {
		return Device{}, false
	}
	d.Kind = "nvidia"
	// The driver and CUDA context take a few hundred MB of every GPU.
	d.Usable = d.Total - gib/2
	return d, true
}

func readMemInfo() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "MemTotal:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return kb << 10
		}
	}
	return 0
}

func cpuName() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "CPU"
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "model name"); ok {
			if _, name, ok := strings.Cut(v, ":"); ok {
				return strings.TrimSpace(name)
			}
		}
	}
	return "CPU"
}
