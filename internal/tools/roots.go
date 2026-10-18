package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Roots confines file access to a session's workspace: the working
// directory first, then additional directories. Access goes through
// os.Root, which rejects escapes via ".." and symlinks.
type Roots struct {
	roots []*os.Root
}

// OpenRoots opens each directory as a root.
func OpenRoots(dirs ...string) (*Roots, error) {
	r := &Roots{}
	for _, dir := range dirs {
		root, err := os.OpenRoot(filepath.Clean(dir))
		if err != nil {
			return nil, errors.Join(fmt.Errorf("open workspace root: %w", err), r.Close())
		}
		r.roots = append(r.roots, root)
	}
	if len(r.roots) == 0 {
		return nil, errors.New("no workspace roots")
	}
	return r, nil
}

// Cwd is the working directory.
func (r *Roots) Cwd() string { return r.roots[0].Name() }

// Close closes all roots.
func (r *Roots) Close() error {
	var errs []error
	for _, root := range r.roots {
		errs = append(errs, root.Close())
	}
	return errors.Join(errs...)
}

// target is a path resolved to the root containing it.
type target struct {
	root *os.Root
	rel  string // path within root
	abs  string // absolute path, for display and locations
}

// resolve maps a path from the model to its root. Relative paths are
// relative to the working directory; absolute paths must lie within a root.
func (r *Roots) resolve(path string) (target, error) {
	if path == "" {
		return target{}, errors.New("path is required")
	}
	if !filepath.IsAbs(path) {
		if !filepath.IsLocal(path) {
			return target{}, fmt.Errorf("path %q is outside the workspace", path)
		}
		return r.within(r.roots[0], filepath.Clean(path)), nil
	}
	for _, root := range r.roots {
		if rel, err := filepath.Rel(root.Name(), path); err == nil && filepath.IsLocal(rel) {
			return r.within(root, rel), nil
		}
	}
	return target{}, fmt.Errorf("path %q is outside the workspace", path)
}

func (r *Roots) within(root *os.Root, rel string) target {
	return target{root: root, rel: rel, abs: filepath.Join(root.Name(), rel)}
}

// display names a file found under t for the model: relative to the working
// directory when it is there, absolute otherwise.
func (r *Roots) display(root *os.Root, rel string) string {
	if root == r.roots[0] {
		return rel
	}
	return filepath.Join(root.Name(), rel)
}
