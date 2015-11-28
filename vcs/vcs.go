// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vcs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/gophersaurus/govend/go15vendorexperiment"
	"github.com/gophersaurus/govend/vcs/internal/singleflight"
)

// Verbose enables verbose operation logging.
var Verbose bool

// ShowCmd controls whether VCS commands are printed.
var ShowCmd bool

// A Cmd describes how to use a version control system
// like Mercurial, Git, or Subversion.
type Cmd struct {
	Name string
	Cmd  string // name of binary to invoke command

	CreateCmd   []string // commands to download a fresh copy of a repository
	DownloadCmd []string // commands to download updates into an existing repository

	TagCmd         []TagCmd // commands to list tags
	TagLookupCmd   []TagCmd // commands to lookup tags before running tagSyncCmd
	TagSyncCmd     []string // commands to sync to specific tag
	TagSyncDefault []string // commands to sync to default tag

	Scheme  []string
	PingCmd string

	RemoteRepo  func(v *Cmd, rootDir string) (remoteRepo string, err error)
	ResolveRepo func(v *Cmd, rootDir, remoteRepo string) (realRepo string, err error)
}

var isSecureScheme = map[string]bool{
	"https":   true,
	"git+ssh": true,
	"bzr+ssh": true,
	"svn+ssh": true,
	"ssh":     true,
}

func (c *Cmd) isSecure(repo string) bool {
	u, err := url.Parse(repo)
	if err != nil {
		// If repo is not a URL, it's not secure.
		return false
	}
	return isSecureScheme[u.Scheme]
}

// A TagCmd describes a command to list available tags
// that can be passed to tagSyncCmd.
type TagCmd struct {
	Cmd     string // command to list tags
	Pattern string // regexp to extract tags from list
}

// List lists the known version control systems
var List = []*Cmd{
	Hg,
	Git,
	Svn,
	Bzr,
}

// ByCmd returns the version control system for the given
// command name (hg, git, svn, bzr).
func ByCmd(cmd string) *Cmd {
	for _, vcs := range List {
		if vcs.Cmd == cmd {
			return vcs
		}
	}
	return nil
}

// Hg describes how to use Mercurial.
var Hg = &Cmd{
	Name: "Mercurial",
	Cmd:  "hg",

	CreateCmd:   []string{"clone -U {repo} {dir}"},
	DownloadCmd: []string{"pull"},

	// We allow both tag and branch names as 'tags'
	// for selecting a version.  This lets people have
	// a go.release.r60 branch and a go1 branch
	// and make changes in both, without constantly
	// editing .hgtags.
	TagCmd: []TagCmd{
		{"tags", `^(\S+)`},
		{"branches", `^(\S+)`},
	},
	TagSyncCmd:     []string{"update -r {tag}"},
	TagSyncDefault: []string{"update default"},

	Scheme:     []string{"https", "http", "ssh"},
	PingCmd:    "identify {scheme}://{repo}",
	RemoteRepo: hgRemoteRepo,
}

func hgRemoteRepo(vcsHg *Cmd, rootDir string) (remoteRepo string, err error) {
	out, err := vcsHg.runOutput(rootDir, "paths default")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Git describes how to use Git.
var Git = &Cmd{
	Name: "Git",
	Cmd:  "git",

	CreateCmd:   []string{"clone {repo} {dir}", "--git-dir={dir}/.git submodule update --init --recursive"},
	DownloadCmd: []string{"pull --ff-only", "submodule update --init --recursive"},

	TagCmd: []TagCmd{
		// tags/xxx matches a git tag named xxx
		// origin/xxx matches a git branch named xxx on the default remote repository
		{"show-ref", `(?:tags|origin)/(\S+)$`},
	},
	TagLookupCmd: []TagCmd{
		{"show-ref tags/{tag} origin/{tag}", `((?:tags|origin)/\S+)$`},
	},
	TagSyncCmd: []string{"checkout {tag}", "submodule update --init --recursive"},
	// both createCmd and downloadCmd update the working dir.
	// No need to do more here. We used to 'checkout master'
	// but that doesn't work if the default branch is not named master.
	// See golang.org/issue/9032.
	TagSyncDefault: []string{"checkout master", "submodule update --init --recursive"},

	Scheme:     []string{"git", "https", "http", "git+ssh", "ssh"},
	PingCmd:    "ls-remote {scheme}://{repo}",
	RemoteRepo: gitRemoteRepo,
}

// scpSyntaxRe matches the SCP-like addresses used by Git to access
// repositories by SSH.
var scpSyntaxRe = regexp.MustCompile(`^([a-zA-Z0-9_]+)@([a-zA-Z0-9._-]+):(.*)$`)

func gitRemoteRepo(git *Cmd, rootDir string) (remoteRepo string, err error) {
	cmd := "config remote.origin.url"
	errParse := errors.New("unable to parse output of git " + cmd)
	errRemoteOriginNotFound := errors.New("remote origin not found")
	outb, err := git.run1(rootDir, cmd, nil, false, false)
	if err != nil {
		// if it doesn't output any message, it means the config argument is correct,
		// but the config value itself doesn't exist
		if outb != nil && len(outb) == 0 {
			return "", errRemoteOriginNotFound
		}
		return "", err
	}
	out := strings.TrimSpace(string(outb))

	var repoURL *url.URL
	if m := scpSyntaxRe.FindStringSubmatch(out); m != nil {
		// Match SCP-like syntax and convert it to a URL.
		// Eg, "git@github.com:user/repo" becomes
		// "ssh://git@github.com/user/repo".
		repoURL = &url.URL{
			Scheme:  "ssh",
			User:    url.User(m[1]),
			Host:    m[2],
			RawPath: m[3],
		}
	} else {
		repoURL, err = url.Parse(out)
		if err != nil {
			return "", err
		}
	}

	// Iterate over insecure schemes too, because this function simply
	// reports the state of the repo. If we can't see insecure schemes then
	// we can't report the actual repo URL.
	for _, s := range git.Scheme {
		if repoURL.Scheme == s {
			return repoURL.String(), nil
		}
	}
	return "", errParse
}

// Bzr describes how to use Bazaar.
var Bzr = &Cmd{
	Name: "Bazaar",
	Cmd:  "bzr",

	CreateCmd: []string{"branch {repo} {dir}"},

	// Without --overwrite bzr will not pull tags that changed.
	// Replace by --overwrite-tags after http://pad.lv/681792 goes in.
	DownloadCmd: []string{"pull --overwrite"},

	TagCmd:         []TagCmd{{"tags", `^(\S+)`}},
	TagSyncCmd:     []string{"update -r {tag}"},
	TagSyncDefault: []string{"update -r revno:-1"},

	Scheme:      []string{"https", "http", "bzr", "bzr+ssh"},
	PingCmd:     "info {scheme}://{repo}",
	RemoteRepo:  bzrRemoteRepo,
	ResolveRepo: bzrResolveRepo,
}

func bzrRemoteRepo(bzr *Cmd, rootDir string) (remoteRepo string, err error) {
	outb, err := bzr.runOutput(rootDir, "config parent_location")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(outb)), nil
}

func bzrResolveRepo(bzr *Cmd, rootDir, remoteRepo string) (realRepo string, err error) {
	outb, err := bzr.runOutput(rootDir, "info "+remoteRepo)
	if err != nil {
		return "", err
	}
	out := string(outb)

	// Expect:
	// ...
	//   (branch root|repository branch): <URL>
	// ...

	found := false
	for _, prefix := range []string{"\n  branch root: ", "\n  repository branch: "} {
		i := strings.Index(out, prefix)
		if i >= 0 {
			out = out[i+len(prefix):]
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("unable to parse output of bzr info")
	}

	i := strings.Index(out, "\n")
	if i < 0 {
		return "", fmt.Errorf("unable to parse output of bzr info")
	}
	out = out[:i]
	return strings.TrimSpace(string(out)), nil
}

// Svn describes how to use Subversion.
var Svn = &Cmd{
	Name: "Subversion",
	Cmd:  "svn",

	CreateCmd:   []string{"checkout {repo} {dir}"},
	DownloadCmd: []string{"update"},

	// There is no tag command in subversion.
	// The branch information is all in the path names.

	Scheme:     []string{"https", "http", "svn", "svn+ssh"},
	PingCmd:    "info {scheme}://{repo}",
	RemoteRepo: svnRemoteRepo,
}

func svnRemoteRepo(svn *Cmd, rootDir string) (remoteRepo string, err error) {
	outb, err := svn.runOutput(rootDir, "info")
	if err != nil {
		return "", err
	}
	out := string(outb)

	// Expect:
	// ...
	// Repository Root: <URL>
	// ...

	i := strings.Index(out, "\nRepository Root: ")
	if i < 0 {
		return "", fmt.Errorf("unable to parse output of svn info")
	}
	out = out[i+len("\nRepository Root: "):]
	i = strings.Index(out, "\n")
	if i < 0 {
		return "", fmt.Errorf("unable to parse output of svn info")
	}
	out = out[:i]
	return strings.TrimSpace(string(out)), nil
}

func (c *Cmd) String() string {
	return c.Name
}

// run runs the command line cmd in the given directory.
// keyval is a list of key, value pairs.  run expands
// instances of {key} in cmd into value, but only after
// splitting cmd into individual arguments.
// If an error occurs, run prints the command line and the
// command's combined stdout+stderr to standard error.
// Otherwise run discards the command's output.
func (c *Cmd) run(dir string, cmd string, keyval ...string) error {
	_, err := c.run1(dir, cmd, keyval, false, false)
	return err
}

// runVerboseOnly is like run but only generates error output to standard error in verbose mode.
func (c *Cmd) runVerboseOnly(dir string, cmd string, keyval ...string) error {
	_, err := c.run1(dir, cmd, keyval, true, false)
	return err
}

// runOutput is like run but returns the output of the command.
func (c *Cmd) runOutput(dir string, cmd string, keyval ...string) ([]byte, error) {
	return c.run1(dir, cmd, keyval, true, true)
}

// run1 is the generalized implementation of run and runOutput.
func (c *Cmd) run1(dir string, cmdline string, keyval []string, verbose bool, commands bool) ([]byte, error) {
	m := make(map[string]string)
	for i := 0; i < len(keyval); i += 2 {
		m[keyval[i]] = keyval[i+1]
	}
	args := strings.Fields(cmdline)
	for i, arg := range args {
		args[i] = expand(m, arg)
	}

	_, err := exec.LookPath(c.Cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"go: missing %s command. See https://golang.org/s/gogetcmd\n",
			c.Name)
		return nil, err
	}

	cmd := exec.Command(c.Cmd, args...)
	cmd.Dir = dir
	cmd.Env = envForDir(cmd.Dir, os.Environ())
	if commands {
		fmt.Printf("cd %s\n", dir)
		fmt.Printf("%s %s\n", c.Cmd, strings.Join(args, " "))
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	out := buf.Bytes()
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "# cd %s; %s %s\n", dir, c.Cmd, strings.Join(args, " "))
			os.Stderr.Write(out)
		}
		return out, err
	}
	return out, nil
}

// Ping pings to determine scheme to use.
func (c *Cmd) Ping(scheme, repo string) error {
	return c.runVerboseOnly(".", c.PingCmd, "scheme", scheme, "repo", repo)
}

// Create creates a new copy of repo in dir.
// The parent of dir must exist; dir must not.
func (c *Cmd) Create(dir, repo string) error {
	for _, cmd := range c.CreateCmd {
		if !go15vendorexperiment.On() && strings.Contains(cmd, "submodule") {
			continue
		}
		if err := c.run(".", cmd, "dir", dir, "repo", repo); err != nil {
			return err
		}
	}
	return nil
}

// CreateAtRev creates a new copy of repo in dir at revision rev.
// The parent of dir must exist; dir must not.
// rev must be a valid revision in repo.
func (c *Cmd) CreateAtRev(dir, repo, rev string) error {
	if err := c.Create(dir, repo); err != nil {
		return err
	}
	for _, cmd := range c.DownloadCmd {
		if err := c.run(dir, cmd, "tag", rev); err != nil {
			return err
		}
	}
	return nil
}

// Download downloads any new changes for the repo in dir.
func (c *Cmd) Download(dir string, verbose bool) error {
	if err := c.fixDetachedHead(dir, verbose); err != nil {
		return err
	}
	for _, cmd := range c.DownloadCmd {
		if !go15vendorexperiment.On() && strings.Contains(cmd, "submodule") {
			continue
		}
		if err := c.run(dir, cmd); err != nil {
			return err
		}
	}
	return nil
}

// fixDetachedHead switches a Git repository in dir from a detached head to the master branch.
// Go versions before 1.2 downloaded Git repositories in an unfortunate way
// that resulted in the working tree state being on a detached head.
// That meant the repository was not usable for normal Git operations.
// Go 1.2 fixed that, but we can't pull into a detached head, so if this is
// a Git repository we check for being on a detached head and switch to the
// real branch, almost always called "master".
// TODO(dsymonds): Consider removing this for Go 1.3.
func (c *Cmd) fixDetachedHead(dir string, verbose bool) error {
	if c != Git {
		return nil
	}

	// "git symbolic-ref HEAD" succeeds iff we are not on a detached head.
	if err := c.runVerboseOnly(dir, "symbolic-ref HEAD"); err == nil {
		// not on a detached head
		return nil
	}
	if verbose {
		log.Printf("%s on detached head; repairing", dir)
	}
	return c.run(dir, "checkout master")
}

// Tags returns the list of available tags for the repo in dir.
func (c *Cmd) Tags(dir string) ([]string, error) {
	var tags []string
	for _, tc := range c.TagCmd {
		out, err := c.runOutput(dir, tc.Cmd)
		if err != nil {
			return nil, err
		}
		re := regexp.MustCompile(`(?m-s)` + tc.Pattern)
		for _, m := range re.FindAllStringSubmatch(string(out), -1) {
			tags = append(tags, m[1])
		}
	}
	return tags, nil
}

// TagSync syncs the repo in dir to the named tag,
// which either is a tag returned by tags or is v.tagDefault.
func (c *Cmd) TagSync(dir, tag string) error {
	if c.TagSyncCmd == nil {
		return nil
	}
	if tag != "" {
		for _, tc := range c.TagLookupCmd {
			out, err := c.runOutput(dir, tc.Cmd, "tag", tag)
			if err != nil {
				return err
			}
			re := regexp.MustCompile(`(?m-s)` + tc.Pattern)
			m := re.FindStringSubmatch(string(out))
			if len(m) > 1 {
				tag = m[1]
				break
			}
		}
	}

	if tag == "" && c.TagSyncDefault != nil {
		for _, cmd := range c.TagSyncDefault {
			if !go15vendorexperiment.On() && strings.Contains(cmd, "submodule") {
				continue
			}
			if err := c.run(dir, cmd); err != nil {
				return err
			}
		}
		return nil
	}

	for _, cmd := range c.TagSyncCmd {
		if !go15vendorexperiment.On() && strings.Contains(cmd, "submodule") {
			continue
		}
		if err := c.run(dir, cmd, "tag", tag); err != nil {
			return err
		}
	}
	return nil
}

// A vcsPath describes how to convert an import path into a
// version control system and repository name.
type vcsPath struct {
	prefix string                              // prefix this description applies to
	re     string                              // pattern for import path
	repo   string                              // repository to use (expand with match of re)
	vcs    string                              // version control system to use (expand with match of re)
	check  func(match map[string]string) error // additional checks
	ping   bool                                // ping for scheme to use to download repo

	regexp *regexp.Regexp // cached compiled form of re
}

// FromDir inspects dir and its parents to determine the
// version control system and code repository to use.
// On return, root is the import path
// corresponding to the root of the repository
// (thus root is a prefix of importPath).
func FromDir(dir, srcRoot string) (vcs *Cmd, root string, err error) {
	// Clean and double-check that dir is in (a subdirectory of) srcRoot.
	dir = filepath.Clean(dir)
	srcRoot = filepath.Clean(srcRoot)
	if len(dir) <= len(srcRoot) || dir[len(srcRoot)] != filepath.Separator {
		return nil, "", fmt.Errorf("directory %q is outside source root %q", dir, srcRoot)
	}

	for len(dir) > len(srcRoot) {
		for _, vcs := range List {
			if fi, err := os.Stat(filepath.Join(dir, "."+vcs.Cmd)); err == nil && fi.IsDir() {
				return vcs, dir[len(srcRoot)+1:], nil
			}
		}

		// Move to parent.
		ndir := filepath.Dir(dir)
		if len(ndir) >= len(dir) {
			// Shouldn't happen, but just in case, stop.
			break
		}
		dir = ndir
	}

	return nil, "", fmt.Errorf("directory %q is not using a known version control system", dir)
}

// RepoRoot represents a version control system, a repo, and a root of
// where to put it on disk.
type RepoRoot struct {
	VCS *Cmd

	// repo is the repository URL, including scheme
	Repo string

	// root is the import path corresponding to the root of the
	// repository
	Root string
}

var httpPrefixRE = regexp.MustCompile(`^https?:`)

// securityMode specifies whether a function should make network
// calls using insecure transports (eg, plain text HTTP).
// The zero value is "secure".
type securityMode int

const (
	Secure securityMode = iota
	Insecure
)

// RepoRootForImportPath analyzes importPath to determine the
// version control system, and code repository to use.
func RepoRootForImportPath(importPath string, security securityMode, verbose bool) (*RepoRoot, error) {
	rr, err := RepoRootFromVCSPaths(importPath, "", security, vcsPaths)
	if err == errUnknownSite {
		// If there are wildcards, look up the thing before the wildcard,
		// hoping it applies to the wildcarded parts too.
		// This makes 'go get rsc.io/pdf/...' work in a fresh GOPATH.
		lookup := strings.TrimSuffix(importPath, "/...")
		if i := strings.Index(lookup, "/.../"); i >= 0 {
			lookup = lookup[:i]
		}
		rr, err = RepoRootForImportDynamic(lookup, security, verbose)
		if err != nil {
			err = fmt.Errorf("unrecognized import path %q (%v)", importPath, err)
		}
	}
	if err != nil {
		rr1, err1 := RepoRootFromVCSPaths(importPath, "", security, vcsPathsAfterDynamic)
		if err1 == nil {
			rr = rr1
			err = nil
		}
	}

	if err == nil && strings.Contains(importPath, "...") && strings.Contains(rr.Root, "...") {
		// Do not allow wildcards in the repo root.
		rr = nil
		err = fmt.Errorf("cannot expand ... in %q", importPath)
	}
	return rr, err
}

var errUnknownSite = errors.New("dynamic lookup required to find mapping")

// RepoRootFromVCSPaths attempts to map importPath to a repoRoot
// using the mappings defined in vcsPaths.
// If scheme is non-empty, that scheme is forced.
func RepoRootFromVCSPaths(importPath, scheme string, security securityMode, vcsPaths []*vcsPath) (*RepoRoot, error) {
	// A common error is to use https://packagepath because that's what
	// hg and git require. Diagnose this helpfully.
	if loc := httpPrefixRE.FindStringIndex(importPath); loc != nil {
		// The importPath has been cleaned, so has only one slash. The pattern
		// ignores the slashes; the error message puts them back on the RHS at least.
		return nil, fmt.Errorf("%q not allowed in import path", importPath[loc[0]:loc[1]]+"//")
	}
	for _, srv := range vcsPaths {
		if !strings.HasPrefix(importPath, srv.prefix) {
			continue
		}
		m := srv.regexp.FindStringSubmatch(importPath)
		if m == nil {
			if srv.prefix != "" {
				return nil, fmt.Errorf("invalid %s import path %q", srv.prefix, importPath)
			}
			continue
		}

		// Build map of named subexpression matches for expand.
		match := map[string]string{
			"prefix": srv.prefix,
			"import": importPath,
		}
		for i, name := range srv.regexp.SubexpNames() {
			if name != "" && match[name] == "" {
				match[name] = m[i]
			}
		}
		if srv.vcs != "" {
			match["vcs"] = expand(match, srv.vcs)
		}
		if srv.repo != "" {
			match["repo"] = expand(match, srv.repo)
		}
		if srv.check != nil {
			if err := srv.check(match); err != nil {
				return nil, err
			}
		}
		vcs := ByCmd(match["vcs"])
		if vcs == nil {
			return nil, fmt.Errorf("unknown version control system %q", match["vcs"])
		}
		if srv.ping {
			if scheme != "" {
				match["repo"] = scheme + "://" + match["repo"]
			} else {
				for _, scheme := range vcs.Scheme {
					if security == Secure && !isSecureScheme[scheme] {
						continue
					}
					if vcs.Ping(scheme, match["repo"]) == nil {
						match["repo"] = scheme + "://" + match["repo"]
						break
					}
				}
			}
		}
		rr := &RepoRoot{
			VCS:  vcs,
			Repo: match["repo"],
			Root: match["root"],
		}
		return rr, nil
	}
	return nil, errUnknownSite
}

// repoRootForImportDynamic finds a *repoRoot for a custom domain that's not
// statically known by repoRootForImportPathStatic.
//
// This handles custom import paths like "name.tld/pkg/foo" or just "name.tld".
func RepoRootForImportDynamic(importPath string, security securityMode, verbose bool) (*RepoRoot, error) {
	slash := strings.Index(importPath, "/")
	if slash < 0 {
		slash = len(importPath)
	}
	host := importPath[:slash]
	if !strings.Contains(host, ".") {
		return nil, errors.New("import path does not begin with hostname")
	}
	urlStr, body, err := HttpsOrHTTP(importPath, security, verbose)
	if err != nil {
		msg := "https fetch: %v"
		if security == Insecure {
			msg = "http/" + msg
		}
		return nil, fmt.Errorf(msg, err)
	}
	defer body.Close()
	imports, err := ParseMetaGoImports(body)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %v", importPath, err)
	}
	// Find the matched meta import.
	mmi, err := matchGoImport(imports, importPath)
	if err != nil {
		if err != errNoMatch {
			return nil, fmt.Errorf("parse %s: %v", urlStr, err)
		}
		return nil, fmt.Errorf("parse %s: no go-import meta tags", urlStr)
	}
	if verbose {
		log.Printf("get %q: found meta tag %#v at %s", importPath, mmi, urlStr)
	}
	// If the import was "uni.edu/bob/project", which said the
	// prefix was "uni.edu" and the RepoRoot was "evilroot.com",
	// make sure we don't trust Bob and check out evilroot.com to
	// "uni.edu" yet (possibly overwriting/preempting another
	// non-evil student).  Instead, first verify the root and see
	// if it matches Bob's claim.
	if mmi.Prefix != importPath {
		if verbose {
			log.Printf("get %q: verifying non-authoritative meta tag", importPath)
		}
		urlStr0 := urlStr
		var imports []MetaImport
		urlStr, imports, err = MetaImportsForPrefix(mmi.Prefix, security, verbose)
		if err != nil {
			return nil, err
		}
		metaImport2, err := matchGoImport(imports, importPath)
		if err != nil || mmi != metaImport2 {
			return nil, fmt.Errorf("%s and %s disagree about go-import for %s", urlStr0, urlStr, mmi.Prefix)
		}
	}

	if !strings.Contains(mmi.RepoRoot, "://") {
		return nil, fmt.Errorf("%s: invalid repo root %q; no scheme", urlStr, mmi.RepoRoot)
	}
	rr := &RepoRoot{
		VCS:  ByCmd(mmi.VCS),
		Repo: mmi.RepoRoot,
		Root: mmi.Prefix,
	}
	if rr.VCS == nil {
		return nil, fmt.Errorf("%s: unknown vcs %q", urlStr, mmi.VCS)
	}
	return rr, nil
}

var fetchGroup singleflight.Group
var (
	fetchCacheMu sync.Mutex
	fetchCache   = map[string]fetchResult{} // key is metaImportsForPrefix's importPrefix
)

// MetaImportsForPrefix takes a package's root import path as declared in a <meta> tag
// and returns its HTML discovery URL and the parsed metaImport lines
// found on the page.
//
// The importPath is of the form "golang.org/x/tools".
// It is an error if no imports are found.
// urlStr will still be valid if err != nil.
// The returned urlStr will be of the form "https://golang.org/x/tools?go-get=1"
func MetaImportsForPrefix(importPrefix string, security securityMode, verbose bool) (urlStr string, imports []MetaImport, err error) {
	setCache := func(res fetchResult) (fetchResult, error) {
		fetchCacheMu.Lock()
		defer fetchCacheMu.Unlock()
		fetchCache[importPrefix] = res
		return res, nil
	}

	resi, _, _ := fetchGroup.Do(importPrefix, func() (resi interface{}, err error) {
		fetchCacheMu.Lock()
		if res, ok := fetchCache[importPrefix]; ok {
			fetchCacheMu.Unlock()
			return res, nil
		}
		fetchCacheMu.Unlock()

		urlStr, body, err := HttpsOrHTTP(importPrefix, security, verbose)
		if err != nil {
			return setCache(fetchResult{urlStr: urlStr, err: fmt.Errorf("fetch %s: %v", urlStr, err)})
		}
		imports, err := ParseMetaGoImports(body)
		if err != nil {
			return setCache(fetchResult{urlStr: urlStr, err: fmt.Errorf("parsing %s: %v", urlStr, err)})
		}
		if len(imports) == 0 {
			err = fmt.Errorf("fetch %s: no go-import meta tag", urlStr)
		}
		return setCache(fetchResult{urlStr: urlStr, imports: imports, err: err})
	})
	res := resi.(fetchResult)
	return res.urlStr, res.imports, res.err
}

type fetchResult struct {
	urlStr  string // e.g. "https://foo.com/x/bar?go-get=1"
	imports []MetaImport
	err     error
}

// MetaImport represents the parsed <meta name="go-import"
// content="prefix vcs reporoot" /> tags from HTML files.
type MetaImport struct {
	Prefix, VCS, RepoRoot string
}

// errNoMatch is returned from matchGoImport when there's no applicable match.
var errNoMatch = errors.New("no import match")

// matchGoImport returns the metaImport from imports matching importPath.
// An error is returned if there are multiple matches.
// errNoMatch is returned if none match.
func matchGoImport(imports []MetaImport, importPath string) (_ MetaImport, err error) {
	match := -1
	for i, im := range imports {
		if !strings.HasPrefix(importPath, im.Prefix) {
			continue
		}
		if match != -1 {
			err = fmt.Errorf("multiple meta tags match import path %q", importPath)
			return
		}
		match = i
	}
	if match == -1 {
		err = errNoMatch
		return
	}
	return imports[match], nil
}

// expand rewrites s to replace {k} with match[k] for each key k in match.
func expand(match map[string]string, s string) string {
	for k, v := range match {
		s = strings.Replace(s, "{"+k+"}", v, -1)
	}
	return s
}

// vcsPaths defines the meaning of import paths referring to
// commonly-used VCS hosting sites (github.com/user/dir)
// and import paths referring to a fully-qualified importPath
// containing a VCS type (foo.com/repo.git/dir)
var vcsPaths = []*vcsPath{
	// Google Code - new syntax
	{
		prefix: "code.google.com/",
		re:     `^(?P<root>code\.google\.com/p/(?P<project>[a-z0-9\-]+)(\.(?P<subrepo>[a-z0-9\-]+))?)(/[A-Za-z0-9_.\-]+)*$`,
		repo:   "https://{root}",
		check:  googleCodeVCS,
	},

	// Google Code - old syntax
	{
		re:    `^(?P<project>[a-z0-9_\-.]+)\.googlecode\.com/(git|hg|svn)(?P<path>/.*)?$`,
		check: oldGoogleCode,
	},

	// Github
	{
		prefix: "github.com/",
		re:     `^(?P<root>github\.com/[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+)(/[A-Za-z0-9_.\-]+)*$`,
		vcs:    "git",
		repo:   "https://{root}",
		check:  noVCSSuffix,
	},
	{
		prefix: "git.target.com/",
		re:     `^(?P<root>git.target\.com/[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+)(/[A-Za-z0-9_.\-]+)*$`,
		vcs:    "git",
		repo:   "https://{root}",
		check:  noVCSSuffix,
	},

	// Bitbucket
	{
		prefix: "bitbucket.org/",
		re:     `^(?P<root>bitbucket\.org/(?P<bitname>[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+))(/[A-Za-z0-9_.\-]+)*$`,
		repo:   "https://{root}",
		check:  bitbucketVCS,
	},

	// IBM DevOps Services (JazzHub)
	{
		prefix: "hub.jazz.net/git",
		re:     `^(?P<root>hub.jazz.net/git/[a-z0-9]+/[A-Za-z0-9_.\-]+)(/[A-Za-z0-9_.\-]+)*$`,
		vcs:    "git",
		repo:   "https://{root}",
		check:  noVCSSuffix,
	},

	// Git at Apache
	{
		prefix: "git.apache.org",
		re:     `^(?P<root>git.apache.org/[a-z0-9_.\-]+\.git)(/[A-Za-z0-9_.\-]+)*$`,
		vcs:    "git",
		repo:   "https://{root}",
	},

	// General syntax for any server.
	// Must be last.
	{
		re:   `^(?P<root>(?P<repo>([a-z0-9.\-]+\.)+[a-z0-9.\-]+(:[0-9]+)?/[A-Za-z0-9_.\-/]*?)\.(?P<vcs>bzr|git|hg|svn))(/[A-Za-z0-9_.\-]+)*$`,
		ping: true,
	},
}

// vcsPathsAfterDynamic gives additional vcsPaths entries
// to try after the dynamic HTML check.
// This gives those sites a chance to introduce <meta> tags
// as part of a graceful transition away from the hard-coded logic.
var vcsPathsAfterDynamic = []*vcsPath{
	// Launchpad. See golang.org/issue/11436.
	{
		prefix: "launchpad.net/",
		re:     `^(?P<root>launchpad\.net/((?P<project>[A-Za-z0-9_.\-]+)(?P<series>/[A-Za-z0-9_.\-]+)?|~[A-Za-z0-9_.\-]+/(\+junk|[A-Za-z0-9_.\-]+)/[A-Za-z0-9_.\-]+))(/[A-Za-z0-9_.\-]+)*$`,
		vcs:    "bzr",
		repo:   "https://{root}",
		check:  launchpadVCS,
	},
}

func init() {
	// fill in cached regexps.
	// Doing this eagerly discovers invalid regexp syntax
	// without having to run a command that needs that regexp.
	for _, srv := range vcsPaths {
		srv.regexp = regexp.MustCompile(srv.re)
	}
	for _, srv := range vcsPathsAfterDynamic {
		srv.regexp = regexp.MustCompile(srv.re)
	}
}

// noVCSSuffix checks that the repository name does not
// end in .foo for any version control system foo.
// The usual culprit is ".git".
func noVCSSuffix(match map[string]string) error {
	repo := match["repo"]
	for _, vcs := range List {
		if strings.HasSuffix(repo, "."+vcs.Cmd) {
			return fmt.Errorf("invalid version control suffix in %s path", match["prefix"])
		}
	}
	return nil
}

var googleCheckout = regexp.MustCompile(`id="checkoutcmd">(hg|git|svn)`)

// googleCodeVCS determines the version control system for
// a code.google.com repository, by scraping the project's
// /source/checkout page.
func googleCodeVCS(match map[string]string) error {
	if err := noVCSSuffix(match); err != nil {
		return err
	}
	data, err := httpGET(expand(match, "https://code.google.com/p/{project}/source/checkout?repo={subrepo}"))
	if err != nil {
		return err
	}

	if m := googleCheckout.FindSubmatch(data); m != nil {
		if vcs := ByCmd(string(m[1])); vcs != nil {
			// Subversion requires the old URLs.
			// TODO: Test.
			if vcs == Svn {
				if match["subrepo"] != "" {
					return fmt.Errorf("sub-repositories not supported in Google Code Subversion projects")
				}
				match["repo"] = expand(match, "https://{project}.googlecode.com/svn")
			}
			match["vcs"] = vcs.Cmd
			return nil
		}
	}

	return fmt.Errorf("unable to detect version control system for code.google.com/ path")
}

// oldGoogleCode is invoked for old-style foo.googlecode.com paths.
// It prints an error giving the equivalent new path.
func oldGoogleCode(match map[string]string) error {
	return fmt.Errorf("invalid Google Code import path: use %s instead",
		expand(match, "code.google.com/p/{project}{path}"))
}

// bitbucketVCS determines the version control system for a
// Bitbucket repository, by using the Bitbucket API.
func bitbucketVCS(match map[string]string) error {
	if err := noVCSSuffix(match); err != nil {
		return err
	}

	var resp struct {
		SCM string `json:"scm"`
	}
	url := expand(match, "https://api.bitbucket.org/1.0/repositories/{bitname}")
	data, err := httpGET(url)
	if err != nil {
		if httpErr, ok := err.(*httpError); ok && httpErr.statusCode == 403 {
			// this may be a private repository. If so, attempt to determine which
			// VCS it uses. See issue 5375.
			root := match["root"]
			for _, vcs := range []string{"git", "hg"} {
				if ByCmd(vcs).Ping("https", root) == nil {
					resp.SCM = vcs
					break
				}
			}
		}

		if resp.SCM == "" {
			return err
		}
	} else {
		if err := json.Unmarshal(data, &resp); err != nil {
			return fmt.Errorf("decoding %s: %v", url, err)
		}
	}

	if ByCmd(resp.SCM) != nil {
		match["vcs"] = resp.SCM
		if resp.SCM == "git" {
			match["repo"] += ".git"
		}
		return nil
	}

	return fmt.Errorf("unable to detect version control system for bitbucket.org/ path")
}

// launchpadVCS solves the ambiguity for "lp.net/project/foo". In this case,
// "foo" could be a series name registered in Launchpad with its own branch,
// and it could also be the name of a directory within the main project
// branch one level up.
func launchpadVCS(match map[string]string) error {
	if match["project"] == "" || match["series"] == "" {
		return nil
	}
	_, err := httpGET(expand(match, "https://code.launchpad.net/{project}{series}/.bzr/branch-format"))
	if err != nil {
		match["root"] = expand(match, "launchpad.net/{project}")
		match["repo"] = expand(match, "https://{root}")
	}
	return nil
}
