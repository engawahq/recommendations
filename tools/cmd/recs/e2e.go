package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/engawa/recommendations/tools/internal/recs"
)

// measurement is measurements/<capability>/<upstream>.json, or the cluster
// baseline in measurements/<env>.json. Environments are keyed
// "<env>-<server version>".
type measurement struct {
	Component    string                 `json:"component,omitempty"`
	Environments map[string]environment `json:"environments"`
}

type environment struct {
	Profile       string            `json:"profile,omitempty"`
	IdleMemoryMiB float64           `json:"idleMemoryMiB"`
	IdleCPUm      float64           `json:"idleCPUm"`
	PodsMiB       float64           `json:"podsMiB,omitempty"`      // baseline only
	ProcessesMiB  float64           `json:"processesMiB,omitempty"` // baseline only
	Versions      map[string]string `json:"versions,omitempty"`
	MeasuredAt    string            `json:"measuredAt"`
}

func e2e(fs *flag.FlagSet, cat *recs.Catalog) error {
	env := fs.String("env", "", "an e2e cluster in config/kubernetes.yaml")
	write := fs.Bool("write", false, "update measurements/ for passing components")
	previous := fs.String("previous", "", "previous release's index.json, for upgrade tests")
	baseline := fs.Bool("baseline", false, "measure the empty cluster instead of components")
	settle := fs.Duration("settle", time.Minute, "wait after install before measuring")
	fs.Parse(os.Args[2:])

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	profile := cfg.E2E[*env].Profile
	if profile == "" {
		return fmt.Errorf("unknown -env %q", *env)
	}
	out, err := recs.Run(nil, "kubectl", "version", "-o", "json")
	if err != nil {
		return err
	}
	var version struct{ ServerVersion struct{ GitVersion string } }
	if err := json.Unmarshal([]byte(out), &version); err != nil {
		return err
	}
	key := *env + "-" + version.ServerVersion.GitVersion
	if *baseline {
		return measureBaseline(*env, key, *write)
	}

	var e2eCfg struct {
		Overrides map[string]map[string]map[string]string `yaml:"overrides"`
	}
	if err := recs.ReadYAML("config/e2e.yaml", &e2eCfg); err != nil {
		return err
	}
	t := &recs.Tester{Catalog: cat, Profile: profile, Overrides: e2eCfg.Overrides[*env], Settle: *settle}
	if *previous != "" {
		if t.Previous, err = readPrevious(*previous); err != nil {
			return err
		}
	}
	comps, err := selectComponents(cat, fs.Args())
	if err != nil {
		return err
	}

	var results []recs.Result
	for _, c := range comps {
		if c.Defaults[profile] == "unsupported" || !c.RunsOn(*env) {
			continue
		}
		slog.Info("testing", "component", c.ID, "env", key)
		res := t.Test(c.ID)
		results = append(results, res)
		if res.Err == nil && *write {
			if err := writeMeasurement(c, key, profile, res); err != nil {
				return err
			}
		}
	}

	fmt.Printf("\n%s (%s):\n", key, profile)
	var errs []error
	for _, r := range results {
		status := fmt.Sprintf("PASS %4.0f MiB %3.0fm CPU", r.IdleMemoryMiB, r.IdleCPUm)
		if r.Err != nil {
			status = "FAIL " + r.Err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", r.ID, r.Err))
		}
		if r.UpgradedFrom != "" {
			status += " (upgraded from " + r.UpgradedFrom + ")"
		}
		fmt.Printf("  %-48s %s\n", r.ID, status)
	}
	return errors.Join(errs...)
}

func readPrevious(path string) (map[string]*recs.Component, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx struct{ Components []*recs.Component }
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	out := map[string]*recs.Component{}
	for _, c := range idx.Components {
		out[c.ID] = c
	}
	return out, nil
}

func measurementPath(id string) string { return filepath.Join("measurements", id+".json") }

func writeMeasurement(c *recs.Component, key, profile string, res recs.Result) error {
	m := readMeasurement(measurementPath(c.ID))
	m.Component = c.ID
	versions := map[string]string{}
	for _, r := range c.Releases {
		if r.Version != "" {
			versions[r.Name] = r.Version
		}
	}
	m.Environments[key] = environment{
		Profile:       profile,
		IdleMemoryMiB: math.Round(res.IdleMemoryMiB),
		IdleCPUm:      math.Round(res.IdleCPUm),
		Versions:      versions,
		MeasuredAt:    time.Now().UTC().Format(time.DateOnly),
	}
	return writeJSON(measurementPath(c.ID), m)
}

// measureBaseline records the empty cluster: its pods plus, for a
// distribution running as a systemd service named after the env (k3s,
// kubesolo), the service itself (API server, containerd, shims), which pod
// metrics miss.
func measureBaseline(env, key string, write bool) error {
	pods, cpu, err := recs.SampleUsage(nil)
	if err != nil {
		return err
	}
	var procs float64
	if cgroup := "/sys/fs/cgroup/system.slice/" + env + ".service"; dirExists(cgroup) {
		if procs, err = unreclaimableMiB(cgroup); err != nil {
			return err
		}
	}
	slog.Info("baseline", "env", key, "podsMiB", int(pods), "serviceMiB", int(procs))
	if !write {
		return nil
	}
	path := measurementPath(env)
	m := readMeasurement(path)
	m.Environments[key] = environment{
		IdleMemoryMiB: math.Round(pods + procs),
		IdleCPUm:      math.Round(cpu),
		PodsMiB:       math.Round(pods),
		ProcessesMiB:  math.Round(procs),
		MeasuredAt:    time.Now().UTC().Format(time.DateOnly),
	}
	return writeJSON(path, m)
}

// unreclaimableMiB returns a cgroup's anonymous and kernel memory: what it
// needs resident. Page cache (image layers, the k3s binary) is reclaimable.
func unreclaimableMiB(cgroup string) (float64, error) {
	stat, err := os.ReadFile(filepath.Join(cgroup, "memory.stat"))
	if err != nil {
		return 0, err
	}
	var bytes float64
	for _, line := range strings.Split(string(stat), "\n") {
		k, v, _ := strings.Cut(line, " ")
		if k == "anon" || k == "kernel" {
			n, _ := strconv.ParseFloat(v, 64)
			bytes += n
		}
	}
	return bytes / (1 << 20), nil
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func readMeasurement(path string) measurement {
	m := measurement{}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &m)
	}
	if m.Environments == nil {
		m.Environments = map[string]environment{}
	}
	return m
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
