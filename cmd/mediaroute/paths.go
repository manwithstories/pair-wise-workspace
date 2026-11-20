package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveInput resolves a user-supplied input path: it must exist and
// be a regular file (symlinks are resolved).
func resolveInput(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	r, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("input: %w", err)
	}
	fi, err := os.Stat(r)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("input %s is not a regular file", p)
	}
	return r, nil
}

// resolveOutput resolves a user-supplied output path. The parent
// directory must exist; an existing symlink at the target must not
// escape its directory.
func resolveOutput(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(abs)
	rdir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("output directory: %w", err)
	}
	target := filepath.Join(rdir, filepath.Base(abs))
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		r, err := filepath.EvalSymlinks(target)
		if err != nil {
			return "", err
		}
		if filepath.Dir(r) != rdir {
			return "", fmt.Errorf("refusing symlink escape: %s", p)
		}
	}
	return target, nil
}

// resolveDir resolves a directory path and ensures it exists.
func resolveDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	r, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("directory: %w", err)
	}
	fi, err := os.Stat(r)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", p)
	}
	return r, nil
}

// inDir resolves name inside base and rejects escapes.
func inDir(base, name string) (string, error) {
	p := filepath.Join(base, name)
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil // new file inside base
		}
		return "", err
	}
	if r != p && !strings.HasPrefix(r, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing symlink escape: %s", name)
	}
	return r, nil
}
