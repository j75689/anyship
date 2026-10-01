package vps

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"time"
)

// excludedDirs are never uploaded as part of a build context.
var excludedDirs = []string{".git", "node_modules", ".anyship", ".wrangler"}

// bundle is everything uploaded to the deployment directory on the host.
type bundle struct {
	compose []byte
	// secrets maps secret names to values supplied by the deployer.
	secrets map[string]string
	// contexts maps a service name to its local build context directory.
	contexts map[string]string
	// extra maps archive paths to generated files, written after the contexts
	// so they take precedence.
	extra map[string][]byte
}

// write streams the bundle as a tar archive, in a stable entry order.
func (b *bundle) write(w io.Writer) error {
	tw := tar.NewWriter(w)
	epoch := time.Unix(0, 0)

	if err := writeFile(tw, "compose.yaml", b.compose, 0o644, epoch); err != nil {
		return err
	}
	if len(b.secrets) > 0 {
		// The directory keeps other host users out; the files stay readable so
		// containers running as non-root users can read their bind-mounted secrets.
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "secrets/", Mode: 0o700, ModTime: epoch}); err != nil {
			return err
		}
		for _, name := range sortedKeys(b.secrets) {
			if err := writeFile(tw, "secrets/"+name, []byte(b.secrets[name]), 0o644, epoch); err != nil {
				return err
			}
		}
	}
	for _, service := range sortedKeys(b.contexts) {
		if err := writeDir(tw, b.contexts[service], contextDir(service)); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(b.extra) {
		if err := writeFile(tw, name, b.extra[name], 0o644, epoch); err != nil {
			return err
		}
	}
	return tw.Close()
}

func writeFile(tw *tar.Writer, name string, data []byte, mode int64, mtime time.Time) error {
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data)), ModTime: mtime}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// writeDir adds the tree under root to the archive at prefix, skipping
// dependency and tool directories.
func writeDir(tw *tar.Writer, root, prefix string) error {
	return filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && p != root && slices.Contains(excludedDirs, entry.Name()) {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil // sockets, devices and the like have no place in a build context
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = path.Join(prefix, filepath.ToSlash(rel))
		if info.IsDir() {
			header.Name += "/"
		}
		header.Uname, header.Gname = "", ""
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }() // read-only; a close error can't lose data
		_, err = io.Copy(tw, f)
		return err
	})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
