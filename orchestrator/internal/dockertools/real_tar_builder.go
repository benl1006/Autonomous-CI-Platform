package dockertools

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type RealTarBuilder struct{}

// Tars a workspace at src and streams it to pw.
func (tb *RealTarBuilder) TarWorkspace(pw *io.PipeWriter, src string) (err error) {
	tw := tar.NewWriter(pw)
	defer func() {
		if closeErr := tw.Close(); closeErr != nil {
			err = closeErr
		}
	}()

	err = filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) (tarErr error) {
		if walkErr != nil {
			return fmt.Errorf("Error while walking directory: %w", walkErr)
		}
		relPath, tarErr := filepath.Rel(src, path)
		if tarErr != nil {
			return fmt.Errorf("Failed create relative path %s/%s: %w", src, path, tarErr)
		}

		// Skip root
		if relPath == "." {
			return nil
		}

		fi, tarErr := d.Info()
		if tarErr != nil {
			return fmt.Errorf("Failed to get info for %q: %w", relPath, tarErr)
		}

		header, tarErr := tar.FileInfoHeader(fi, d.Name())
		if tarErr != nil {
			return fmt.Errorf("Failed to create file info header: %w", tarErr)
		}
		header.Name = filepath.ToSlash(relPath)

		if tarErr = tw.WriteHeader(header); tarErr != nil {
			return fmt.Errorf("Failed to write header: %w", tarErr)
		}

		if d.Type().IsRegular() {
			var file *os.File
			file, tarErr = os.Open(path)
			if tarErr != nil {
				return fmt.Errorf("Failed to open file %q: %w", relPath, tarErr)
			}
			defer func() {
				if closeErr := file.Close(); closeErr != nil {
					tarErr = closeErr
				}
			}()
			if _, tarErr = io.Copy(tw, file); tarErr != nil {
				return fmt.Errorf("Failed to write file contents to tar writer: %w", tarErr)
			}
		}

		return tarErr
	})
	return err
}
