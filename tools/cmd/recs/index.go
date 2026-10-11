package main

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/engawa/recommendations/tools/internal/recs"
)

// indexVersion changes only on incompatible changes to index.json.
const indexVersion = 1

type budgetConfig struct {
	SingleNode struct {
		BudgetMiB    float64  `yaml:"budgetMiB"`
		Environments []string `yaml:"environments"`
	} `yaml:"singleNode"`
}

// index writes index.json: every component with its install order and
// measurements, recommended components per capability, the budget and the
// Kubernetes versions.
func index(fs *flag.FlagSet, cat *recs.Catalog) error {
	out := fs.String("out", "index.json", "output file")
	fs.Parse(os.Args[2:])
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var budget budgetConfig
	if err := recs.ReadYAML("config/budget.yaml", &budget); err != nil {
		return err
	}
	type component struct {
		*recs.Component
		InstallOrder map[string][]string    `json:"installOrder"`
		Measured     map[string]environment `json:"measured,omitempty"`
	}
	baselines := map[string]environment{}
	for _, env := range budget.SingleNode.Environments {
		maps.Copy(baselines, readMeasurement(measurementPath(env)).Environments)
	}
	capabilities := map[string]map[string][]string{}
	var comps []component
	for _, c := range cat.Components {
		ic := component{Component: c, InstallOrder: map[string][]string{}}
		for _, p := range recs.Profiles {
			if c.Defaults[p] == "unsupported" {
				continue
			}
			order, err := cat.InstallOrder([]string{c.ID}, p)
			if err != nil {
				return err
			}
			for _, o := range order {
				ic.InstallOrder[p] = append(ic.InstallOrder[p], o.ID)
			}
		}
		ic.Measured = readMeasurement(measurementPath(c.ID)).Environments
		comps = append(comps, ic)
		if capabilities[c.Capability] == nil {
			capabilities[c.Capability] = map[string][]string{}
		}
		for _, p := range recs.Profiles {
			if c.Defaults[p] == "recommended" {
				capabilities[c.Capability][p] = append(capabilities[c.Capability][p], c.ID)
			}
		}
	}
	return writeJSON(*out, map[string]any{
		"indexVersion": indexVersion,
		"profiles":     recs.Profiles,
		"kubernetes":   cfg.Versions,
		"budget":       budget,
		"baselines":    baselines,
		"capabilities": capabilities, // capability -> profile -> recommended IDs
		"components":   comps,
	})
}

// budget sums, for each environment, the measured idle memory of the empty
// cluster and every component recommended on single-node that runs on it, and
// fails above the budget.
func budget(fs *flag.FlagSet, cat *recs.Catalog) error {
	fs.Parse(os.Args[2:])
	var cfg budgetConfig
	if err := recs.ReadYAML("config/budget.yaml", &cfg); err != nil {
		return err
	}
	var errs []error
	for _, env := range cfg.SingleNode.Environments {
		base, ok := latest(readMeasurement(measurementPath(env)), env)
		if !ok {
			errs = append(errs, fmt.Errorf("no %s baseline in %s", env, measurementPath(env)))
			continue
		}
		total := base
		fmt.Printf("%-48s %5.0f MiB\n", env+" baseline", base)
		for _, id := range recommended(cat, "single-node") {
			if c, _ := cat.Get(id); !c.RunsOn(env) {
				continue
			}
			mib, ok := latest(readMeasurement(measurementPath(id)), env)
			if !ok {
				errs = append(errs, fmt.Errorf("%s: not measured on %s", id, env))
				continue
			}
			total += mib
			fmt.Printf("%-48s %5.0f MiB\n", id, mib)
		}
		fmt.Printf("%-48s %5.0f MiB of %.0f\n\n", env+" total", total, cfg.SingleNode.BudgetMiB)
		if total > cfg.SingleNode.BudgetMiB {
			errs = append(errs, fmt.Errorf("%s: %.0f MiB exceeds the %.0f MiB budget", env, total, cfg.SingleNode.BudgetMiB))
		}
	}
	return errors.Join(errs...)
}

// latest returns the idle memory of the newest version of env ("k3s" matches
// "k3s-v1.37.1+k3s1").
func latest(m measurement, env string) (float64, bool) {
	keys := slices.Sorted(maps.Keys(m.Environments))
	for _, k := range slices.Backward(keys) {
		if strings.HasPrefix(k, env+"-") {
			return m.Environments[k].IdleMemoryMiB, true
		}
	}
	return 0, false
}
