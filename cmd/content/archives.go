package content

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// A startup file whose source is `__static__/<folder>.tar.gz` (or .tgz, .tar)
// is built by labctl from the sibling `<folder>/` directory of the declaring
// index.md (or manifest.yaml) before every push - the folder is the source of
// truth, the archive is just a build artifact that gets pushed like any other
// static file. If there is no such folder, the source is left alone.
var archiveSourceRe = regexp.MustCompile(`^__static__/([^/]+)\.(tar\.gz|tgz|tar)$`)

// archiveModTime keeps the archives reproducible: unchanged sources yield the
// same bytes, so an untouched archive is never re-pushed. Fixed and positive,
// so GNU tar doesn't warn about implausibly old files.
var archiveModTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// createStartupFileArchives (re)builds every startup file archive declared in
// the content directory and returns the folders they were built from.
func createStartupFileArchives(dir string) ([]string, error) {
	files, err := listFiles(dir)
	if err != nil {
		return nil, err
	}

	var folders []string
	built := map[string]bool{}

	for _, file := range files {
		sources, err := startupFileSources(file)
		if err != nil {
			return nil, fmt.Errorf("couldn't read startup files from %s: %w", file, err)
		}

		for _, source := range sources {
			m := archiveSourceRe.FindStringSubmatch(source)
			if m == nil {
				continue
			}

			folder := filepath.Join(filepath.Dir(file), m[1])
			if info, err := os.Stat(folder); err != nil || !info.IsDir() {
				continue
			}

			archive := filepath.Join(filepath.Dir(file), "__static__", m[1]+"."+m[2])
			if built[archive] {
				continue
			}
			if err := writeArchive(archive, folder, m[2] != "tar"); err != nil {
				return nil, fmt.Errorf("couldn't build %s: %w", archive, err)
			}

			built[archive] = true
			folders = append(folders, folder)
		}
	}

	return folders, nil
}

type startupFileSourcesDoc struct {
	StartupFiles []struct {
		Source string `yaml:"source"`
	} `yaml:"startupFiles"`

	Machines []struct {
		StartupFiles []struct {
			Source string `yaml:"source"`
		} `yaml:"startupFiles"`
	} `yaml:"machines"`
}

// startupFileSources returns the `source` of every startup file declared in a
// playground manifest or in a content markdown file's front matter (its
// `playground:` block - unless it's just the playground's name). Other files
// declare none.
func startupFileSources(file string) ([]string, error) {
	base := filepath.Base(file)
	if !strings.HasSuffix(base, ".md") && base != "manifest.yaml" && base != "manifest.yml" {
		return nil, nil
	}

	data, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var doc startupFileSourcesDoc
	if strings.HasSuffix(base, ".md") {
		var front struct {
			Playground yaml.Node `yaml:"playground"`
		}
		if err := yaml.Unmarshal(frontMatter(data), &front); err != nil {
			return nil, err
		}
		if front.Playground.Kind != yaml.MappingNode {
			return nil, nil
		}
		if err := front.Playground.Decode(&doc); err != nil {
			return nil, err
		}
	} else if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}

	var sources []string
	for _, f := range doc.StartupFiles {
		sources = append(sources, f.Source)
	}
	for _, m := range doc.Machines {
		for _, f := range m.StartupFiles {
			sources = append(sources, f.Source)
		}
	}

	return sources, nil
}

// frontMatter returns the YAML between the leading "---" line and the next
// one, or nil when the file has no front matter.
func frontMatter(data []byte) []byte {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil
	}

	rest := data[len("---\n"):]
	end := bytes.Index(rest, []byte("\n---\n"))
	if end < 0 {
		return nil
	}

	return rest[:end+1]
}

// writeArchive builds the archive and writes it only if its bytes changed -
// rewriting an identical archive would make watch mode loop on its own writes.
func writeArchive(archive, folder string, compress bool) error {
	data, err := buildArchive(folder, compress)
	if err != nil {
		return err
	}

	if existing, err := os.ReadFile(archive); err == nil && bytes.Equal(existing, data) {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(archive), 0755); err != nil {
		return err
	}

	return os.WriteFile(archive, data, 0644)
}

// buildArchive tars the folder's regular files (the same set a push would
// see: .labctlignore inside the folder applies) with fixed metadata.
func buildArchive(folder string, compress bool) ([]byte, error) {
	files, err := listFiles(folder)
	if err != nil {
		return nil, err
	}
	slices.Sort(files)

	var buf bytes.Buffer
	var gz *gzip.Writer
	var tw *tar.Writer
	if compress {
		gz = gzip.NewWriter(&buf)
		tw = tar.NewWriter(gz)
	} else {
		tw = tar.NewWriter(&buf)
	}

	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}

		rel, err := filepath.Rel(folder, file)
		if err != nil {
			return nil, err
		}

		if err := tw.WriteHeader(&tar.Header{
			Name:    filepath.ToSlash(rel),
			Mode:    int64(info.Mode().Perm()),
			Size:    info.Size(),
			ModTime: archiveModTime,
		}); err != nil {
			return nil, err
		}

		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		if err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}
