package main

import (
	"bytes"
	"code.google.com/p/go.tools/go/vcs"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Godeps describes what a package needs to be rebuilt reproducibly.
// It's the same information stored in file Godeps.
type Godeps struct {
	ImportPath string
	GoVersion  string
	Deps       []Dependency

	outerRoot string
}

// A Dependency is a specific revision of a package.
type Dependency struct {
	ImportPath string
	Comment    string `json:",omitempty"` // Description of commit, if present.
	Rev        string // VCS-specific commit ID.

	outerRoot string // dir, if present, in outer GOPATH
	repoRoot  *vcs.RepoRoot
	vcs       *VCS
}

func LoadGodeps(a []*Package) (*Godeps, error) {
	var err error
	g := new(Godeps)
	g.ImportPath = a[0].ImportPath
	g.GoVersion, err = goVersion()
	if err != nil {
		return nil, err
	}
	deps := a[0].Deps
	for _, p := range a[1:] {
		deps = append(deps, p.ImportPath)
		deps = append(deps, p.Deps...)
	}
	sort.Strings(deps)
	pkgs, err := LoadPackages(deps)
	if err != nil {
		log.Fatalln(err)
	}
	seen := []string{a[0].ImportPath}
	var err1 error
	for _, pkg := range pkgs {
		name := pkg.ImportPath
		if pkg.Error.Err != "" {
			log.Println(pkg.Error.Err)
			err = errors.New("error loading dependencies")
			continue
		}
		if !pathPrefixIn(seen, name) && !pkg.Standard {
			vcs, _, err := VCSForImportPath(pkg.ImportPath)
			if err != nil {
				log.Println(err)
				err1 = errors.New("error loading dependencies")
				continue
			}
			seen = append(seen, name+"/")
			var id string
			id, err = vcs.identify(pkg.Dir)
			if err != nil {
				log.Println(err)
				err1 = errors.New("error loading dependencies")
				continue
			}
			if vcs.isDirty(pkg.Dir) {
				log.Println("dirty working tree:", pkg.Dir)
				err1 = errors.New("error loading dependencies")
				continue
			}
			comment := vcs.describe(pkg.Dir, id)
			g.Deps = append(g.Deps, Dependency{
				ImportPath: name,
				Rev:        id,
				Comment:    comment,
			})
		}
	}
	if err1 != nil {
		return nil, err1
	}
	return g, nil
}

func ReadGodeps(path string) (*Godeps, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	g := new(Godeps)
	err = json.NewDecoder(f).Decode(g)
	if err != nil {
		return nil, err
	}
	err = g.loadGoList()
	if err != nil {
		return nil, err
	}

	for i := range g.Deps {
		d := &g.Deps[i]
		d.vcs, d.repoRoot, err = VCSForImportPath(d.ImportPath)
		if err != nil {
			return nil, err
		}
	}
	return g, nil
}

func (g *Godeps) loadGoList() error {
	a := []string{g.ImportPath}
	for _, d := range g.Deps {
		a = append(a, d.ImportPath)
	}
	ps, err := LoadPackages(a)
	if err != nil {
		return err
	}
	g.outerRoot = ps[0].Root
	for i, p := range ps[1:] {
		g.Deps[i].outerRoot = p.Root
	}
	return nil
}

func (g *Godeps) WriteTo(w io.Writer) (int, error) {
	b, err := json.MarshalIndent(g, "", "\t")
	if err != nil {
		return 0, err
	}
	return w.Write(append(b, '\n'))
}

// Returns a path to the local copy of d's repository.
// E.g.
//
//   ImportPath             RepoPath
//   github.com/kr/s3       $spool/github.com/kr/s3
//   github.com/lib/pq/oid  $spool/github.com/lib/pq
func (d Dependency) RepoPath() string {
	return filepath.Join(spool, "repo", d.repoRoot.Root)
}

// Returns a URL for the remote copy of the repository.
func (d Dependency) RemoteURL() string {
	return d.repoRoot.Repo
}

// Returns the url of a local disk clone of the repo, if any.
func (d Dependency) FastRemotePath() string {
	if d.outerRoot != "" {
		return d.outerRoot + "/src/" + d.repoRoot.Root
	}
	return ""
}

// Returns a path to the checked-out copy of d's commit.
func (d Dependency) Workdir() string {
	return filepath.Join(d.Gopath(), "src", d.ImportPath)
}

// Returns a path to the checked-out copy of d's repo root.
func (d Dependency) WorkdirRoot() string {
	return filepath.Join(d.Gopath(), "src", d.repoRoot.Root)
}

// Returns a path to a parent of Workdir such that using
// Gopath in GOPATH makes d available to the go tool.
func (d Dependency) Gopath() string {
	return filepath.Join(spool, "rev", d.Rev[:2], d.Rev[2:])
}

// Creates an empty repo in d.RepoPath().
func (d Dependency) CreateRepo(fastRemote, mainRemote string) error {
	if err := os.MkdirAll(d.RepoPath(), 0777); err != nil {
		return err
	}
	if err := d.vcs.create(d.RepoPath()); err != nil {
		return err
	}
	if err := d.link(fastRemote, d.FastRemotePath()); err != nil {
		return err
	}
	return d.link(mainRemote, d.RemoteURL())
}

func (d Dependency) link(remote, url string) error {
	return d.vcs.link(d.RepoPath(), remote, url)
}

func (d Dependency) fetchAndCheckout(remote string) error {
	if err := d.fetch(remote); err != nil {
		return fmt.Errorf("fetch: %s", err)
	}
	if err := d.checkout(); err != nil {
		return fmt.Errorf("checkout: %s", err)
	}
	return nil
}

func (d Dependency) fetch(remote string) error {
	return d.vcs.fetch(d.RepoPath(), remote)
}

func (d Dependency) checkout() error {
	dir := d.WorkdirRoot()
	if exists(dir) {
		return nil
	}
	if !d.vcs.exists(d.RepoPath(), d.Rev) {
		return fmt.Errorf("unknown rev %s for %s", d.Rev, d.ImportPath)
	}
	if err := os.MkdirAll(dir, 0777); err != nil {
		return err
	}
	return d.vcs.checkout(dir, d.Rev, d.RepoPath())
}

func pathPrefixIn(a []string, s string) bool {
	for _, p := range a {
		if s == p || strings.HasPrefix(s, p+"/") {
			return true
		}
	}
	return false
}

func goVersion() (string, error) {
	cmd := exec.Command("go", "version")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(out)), nil
}
