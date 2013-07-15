// Copyright 2011 Google Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// App represents an entire Go App Engine app.
type App struct {
	Files        []*File    // the complete set of source files for this app
	Packages     []*Package // the packages
	RootPackages []*Package // the subset of packages with init functions

	PackageIndex map[string]*Package // index from import path to package object
}

// Package represents a Go package.
type Package struct {
	ImportPath   string     // the path under which this package may be imported
	Files        []*File    // the set of source files that form this package
	BaseDir      string     // what the file names are relative to, if outside app
	Dependencies []*Package // the packages that this directly depends upon, in no particular order
	HasInit      bool       // whether the package has any init functions
	Dupe         bool       // whether the package is is a duplicate
}

func (p *Package) String() string {
	return fmt.Sprintf("%+v", *p)
}

// Implement sort.Interface for []*Package.
type byImportPath []*Package

func (s byImportPath) Len() int           { return len(s) }
func (s byImportPath) Less(i, j int) bool { return s[i].ImportPath < s[j].ImportPath }
func (s byImportPath) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

type File struct {
	Name        string   // the file name
	PackageName string   // the package this file declares itself to be
	ImportPaths []string // import paths
	HasInit     bool     // whether the file has an init function
}

func (f *File) String() string {
	return fmt.Sprintf("%+v", *f)
}

// Implement sort.Interface for []*File.
type byFileName []*File

func (s byFileName) Len() int           { return len(s) }
func (s byFileName) Less(i, j int) bool { return s[i].Name < s[j].Name }
func (s byFileName) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// vfs is a tiny VFS overlay that exposes a subset of files in a tree.
type vfs struct {
	baseDir   string
	filenames []string
}

func (v vfs) readDir(dir string) (fis []os.FileInfo, err error) {
	dir = filepath.Clean(dir)
	for _, f := range v.filenames {
		f = filepath.Join(v.baseDir, f)
		if filepath.Dir(f) == dir {
			fi, err := os.Stat(f)
			if err != nil {
				return nil, err
			}
			fis = append(fis, fi)
		}
	}
	return fis, nil
}

// ParseFiles parses the named files, deduces their package structure,
// and returns the dependency DAG as an App.
// Elements of filenames are considered relative to baseDir.
func ParseFiles(baseDir string, filenames []string) (*App, error) {
	app := &App{
		PackageIndex: make(map[string]*Package),
	}
	pkgFiles := make(map[string][]*File) // app package name => its files

	vfs := vfs{baseDir, filenames}
	ctxt := &build.Context{
		GOARCH:      build.Default.GOARCH,
		GOOS:        build.Default.GOOS,
		GOROOT:      *goRoot,
		GOPATH:      baseDir,
		BuildTags:   []string{"appengine"},
		ReleaseTags: build.Default.ReleaseTags,
		Compiler:    "gc",
		HasSubdir: func(root, dir string) (rel string, ok bool) {
			// Override the default HasSubdir, which evaluates symlinks.
			const sep = string(filepath.Separator)
			root = filepath.Clean(root)
			if !strings.HasSuffix(root, sep) {
				root += sep
			}
			dir = filepath.Clean(dir)
			if !strings.HasPrefix(dir, root) {
				return "", false
			}
			return dir[len(root):], true
		},
		ReadDir: func(dir string) ([]os.FileInfo, error) {
			return vfs.readDir(dir)
		},
	}

	dirs := make(map[string]bool)
	for _, f := range filenames {
		dir := filepath.Dir(f) // "." for top-level files
		if dir == "" || dir == string(filepath.Separator) {
			return nil, fmt.Errorf("bad filename %q", f)
		}
		dirs[dir] = true
	}
	for dir := range dirs {
		pkg, err := ctxt.ImportDir(filepath.Join(baseDir, dir), 0)
		if _, ok := err.(*build.NoGoError); ok {
			// There were .go files, but they were all excluded (e.g. by "// +build ignore").
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed parsing dir %v: %v", dir, err)
		}

		for _, f := range pkg.GoFiles {
			filename := filepath.Join(dir, f)
			file, err := parseFile(baseDir, filename)
			if err != nil {
				return nil, err
			}
			app.Files = append(app.Files, file)
			pkgFiles[dir] = append(pkgFiles[dir], file)
		}
	}

	allowedDupes := make(map[string]bool)
	if *pkgDupes != "" {
		for _, pkg := range strings.Split(*pkgDupes, ",") {
			allowedDupes[pkg] = true
		}
	}

	// Create Package objects.
	for dirname, files := range pkgFiles {
		imp := filepath.ToSlash(dirname)
		if dirname == "." {
			// top-level package; generate random package name
			rng := rand.New(rand.NewSource(time.Now().Unix()))
			imp = fmt.Sprintf("main%05d", rng.Intn(1e5))
		}

		p := &Package{
			ImportPath: imp,
			Files:      files,
		}
		if p.ImportPath == "main" {
			return nil, errors.New("top-level main package is forbidden")
		}
		if isStandardPackage(p.ImportPath) {
			if !allowedDupes[p.ImportPath] {
				return nil, fmt.Errorf("package %q has the same name as a standard package", p.ImportPath)
			}
			p.Dupe = true
		}
		for _, f := range files {
			if f.HasInit {
				p.HasInit = true
				break
			}
		}
		app.Packages = append(app.Packages, p)
		if p.HasInit {
			app.RootPackages = append(app.RootPackages, p)
		}
		app.PackageIndex[p.ImportPath] = p
	}

	if *goPath != "" {
		var re *regexp.Regexp
		var err error
		if *noBuildFiles != "" {
			re, err = regexp.Compile(*noBuildFiles)
			if err != nil {
				return nil, fmt.Errorf("bad -nobuild_files: %v", err)
			}
		}
		if err := addFromGOPATH(app, re); err != nil {
			return nil, err
		}
	}

	// Populate dependency lists.
	for _, p := range app.Packages {
		imports := make(map[string]int) // ImportPath => 1
		for _, f := range p.Files {
			for _, path := range f.ImportPaths {
				imports[path] = 1
			}
		}
		p.Dependencies = make([]*Package, 0, len(imports))
		for path := range imports {
			pkg, ok := app.PackageIndex[path]
			if !ok {
				// A file declared an import we don't know.
				// It could be a package from the standard library.
				continue
			}
			p.Dependencies = append(p.Dependencies, pkg)
		}
		sort.Sort(byImportPath(p.Dependencies))
	}

	// Sort topologically.
	if err := topologicalSort(app.Packages); err != nil {
		return nil, err
	}

	return app, nil
}

// addFromGOPATH adds packages from GOPATH that are needed by the app.
func addFromGOPATH(app *App, noBuild *regexp.Regexp) error {
	warned := make(map[string]bool)
	for i := 0; i < len(app.Packages); i++ { // app.Packages is grown during this loop
		p := app.Packages[i]
		for _, f := range p.Files {
			for _, path := range f.ImportPaths {
				if isStandardPackage(path) || app.PackageIndex[path] != nil {
					continue
				}
				pkg, err := gopathPackage(path)
				if err != nil {
					if !warned[path] {
						log.Printf("Can't find package %q in $GOPATH: %v", path, err)
						warned[path] = true
					}
					continue
				}

				files := make([]*File, 0, len(pkg.GoFiles))
				for _, f := range pkg.GoFiles {
					if noBuild != nil && noBuild.MatchString(filepath.Join(path, f)) {
						continue
					}
					files = append(files, &File{
						Name:        f,
						PackageName: pkg.Name,
						// NOTE: This is inaccurate, but it is sufficient to
						// record all the package imports for each file.
						ImportPaths: pkg.Imports,
					})
				}
				if len(files) == 0 {
					return fmt.Errorf("package %s required, but all its files were excluded by nobuild_files", path)
				}
				p := &Package{
					ImportPath: path,
					Files:      files,
					BaseDir:    pkg.Dir,
				}
				app.Packages = append(app.Packages, p)
				app.PackageIndex[path] = p
			}
		}
	}
	return nil
}

// isInit returns whether the given function declaration is a true init function.
// Such a function must be called "init", not have a receiver, and have no arguments or return types.
func isInit(f *ast.FuncDecl) bool {
	ft := f.Type
	return f.Name.Name == "init" && f.Recv == nil && ft.Params.NumFields() == 0 && ft.Results.NumFields() == 0
}

// parseFile parses a single Go source file into a *File.
func parseFile(baseDir, filename string) (*File, error) {
	var fset token.FileSet
	file, err := parser.ParseFile(&fset, filepath.Join(baseDir, filename), nil, 0)
	if err != nil {
		return nil, err
	}

	// Walk the file's declarations looking for all the imports.
	// Determine whether the file has an init function at the same time.
	var imports []string
	hasInit := false
	for _, decl := range file.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.IMPORT {
			for _, spec := range genDecl.Specs {
				importSpec := spec.(*ast.ImportSpec)
				val := string(importSpec.Path.Value)
				path, err := strconv.Unquote(val)
				if err != nil {
					return nil, fmt.Errorf("parser: bad ImportSpec %q: %v", val, err)
				}
				if !checkImport(path) {
					return nil, fmt.Errorf("parser: bad import %q", path)
				}
				imports = append(imports, path)
			}
		}
		if funcDecl, ok := decl.(*ast.FuncDecl); ok {
			if isInit(funcDecl) {
				hasInit = true
			}
		}
	}

	// Check for untagged struct literals from the standard package library.
	ch := newCompLitChecker(&fset)
	ast.Walk(ch, file)
	if len(ch.errors) > 0 {
		return nil, ch.errors
	}

	return &File{
		Name:        filename,
		PackageName: file.Name.Name,
		ImportPaths: imports,
		HasInit:     hasInit,
	}, nil
}

var legalImportPath = regexp.MustCompile(`^[a-zA-Z0-9_\-./~]+$`)

// checkImport will return whether the provided import path is good.
func checkImport(path string) bool {
	if path == "" {
		return false
	}
	if len(path) > 1024 {
		return false
	}
	if filepath.IsAbs(path) || strings.Contains(path, "..") {
		return false
	}
	if !legalImportPath.MatchString(path) {
		return false
	}
	if path == "syscall" || path == "unsafe" {
		return false
	}
	return true
}

type compLitChecker struct {
	fset    *token.FileSet
	imports map[string]string // Local name => import path; only standard packages.
	errors  scanner.ErrorList // accumulated errors
}

func newCompLitChecker(fset *token.FileSet) *compLitChecker {
	return &compLitChecker{
		fset:    fset,
		imports: make(map[string]string),
	}
}

func (c *compLitChecker) errorf(node ast.Node, format string, a ...interface{}) {
	c.errors = append(c.errors, &scanner.Error{
		Pos: c.fset.Position(node.Pos()),
		Msg: fmt.Sprintf(format, a...),
	})
}

func (c *compLitChecker) Visit(node ast.Node) ast.Visitor {
	if imp, ok := node.(*ast.ImportSpec); ok {
		pth, _ := strconv.Unquote(imp.Path.Value)
		if !isStandardPackage(pth) {
			return c
		}
		if imp.Name != nil {
			id := imp.Name.Name
			if id == "." {
				return c
			}
			c.imports[id] = pth
		} else {
			// All standard packages have their last path component as their package name.
			c.imports[filepath.Base(pth)] = pth
		}
		return c
	}

	lit, ok := node.(*ast.CompositeLit)
	if !ok {
		return c
	}
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok {
		return c
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return c
	}
	pth, ok := c.imports[id.Name]
	if !ok {
		// This must be pkg.T for a package in the app.
		return c
	}

	// Check exception list.
	if untaggedLiteralWhitelist[pth+"."+sel.Sel.Name] {
		return c
	}

	allTags := true
	for _, elt := range lit.Elts {
		_, ok := elt.(*ast.KeyValueExpr)
		allTags = allTags && ok
	}
	if !allTags {
		c.errorf(lit, "composite struct literal %v.%v with untagged fields", pth, sel.Sel)
	}

	return c
}

// Cache of standard package status.
var stdPackageCache = make(map[string]bool)

// isStandardPackage reports whether import path s is a standard package.
func isStandardPackage(s string) bool {
	if std, ok := stdPackageCache[s]; ok {
		return std
	}

	// Don't consider any import path containing a dot to be a standard package.
	if strings.Contains(s, ".") {
		stdPackageCache[s] = false
		return false
	}

	ctxt := build.Default
	ctxt.GOROOT = *goRoot
	ctxt.Compiler = "gc"
	pkg, err := ctxt.Import(s, "/nowhere", build.FindOnly|build.AllowBinary)
	if err != nil {
		stdPackageCache[s] = false
		return false
	}
	std := pkg.ImportPath != ""
	stdPackageCache[s] = std
	return std
}

// gopathPackage imports information about a package in GOPATH.
func gopathPackage(s string) (*build.Package, error) {
	ctxt := build.Default
	ctxt.GOROOT = *goRoot
	ctxt.GOPATH = *goPath
	ctxt.BuildTags = append([]string{"appengine"}, ctxt.BuildTags...) // don't affect build.Default
	ctxt.Compiler = "gc"
	// Don't use FindOnly or AllowBinary because we want import information
	// and we require the source files.
	return ctxt.Import(s, "/nowhere", 0)
}

// topologicalSort sorts the given slice of *Package in topological order.
// The ordering is such that X comes before Y if X is a dependency of Y.
// A cyclic dependency graph is signalled by an error being returned.
func topologicalSort(p []*Package) error {
	selected := make(map[*Package]bool, len(p))
	for len(p) > 0 {
		// Sweep the working list and move the packages with no
		// selected dependencies to the front.
		//
		// n acts as both a count of the dependency-free packages,
		// and as the marker for the position of the first package
		// with a dependency that can be swapped to a later position.
		n := 0
	packageLoop:
		for i, pkg := range p {
			for _, dep := range pkg.Dependencies {
				if !selected[dep] {
					continue packageLoop
				}
			}
			selected[pkg] = true
			p[i], p[n] = p[n], pkg
			n++
		}
		if n == 0 {
			// No leaves, so there must be a cycle.
			cycle := findCycle(p)
			paths := make([]string, len(cycle)+1)
			for i, pkg := range cycle {
				paths[i] = pkg.ImportPath
			}
			paths[len(cycle)] = cycle[0].ImportPath // duplicate last package
			return fmt.Errorf("parser: cyclic dependency graph: %s", strings.Join(paths, " -> "))
		}
		p = p[n:]
	}
	return nil
}

// findCycle finds a cycle in pkgs.
// It assumes that a cycle exists.
func findCycle(pkgs []*Package) []*Package {
	pkgMap := make(map[*Package]bool, len(pkgs)) // quick index of packages
	var min *Package
	for _, pkg := range pkgs {
		pkgMap[pkg] = true
		if min == nil || pkg.ImportPath < min.ImportPath {
			min = pkg
		}
	}

	// Every element of pkgs is a member of a cycle,
	// so find a cycle starting with the first one lexically.
	cycle := []*Package{min}
	seen := map[*Package]int{min: 0} // map of package to index in cycle
	for {
		last := cycle[len(cycle)-1]
		for _, dep := range last.Dependencies {
			if i, ok := seen[dep]; ok {
				// Cycle found.
				return cycle[i:]
			}
		}
		// None of the dependencies of last are in cycle, so pick one of
		// its dependencies (that we know is in a cycle) to add to cycle.
		// We are always able to find such a dependency, because
		// otherwise last would not be a member of a cycle.
		var dep *Package
		for _, d := range last.Dependencies {
			if pkgMap[d] {
				dep = d
				break
			}
		}

		seen[dep] = len(cycle)
		cycle = append(cycle, dep)
	}
}

func init() {
	// Add some App Engine-specific entries to the untagged literal whitelist.
	untaggedLiteralWhitelist["appengine/datastore.PropertyList"] = true
	untaggedLiteralWhitelist["appengine.MultiError"] = true
}
