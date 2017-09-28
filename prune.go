package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/imikushin/prune/util"
	"github.com/urfave/cli"
)

var Version string = "v0.1.0-dev"

func main() {
	app := cli.NewApp()
	app.Version = Version
	app.Author = "@imikushin"
	app.Description = "Remove unused packages and files from your Go project's ./vendor dir"
	app.Usage = "prune ./vendor/"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "directory, C",
			Value: ".",
			Usage: "The directory in which to run",
		},
		cli.BoolFlag{
			Name:  "debug, d",
			Usage: "Debug logging",
		},
		cli.StringFlag{
			Name:   "target, T",
			Value:  "vendor",
			Hidden: true,
			Usage:  "The directory to store results",
		},
		cli.StringFlag{
			Name:   "gopath",
			Hidden: true,
			EnvVar: "GOPATH",
		},
	}
	app.Action = run

	app.Run(os.Args)
}

var gopath string

func run(c *cli.Context) error {
	if c.Bool("debug") {
		logrus.SetLevel(logrus.DebugLevel)
	}

	dir := c.String("directory")
	targetDir := c.String("target")
	gopath = c.String("gopath")

	if err := os.Chdir(dir); err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	logrus.Debugf("dir: '%s'", dir)

	return cleanup(dir, targetDir)
}

func parentPackages(root, p string) util.Packages {
	r := util.Packages{}
	lenRoot := len(root)
	for len(p) > lenRoot {
		r[p] = true
		p, _ = path.Split(p)
		if len(p) > 0 && p[len(p)-1] == '/' {
			p = p[:len(p)-1]
		}
	}
	return r
}

func listImports(rootPackage, libRoot, pkg string) <-chan util.Packages {
	pkgPath := "."
	if pkg != rootPackage {
		if strings.HasPrefix(pkg, rootPackage+"/") {
			pkgPath = pkg[len(rootPackage)+1:]
		} else {
			pkgPath = libRoot + "/" + pkg
		}
	}
	logrus.Debugf("listImports, pkgPath: '%s'", pkgPath)
	sch := make(chan string)
	noVendoredTests := func(info os.FileInfo) bool {
		if strings.HasPrefix(pkgPath, libRoot+"/") && strings.HasSuffix(info.Name(), "_test.go") {
			return false
		}
		return true
	}
	go func() {
		defer close(sch)

		// Gather all the Go imports
		ps, err := parser.ParseDir(token.NewFileSet(), pkgPath, noVendoredTests, parser.ImportsOnly)
		if err != nil {
			if os.IsNotExist(err) {
				logrus.Debugf("listImports, pkgPath does not exist: %s", err)
			} else {
				logrus.Errorf("Error parsing imports, pkgPath: '%s', err: '%s'", pkgPath, err)
			}
			return
		}
		logrus.Infof("Collecting imports for package '%s'", pkg)
		for _, p := range ps {
			for _, f := range p.Files {
				for _, v := range f.Imports {
					imp := v.Path.Value[1 : len(v.Path.Value)-1]
					if pkgComponents := strings.Split(imp, "/"); !strings.Contains(pkgComponents[0], ".") {
						continue
					} else if pkgComponents[0] == "." || pkgComponents[0] == ".." {
						imp = filepath.Clean(filepath.Join(pkg, imp))
					}
					if imp == rootPackage || strings.HasPrefix(imp, rootPackage+"/") {
						continue
					}
					sch <- imp
					logrus.Debugf("listImports, sch <- '%s'", v.Path.Value[1:len(v.Path.Value)-1])
				}
			}
		}
		// Gather all the CGO imports
		ps, err = parser.ParseDir(token.NewFileSet(), pkgPath, noVendoredTests, parser.ParseComments)
		if err != nil {
			if os.IsNotExist(err) {
				logrus.Debugf("listImports, pkgPath does not exist: %s", err)
			} else {
				logrus.Errorf("Error parsing comments, pkgPath: '%s', err: '%s'", pkgPath, err)
			}
			return
		}
		logrus.Infof("Collecting CGO imports for package '%s'", pkg)
		for _, p := range ps {
			for _, f := range p.Files {
				// Drill down to locate C preable definitions
				for _, decl := range f.Decls {
					d, ok := decl.(*ast.GenDecl)
					if !ok {
						continue
					}
					for _, spec := range d.Specs {
						s, ok := spec.(*ast.ImportSpec)
						if !ok || s.Path.Value != `"C"` {
							continue
						}
						cg := s.Doc
						if cg == nil && len(d.Specs) == 1 {
							cg = d.Doc
						}
						if cg != nil {
							// Extract any includes from the preamble
							for _, line := range strings.Split(cg.Text(), "\n") {
								if line = strings.TrimSpace(line); strings.HasPrefix(line, "#include \"") {
									if includePath := filepath.Dir(line[10 : len(line)-1]); includePath != "." {
										if _, err := os.Stat(filepath.Join(pkgPath, includePath)); !os.IsNotExist(err) {
											sch <- filepath.Clean(filepath.Join(pkg, includePath))
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}()
	lnc := util.MergeStrChans(sch, util.OneStr(pkg))
	return chanPackagesFromLines(lnc)
}

func chanPackagesFromLines(lnc <-chan string) <-chan util.Packages {
	return util.ChanPackages(func() util.Packages {
		r := util.Packages{}
		for v := range lnc {
			r[v] = true
		}
		return r
	})
}

func listPackages(rootPackage, targetDir string) util.Packages {
	r := util.Packages{}
	filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			logrus.Warning(err)
			return err
		}
		if !info.IsDir() {
			return nil
		}
		if path == targetDir ||
			strings.HasSuffix(path, targetDir+"/") ||
			path != "." && strings.HasPrefix(path[strings.LastIndex(path, "/")+1:], ".") {
			return filepath.SkipDir
		}
		logrus.Debugf("path: '%s'", path)
		pkgs, err := parser.ParseDir(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
		if err != nil {
			logrus.Error(err)
			return err
		}
		if len(pkgs) > 0 {
			logrus.Debugf("Adding package: '%s'", path)
			if path == "." {
				r[rootPackage] = true
			} else {
				r[rootPackage+"/"+path] = true
			}
		}
		return nil
	})
	return r
}

func collectImports(rootPackage, libRoot, targetDir string) util.Packages {
	logrus.Infof("Collecting packages in '%s'", rootPackage)

	imports := util.Packages{}
	packages := listPackages(rootPackage, targetDir)

	seenPackages := util.Packages{}
	for len(packages) > 0 {
		cs := []<-chan util.Packages{}
		for p := range packages {
			cs = append(cs, listImports(rootPackage, libRoot, p))
		}
		for ps := range util.MergePackagesChans(cs...) {
			imports.Merge(ps)
		}
		seenPackages.Merge(packages)
		packages = util.Packages{}
		for i := range imports {
			if !seenPackages[i] {
				packages[i] = true
			}
		}
	}

	for p := range imports {
		logrus.Debugf("Keeping: '%s'", p)
	}

	logrus.Debugf("imports len: %v", len(imports))
	return imports
}

func removeUnusedImports(imports util.Packages, targetDir string) error {
	importsParents := util.Packages{}
	for i := range imports {
		importsParents.Merge(parentPackages("", i))
	}
	return filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		logrus.Debugf("removeUnusedImports, path: '%s', err: '%v'", path, err)
		if os.IsNotExist(err) {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		if path == targetDir {
			return nil
		}
		if !info.IsDir() {
			pkg := path[len(targetDir+"/"):strings.LastIndex(path, "/")]
			if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".go") && !imports[pkg] {
				logrus.Debugf("Removing unused source file: '%s'", path)
				if err := os.Remove(path); err != nil {
					if os.IsNotExist(err) {
						return nil
					}
					logrus.Errorf("Error removing file: '%s', err: '%v'", path, err)
					return err
				}
			}
			return nil
		}
		pkg := path[len(targetDir+"/"):]
		if !imports[pkg] && !importsParents[pkg] {
			logrus.Infof("Removing unused dir: '%s'", path)
			err := os.RemoveAll(path)
			if err == nil {
				return filepath.SkipDir
			}
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			logrus.Errorf("Error removing unused dir, path: '%s', err: '%v'", path, err)
			return err
		}
		return nil
	})
}

func removeEmptyDirs(targetDir string) error {
	for count := 1; count != 0; {
		count = 0
		if err := filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
			logrus.Debugf("removeEmptyDirs, path: '%s', err: '%v'", path, err)
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			if err != nil {
				return err
			}
			if path == targetDir {
				return nil
			}
			if info.IsDir() {
				err := os.Remove(path)
				if err == nil {
					logrus.Infof("Removed Empty dir: '%s'", path)
					count++
					return filepath.SkipDir
				}
				if os.IsNotExist(err) {
					return filepath.SkipDir
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func guessRootPackage(dir string) string {
	logrus.Warnf("GOPATH is '%s'", gopath)
	if gopath == "" || strings.Contains(gopath, ":") {
		logrus.Fatalf("GOPATH not set or is not a single path")
	}
	srcPath := filepath.Clean(path.Join(gopath, "src"))
	if !strings.HasPrefix(dir, srcPath+"/") {
		logrus.Fatalf("Your project dir is not a subdir of $GOPATH/src")
	}
	if _, err := os.Stat(srcPath); err != nil {
		logrus.Fatalf("It didn't work: $GOPATH/src does not exist or something: %s", err)
	}
	logrus.Debugf("srcPath: '%s'", srcPath)
	return dir[len(srcPath+"/"):]
}

func cleanup(dir, targetDir string) error {
	rootPackage := guessRootPackage(dir)

	logrus.Debugf("rootPackage: '%s'", rootPackage)

	os.Chdir(dir)

	imports := collectImports(rootPackage, targetDir, targetDir)
	if err := removeUnusedImports(imports, targetDir); err != nil {
		logrus.Errorf("Error removing unused dirs: %v", err)
	}
	if err := removeEmptyDirs(targetDir); err != nil {
		logrus.Errorf("Error removing empty dirs: %v", err)
	}
	return nil
}
