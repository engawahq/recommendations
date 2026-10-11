// Command recs checks, renders, tests and packages the catalog.
//
//	recs validate                    schema, licenses, references, value comments
//	recs render [ids]                helm template every profile and Kubernetes version, then kubeconform
//	recs list                        e2e CI matrix (profile × component) as JSON
//	recs e2e -profile <p> [ids]      install, measure, smoke test, uninstall
//	recs budget                      single-node memory budget
//	recs index                       write index.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/engawa/recommendations/tools/internal/recs"
)

var commands = map[string]func(*flag.FlagSet, *recs.Catalog) error{
	"validate": validate,
	"render":   render,
	"list":     list,
	"e2e":      e2e,
	"budget":   budget,
	"index":    index,
}

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: recs validate|render|list|e2e|budget|index [flags] [ids]")
		os.Exit(2)
	}
	if dir, err := os.UserCacheDir(); err == nil {
		recs.CacheDir = filepath.Join(dir, "recs")
	}
	// Isolate helm from the user's repositories and registry logins.
	helm := filepath.Join(recs.CacheDir, "helm")
	os.Setenv("HELM_REPOSITORY_CONFIG", filepath.Join(helm, "repositories.yaml"))
	os.Setenv("HELM_REPOSITORY_CACHE", filepath.Join(helm, "repository"))
	os.Setenv("HELM_REGISTRY_CONFIG", filepath.Join(helm, "registry.json"))

	name := os.Args[1]
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cat, err := recs.Load(".")
	if err == nil {
		err = commands[name](fs, cat)
	}
	if err != nil {
		slog.Error("recs "+name+" failed", "err", err)
		os.Exit(1)
	}
}

func validate(fs *flag.FlagSet, cat *recs.Catalog) error {
	fs.Parse(os.Args[2:])
	if err := recs.Check(".", cat); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

// list prints the e2e CI matrix: every profile × component it supports, and
// the kind node version.
func list(fs *flag.FlagSet, cat *recs.Catalog) error {
	fs.Parse(os.Args[2:])
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	type job struct {
		Profile   string `json:"profile"`
		Component string `json:"component"`
	}
	var jobs []job
	for _, p := range recs.Profiles {
		for _, c := range cat.Components {
			if c.Defaults[p] != "unsupported" {
				jobs = append(jobs, job{p, c.ID})
			}
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"include": jobs, "version": cfg.E2E.Version,
	})
}

func recommended(cat *recs.Catalog, profile string) []string {
	var ids []string
	for _, c := range cat.Components {
		if c.Defaults[profile] == "recommended" {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// config is config/kubernetes.yaml.
type config struct {
	Versions []string `yaml:"versions"`
	E2E      struct {
		Version string `yaml:"version"` // kindest/node
	} `yaml:"e2e"`
}

func loadConfig() (config, error) {
	var cfg config
	return cfg, recs.ReadYAML("config/kubernetes.yaml", &cfg)
}

func selectComponents(cat *recs.Catalog, ids []string) ([]*recs.Component, error) {
	if len(ids) == 0 {
		return cat.Components, nil
	}
	var out []*recs.Component
	for _, id := range ids {
		c, ok := cat.Get(id)
		if !ok {
			return nil, fmt.Errorf("unknown component %q", id)
		}
		out = append(out, c)
	}
	return out, nil
}
