// Command recs checks, renders, tests and packages the catalog.
//
//	recs validate                    schema, licenses, references, value comments
//	recs render [-kubeconform] [ids] helm template every profile and Kubernetes version
//	recs plan [-profile p] [ids]     install order
//	recs list -env <env>             components and cluster version as JSON (CI matrix)
//	recs e2e -env <env> [ids]        install, measure, smoke test, uninstall
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
	"plan":     plan,
	"list":     list,
	"e2e":      e2e,
	"budget":   budget,
	"index":    index,
}

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: recs validate|render|plan|list|e2e|budget|index [flags] [ids]")
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

func plan(fs *flag.FlagSet, cat *recs.Catalog) error {
	profile := fs.String("profile", "single-node", "profile")
	fs.Parse(os.Args[2:])
	ids := fs.Args()
	if len(ids) == 0 {
		ids = recommended(cat, *profile)
	}
	order, err := cat.InstallOrder(ids, *profile)
	for _, c := range order {
		fmt.Println(c.ID)
	}
	return err
}

// list prints, for a CI matrix, the components an e2e environment tests and
// the cluster version to test them on.
func list(fs *flag.FlagSet, cat *recs.Catalog) error {
	env := fs.String("env", "k3s", "e2e environment")
	fs.Parse(os.Args[2:])
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	e := cfg.E2E[*env]
	ids := []string{}
	for _, c := range cat.Components {
		if c.Defaults[e.Profile] != "unsupported" && c.RunsOn(*env) {
			ids = append(ids, c.ID)
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"components": ids, "version": e.Version,
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
	E2E      map[string]struct {
		Profile string `yaml:"profile"`
		Version string `yaml:"version"`
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
