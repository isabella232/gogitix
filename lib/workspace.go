package lib

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/fatih/color"

	"log"

	"gopkg.in/launchdarkly/gogitix.v2/lib/utils"
)

type Workspace struct {
	GitDir              string   // Original git directory
	WorkDir             string   // Base of the temporary directory created with git index
	RootDir             string   // Base directory of the top-level go package in the git index
	UpdatedDirs         []string // Directories that have changed and still exist (sorted)
	UpdatedTrees        []string // Top directories that have changed and still exist (sorted)
	UpdatedFiles        []string // Files that have changed and still exist
	UpdatedPackages     []string // Packages that have changed and still exist
	LocallyChangedFiles []string // Files where the git index differs from what's in the working tree
	deleteOnClose       bool     // whether to delete the workspace when we are done
}

func Start(gitRoot string, pathSpec []string, useLndir bool, gitRevSpec string, staging bool) (Workspace, error) {
	workDir := gitRoot
	rootDir := gitRoot
	rootPackage := strings.TrimSpace(MustRunCmd("sh", "-c", fmt.Sprintf("cd %s && go list -e .", gitRoot)))

	// If we need to make a copy for staging of a revspec
	if gitRevSpec != "" || staging {
		var err error
		workDir, err = ioutil.TempDir("", path.Base(os.Args[0]))
		if err != nil {
			return Workspace{}, err
		}

		workDir, _ = filepath.EvalSymlinks(workDir)

		if err := os.Setenv("GOPATH", strings.Join([]string{workDir, os.Getenv("GOPATH")}, ":")); err != nil {
			return Workspace{}, err
		}

		rootDir = path.Join(workDir, "src", rootPackage)
	}

	yellow := color.New(color.FgYellow)
	yellow.Printf("Identifying changed files.")
	ticker := time.NewTicker(500 * time.Millisecond)
	defer func() {
		ticker.Stop()
		yellow.Printf("\n")
	}()

	go func() {
		for {
			_, ok := <-ticker.C
			if !ok {
				break
			}
			yellow.Printf(".")
		}
	}()

	updatedFilesChan := make(chan []string, 1)
	locallyChangedFilesChan := make(chan []string, 1)
	updatedDirsChan := make(chan []string, 1)

	go func() {
		updatedFilesChan <- getUpdatedFiles(gitRoot, pathSpec, gitRevSpec, staging)
	}()

	go func() {
		locallyChangedFilesChan <- getLocallyChangedFiles(gitRoot, pathSpec)
	}()

	go func() {
		updatedDirsChan <- getUpdatedDirs(gitRoot, pathSpec, gitRevSpec, staging)
	}()

	// Try to create a shadow copy instead of checking out all the files
	lndir := ""
	lndirArgs := []string{"-silent"}
	if useLndir {
		if _, err := RunCmd("which", "go-lndir"); err == nil {
			lndir = "go-lndir"
			lndirArgs = append(lndirArgs, "-gitignore")
		} else if _, err := RunCmd("which", "lndir"); err == nil {
			lndir = "lndir"
		} else {
			Failf("Unable to find go-lndir or lndir")
		}
	}

	// Check out revSpec to test if we've been given one
	if gitRevSpec != "" {
		shas := MustRunCmd("git", "-C", gitRoot, "rev-list", gitRevSpec)
		if len(shas) == 0 {
			log.Fatalf(`Could not find any SHAS in range "%s"`, gitRevSpec)
		}
		mostRecentSha := strings.Fields(shas)[0]
		if err := os.MkdirAll(rootDir, os.ModePerm); err != nil {
			return Workspace{}, err
		}
		MustRunCmd("git", "-C", gitRoot, "--work-tree", rootDir, "checkout", mostRecentSha, "--", ".")
	} else if staging {
		if lndir != "" {
			absGitRoot, err := filepath.Abs(gitRoot)
			if err != nil {
				return Workspace{}, err
			}
			if err := os.MkdirAll(rootDir, os.ModePerm); err != nil {
				return Workspace{}, err
			}
			// Start with a copy of the current workspace
			MustRunCmd(lndir, append(lndirArgs, absGitRoot, rootDir)...)

			// Copy out any files that have local changes from the index
			cmd := fmt.Sprintf("git ls-files --modified --deleted | git checkout-index --stdin -f --prefix %s/", rootDir)
			MustRunCmd("sh", "-c", cmd)

			// Finally, copy out the files we want to test
			MustRunCmd("git", "-C", gitRoot, "checkout-index", "-f", "--prefix", rootDir+"/")
		} else {
			MustRunCmd("git", "-C", gitRoot, "checkout-index", "-a", "--prefix", rootDir+"/")
		}
	}

	if err := os.Chdir(rootDir); err != nil {
		return Workspace{}, err
	}

	updatedDirs := <-updatedDirsChan
	updatedPackages := getUpdatedPackages(rootPackage, updatedDirs)

	updatedFiles := <-updatedFilesChan
	locallyChangedFiles := <-locallyChangedFilesChan

	return Workspace{
		GitDir:              gitRoot,
		WorkDir:             workDir,
		RootDir:             rootDir,
		UpdatedFiles:        utils.SortStrings(updatedFiles),
		UpdatedDirs:         utils.SortStrings(updatedDirs),
		UpdatedPackages:     utils.SortStrings(updatedPackages),
		UpdatedTrees:        utils.SortStrings(utils.ShortestPrefixes(updatedDirs)),
		LocallyChangedFiles: utils.SortStrings(locallyChangedFiles),
		deleteOnClose:       gitRevSpec != "" || staging,
	}, nil
}
func getLocallyChangedFiles(gitRoot string, pathSpec []string) []string {
	return strings.Fields(MustRunCmd("git", append([]string{"-C", gitRoot, "diff", "--name-only", "--diff-filter=ACMR", "--"}, pathSpec...)...))
}

func getUpdatedFiles(gitRoot string, pathSpec []string, gitRevSpec string, staging bool) []string {
	diffCmd := []string{"diff", "--name-only", "--diff-filter=ACMR"}
	if gitRevSpec != "" {
		diffCmd = append(diffCmd, gitRevSpec)
	} else if staging {
		diffCmd = append(diffCmd, "--cached")
	} else {
		diffCmd = append(diffCmd, "HEAD")
	}
	diffCmd = append(diffCmd, "--")
	diffCmd = append(diffCmd, pathSpec...)
	return strings.Fields(MustRunCmd("git", append([]string{"-C", gitRoot}, diffCmd...)...))
}

func (ws Workspace) Close() error {
	if !ws.deleteOnClose {
		return nil
	}
	return os.RemoveAll(ws.WorkDir)
}

// Must be run in rootDir
func getUpdatedPackages(rootPackage string, updatedDirs []string) []string {
	packages := strings.Fields(MustRunCmd("go", "list", "./..."))
	updatedPackages := map[string]bool{}

	updatedDirMap := utils.StrMap(updatedDirs)

	for _, p := range packages {
		dirName := strings.TrimPrefix(p, rootPackage+"/")
		if dirName == rootPackage {
			dirName = "."
		}
		if updatedDirMap[dirName] {
			updatedPackages[p] = true
		}
	}

	return utils.StrKeys(updatedPackages)
}

func getUpdatedDirs(gitRoot string, pathSpec []string, gitRevSpec string, staging bool) []string {
	diffCmd := []string{"diff", "--name-status", "--diff-filter=ACDMR"}
	if gitRevSpec != "" {
		diffCmd = append(diffCmd, gitRevSpec)
	} else if staging {
		diffCmd = append(diffCmd, "--cached")
	} else {
		diffCmd = append(diffCmd, "HEAD")
	}
	diffCmd = append(diffCmd, "--")
	diffCmd = append(diffCmd, pathSpec...)
	fileStatus := MustRunCmd("git", append([]string{"-C", gitRoot}, diffCmd...)...)
	scanner := bufio.NewScanner(strings.NewReader(fileStatus))
	var allFiles []string
	for scanner.Scan() {
		allFiles = append(allFiles, strings.Fields(scanner.Text())[1:]...)
	}
	updatedDirs := map[string]bool{}
	for _, f := range allFiles {
		updatedDirs[filepath.Dir(f)] = true
	}
	// Keep only the directories that still exist
	existingDirs := []string{}
	for d := range updatedDirs {
		if _, err := os.Stat(d); err == nil {
			existingDirs = append(existingDirs, d)
		}
	}

	return existingDirs
}
