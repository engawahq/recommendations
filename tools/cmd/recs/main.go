// Command recs checks, renders, tests and packages the catalog.
//
//	recs validate                    schema, licenses, references, value comments
//	recs render [ids]                helm template every profile and Kubernetes version, then kubeconform
//	recs list [-since <ref>]         e2e CI matrix (profile × architecture × component) as JSON
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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/engawahq/recommendations/tools/internal/recs"
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
	since := fs.String("since", "", "only components changed since this git commit")
	fs.Parse(os.Args[2:])
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var want map[string]bool // nil tests every component
	if *since != "" && strings.Trim(*since, "0") != "" {
		out, err := exec.Command("git", "diff", "--name-only", *since, "HEAD").Output()
		if err != nil {
			return fmt.Errorf("git diff %s: %w", *since, err)
		}
		want = changed(cat, strings.Fields(string(out)))
	}
	type job struct {
		Profile   string `json:"profile"`
		Arch      string `json:"arch"`
		Component string `json:"component"`
	}
	var jobs []job
	for _, p := range recs.Profiles {
		for _, c := range cat.Components {
			if c.Defaults[p] == "unsupported" {
				continue
			}
			// A component is retested when it or anything it installs changed.
			order, err := cat.InstallOrder([]string{c.ID}, p)
			if err != nil {
				return err
			}
			if want == nil || slices.ContainsFunc(order, func(o *recs.Component) bool { return want[o.ID] }) {
				for _, arch := range []string{"amd64", "arm64"} {
					jobs = append(jobs, job{p, arch, c.ID})
				}
			}
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"include": jobs, "version": cfg.E2E.Version,
	})
}

// shared lists the files every e2e run depends on.
var shared = []string{"config/", "tools/", "go.", "flake.", ".github/kind.yaml", ".github/workflows/e2e.yml"}

// changed returns the IDs of the components whose files changed, or nil when
// a shared file changed.
func changed(cat *recs.Catalog, files []string) map[string]bool {
	ids := map[string]bool{}
	for _, f := range files {
		for _, s := range shared {
			if strings.HasPrefix(f, s) {
				return nil
			}
		}
		for _, c := range cat.Components {
			if c.Path == f {
				ids[c.ID] = true
			}
		}
	}
	return ids
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
