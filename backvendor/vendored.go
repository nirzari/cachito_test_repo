// Copyright (C) 2018 Tim Waugh
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package backvendor

import (
	"bytes"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/pkg/errors"
)

func pathStartsWith(dir, prefix string) bool {
	return strings.HasPrefix(dir, prefix) &&
		(len(dir) == len(prefix) || dir[len(prefix)] == filepath.Separator)
}

type vendoredSearch struct {
	// Path to the "vendor" directory
	vendor string

	// Path to last project identified
	lastdir string

	// Vendored packages, indexed by Root
	vendored map[string]*RepoPath
}

func (s *vendoredSearch) inLastDir(pth string) bool {
	return s.lastdir != "" && pathStartsWith(pth, s.lastdir)
}

func processVendoredSource(src *GoSource, search *vendoredSearch, pth string) error {
	// For .go source files, see which directory they are in
	thisImport := filepath.Dir(pth[1+len(search.vendor):])
	repoRoot, err := src.RepoPathForImportPath(thisImport)
	if err != nil {
		return err
	}

	// The project name is relative to the vendor dir
	search.vendored[repoRoot.Root] = repoRoot
	search.lastdir = filepath.Join(search.vendor, repoRoot.Root)
	return nil
}

// VendoredProjects return a map of project import names to information
// about those projects, including which version control system they use.
func (src GoSource) VendoredProjects() (map[string]*RepoPath, error) {
	search := vendoredSearch{
		vendor:   src.Vendor(),
		vendored: make(map[string]*RepoPath),
	}
	walkfn := func(pth string, info os.FileInfo, err error) error {
		if err != nil {
			// Stop on error
			return err
		}

		// Ignore paths within the last project we identified
		if search.inLastDir(pth) {
			return nil
		}

		// Ignore anything except Go source
		if !info.Mode().IsRegular() || !strings.HasSuffix(pth, ".go") {
			return nil
		}

		// Identify the project
		return processVendoredSource(&src, &search, pth)
	}

	if _, err := os.Stat(src.Path); err != nil {
		return nil, err
	}

	_, err := os.Stat(search.vendor)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		err = filepath.Walk(search.vendor, walkfn)
		if err != nil {
			return nil, err
		}
	}

	return search.vendored, nil
}

// updateHashesAfterStrip syncs the tree to tag or revision ref and
// recalculates file hashes for the provided paths based on stripping
// import comments (in the same way as godep).  The boolean return
// value indicates whether any of the supplied hashes were modified as
// a result.
func updateHashesAfterStrip(hashes *FileHashes, wt WorkingTree, ref string, paths []string) (bool, error) {
	// Update working tree to match the ref
	err := wt.RevSync(ref)
	if err != nil {
		return false, errors.Wrapf(err, "RevSync to %s", ref)
	}

	anyChanged := false
	for _, path := range paths {
		w := bytes.NewBuffer(nil)
		changed, err := wt.StripImportComment(path, w)
		if err != nil {
			return false, err
		}
		if !changed {
			continue
		}

		// Write the altered content out to a file
		f, err := ioutil.TempFile("", "backvendor-strip.")
		if err != nil {
			return anyChanged, errors.Wrap(err, "updating hash")
		}

		// Remove the new file after we've hashed it
		defer os.Remove(f.Name())

		// Write to the file and close it, checking for errors
		_, err = w.WriteTo(f)
		if err != nil {
			f.Close() // ignore any secondary error
			return anyChanged, errors.Wrap(err, "updating hash")
		}

		if err = f.Close(); err != nil {
			return anyChanged, errors.Wrap(err, "updating hash")
		}

		// Re-hash the altered file
		h, err := hashes.h.Hash(path, f.Name())
		if err != nil {
			return anyChanged, err
		}
		hashes.hashes[path] = h
		anyChanged = true

	}

	return anyChanged, nil
}

func matchFromRefs(strip bool, hashes *FileHashes, wt WorkingTree, subPath string, refs []string) ([]string, error) {
	var paths []string
	if strip {
		for path := range hashes.hashes {
			paths = append(paths, filepath.Join(subPath, path))
		}
	}

	matchFromRef := func(th *FileHashes, ref string) (bool, error) {
		if hashes.IsSubsetOf(th) {
			return true, nil
		}

		if !strip {
			return false, nil
		}

		for _, path := range paths {
			if _, ok := th.hashes[path]; !ok {
				// File missing from revision
				return false, nil
			}
		}

		changed, err := updateHashesAfterStrip(th, wt, ref, paths)
		if err != nil {
			return false, err
		}

		return changed && hashes.IsSubsetOf(th), nil
	}

	matches := make([]string, 0)
	for _, ref := range refs {
		log.Debugf("%s: trying match", ref)
		refHashes, err := wt.FileHashesFromRef(ref, subPath)
		if err != nil {
			if err == ErrorInvalidRef {
				continue
			}
			return nil, err
		}
		ok, err := matchFromRef(refHashes, ref)
		if err != nil {
			return nil, err
		}
		if ok {
			matches = append(matches, ref)
		} else if len(matches) > 0 {
			// This is the end of a matching run of refs
			break
		}
	}

	if len(matches) == 0 {
		return nil, ErrorVersionNotFound
	}

	return matches, nil
}

// Reference describes the origin of a vendored project.
type Reference struct {
	// Tag is the semver tag within the upstream repository which
	// corresponds exactly to the vendored copy of the project. If
	// no tag corresponds Tag is "".
	Tag string

	// Rev is the upstream revision from which the vendored
	// copy was taken. If this is not known Reference is "".
	Rev string

	// Ver is the semantic version or pseudo-version for the
	// commit named in Reference. This is Tag if Tag is not "".
	Ver string
}

// chooseBestTag takes a sorted list of tags and returns the oldest
// semver tag which is not a prerelease, or else the oldest tag.
func chooseBestTag(tags []string) string {
	for i := len(tags) - 1; i >= 0; i-- {
		tag := tags[i]
		v, err := semver.NewVersion(tag)
		if err != nil {
			continue
		}
		if v.Prerelease() == "" {
			log.Debugf("best from %v: %v (no prerelease)", tags, tag)
			return tag
		}
	}

	tag := tags[len(tags)-1]
	log.Debugf("best from %v: %v (earliest)", tags, tag)
	return tag
}

// DescribeProject attempts to identify the tag in the version control
// system which corresponds to the project, based on comparison with
// files in dir. Vendored files and files whose names begin with "."
// are ignored.
func (src GoSource) DescribeProject(project *RepoPath, dir string) (*Reference, error) {
	wt, err := NewWorkingTree(&project.RepoRoot)
	if err != nil {
		return nil, err
	}
	defer wt.Close()

	// Make a local copy of src.excludes we can add keys to
	excludes := make(map[string]struct{})
	for key := range src.excludes {
		excludes[key] = struct{}{}
	}

	// Ignore vendor directory
	excludes[filepath.Join(dir, "vendor")] = struct{}{}

	// Work out how to hash files ready for comparison
	hasher, ok := NewHasher(project.VCS.Cmd)
	if !ok {
		return nil, ErrorUnknownVCS
	}

	// Work out the sub-directory within the repository root to
	// use for comparison.
	subPath := project.SubPath
	projDir := filepath.Join(project.Root, subPath)
	log.Debugf("describing %s compared to %s", dir, projDir)

	// Compute the hashes of the local files
	hashes, err := NewFileHashes(hasher, dir, excludes)
	if err != nil {
		return nil, err
	}

	for path, _ := range hashes.hashes {
		// Ignore dot files (e.g. .git)
		if strings.HasPrefix(path, ".") {
			delete(hashes.hashes, path)
		}
	}

	if len(hashes.hashes) == 0 {
		return nil, ErrorNoFiles
	}
	// If godep is in use, strip import comments from the
	// project's vendored files (but not files from the top-level
	// project).
	strip := src.usesGodep && dir != src.Path

	// First try to match against a specific version, if specified
	if project.Version != "" {
		matches, err := matchFromRefs(strip, hashes, wt,
			subPath, []string{project.Version})
		switch err {
		case nil:
			// Found a match
			match := matches[0]
			log.Debugf("Found match for %q which matches dependency management version", match)
			ver, err := PseudoVersion(wt, match)
			if err != nil {
				return nil, err
			}

			return &Reference{
				Rev: match,
				Ver: ver,
			}, nil
		case ErrorVersionNotFound:
			// No match, carry on
		default:
			// Some other error, fail
			return nil, err
		}
	}

	// Second try matching against tags for semantic versions
	tags, err := wt.VersionTags()
	if err != nil {
		return nil, err
	}

	matches, err := matchFromRefs(strip, hashes, wt, subPath, tags)
	switch err {
	case nil:
		// Found a match
		match := chooseBestTag(matches)
		rev, err := wt.RevisionFromTag(match)
		if err != nil {
			return nil, err
		}

		return &Reference{
			Tag: match,
			Rev: rev,
			Ver: match,
		}, nil
	case ErrorVersionNotFound:
		// No match, carry on
	default:
		// Some other error, fail
		return nil, err
	}

	// Third try each revision
	revs, err := wt.Revisions()
	if err != nil {
		return nil, err
	}

	matches, err = matchFromRefs(strip, hashes, wt, subPath, revs)
	if err != nil {
		return nil, err
	}

	// Use newest matching revision
	rev := matches[0]
	ver, err := PseudoVersion(wt, rev)
	if err != nil {
		return nil, err
	}

	return &Reference{
		Rev: rev,
		Ver: ver,
	}, nil
}

// DescribeVendoredProject attempts to identify the tag in the version
// control system which corresponds to the vendored copy of the
// project.
func (src GoSource) DescribeVendoredProject(project *RepoPath) (*Reference, error) {
	projRootImportPath := filepath.FromSlash(project.Root)
	projDir := filepath.Join(src.Vendor(), projRootImportPath)
	ref, err := src.DescribeProject(project, projDir)
	return ref, err
}
