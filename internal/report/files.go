package report

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type FileResult struct {
	RelativePath string
	SHA256       string
}

type FileStore struct {
	root *os.Root
}

const fileChunkSize = 64 << 10

func OpenFileStore(name string) (*FileStore, error) {
	if name == "" || !filepath.IsAbs(name) || hasDotDotComponent(name) {
		return nil, fmt.Errorf("invalid report root %q", name)
	}
	name = filepath.Clean(name)
	if filepath.Dir(name) == name {
		return nil, fmt.Errorf("report root must not be a filesystem root: %q", name)
	}
	info, err := os.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("inspect report root %q: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("report root must be a directory and not a symlink: %q", name)
	}

	root, err := os.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open report root %q: %w", name, err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("inspect opened report root %q: %w", name, err)
	}
	currentInfo, err := os.Lstat(name)
	if err != nil || currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, currentInfo) {
		root.Close()
		if err != nil {
			return nil, fmt.Errorf("reinspect report root %q: %w", name, err)
		}
		return nil, fmt.Errorf("report root changed while opening: %q", name)
	}
	return &FileStore{root: root}, nil
}

func (s *FileStore) Write(ctx context.Context, relativePath string, body []byte) (FileResult, error) {
	path, err := validateRelativePath(relativePath)
	if err != nil {
		return FileResult{}, err
	}
	hash, err := sha256HexContext(ctx, body)
	if err != nil {
		return FileResult{}, fmt.Errorf("write report %q: %w", path, err)
	}
	result := FileResult{RelativePath: path, SHA256: hash}

	existingHash, exists, err := s.existingHash(ctx, path)
	if err != nil {
		return FileResult{}, fmt.Errorf("inspect report %q: %w", path, err)
	}
	if exists {
		if err := ctx.Err(); err != nil {
			return FileResult{}, fmt.Errorf("write report %q: %w", path, err)
		}
		if existingHash == result.SHA256 {
			return result, nil
		}
		return FileResult{}, fmt.Errorf("report %q already exists with different content", path)
	}

	parent := filepath.Dir(path)
	if err := s.createParents(parent); err != nil {
		return FileResult{}, fmt.Errorf("create report parent %q: %w", parent, err)
	}
	if err := ctx.Err(); err != nil {
		return FileResult{}, fmt.Errorf("write report %q: %w", path, err)
	}

	temp, tempPath, err := s.createTemp(parent)
	if err != nil {
		return FileResult{}, fmt.Errorf("create report temporary file: %w", err)
	}
	closed := false
	removeTemp := true
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		if removeTemp {
			_ = s.root.Remove(tempPath)
		}
	}()

	if err := writeAllContext(ctx, temp, body); err != nil {
		return FileResult{}, fmt.Errorf("write report %q: %w", path, err)
	}
	if err := temp.Sync(); err != nil {
		return FileResult{}, fmt.Errorf("sync report %q: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return FileResult{}, fmt.Errorf("write report %q: %w", path, err)
	}
	if err := temp.Chmod(0o640); err != nil {
		return FileResult{}, fmt.Errorf("chmod report %q: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		closed = true
		return FileResult{}, fmt.Errorf("close report %q: %w", path, err)
	}
	closed = true
	if err := ctx.Err(); err != nil {
		return FileResult{}, fmt.Errorf("write report %q: %w", path, err)
	}

	if err := s.root.Link(tempPath, path); err != nil {
		existingHash, exists, inspectErr := s.existingHash(ctx, path)
		if inspectErr == nil && exists && existingHash == result.SHA256 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return FileResult{}, fmt.Errorf("publish report %q: %w", path, ctxErr)
			}
			return result, nil
		}
		if inspectErr != nil {
			return FileResult{}, fmt.Errorf("publish report %q (%v), inspect collision: %w", path, err, inspectErr)
		}
		if exists {
			return FileResult{}, fmt.Errorf("report %q already exists with different content", path)
		}
		return FileResult{}, fmt.Errorf("publish report %q: %w", path, err)
	}
	if err := s.root.Remove(tempPath); err != nil {
		return FileResult{}, fmt.Errorf("remove published report temporary file: %w", err)
	}
	removeTemp = false
	s.syncParent(parent)
	return result, nil
}

func (s *FileStore) Open(relativePath string) (*os.File, os.FileInfo, error) {
	path, err := validateRelativePath(relativePath)
	if err != nil {
		return nil, nil, err
	}
	file, err := s.root.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open report %q: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("stat report %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, fmt.Errorf("report %q is not a regular file", path)
	}
	return file, info, nil
}

func (s *FileStore) Close() error {
	return s.root.Close()
}

// Remove deletes one regular file, never a directory or symlink. Missing files
// are successful so a deletion can be retried after a database commit failure.
func (s *FileStore) Remove(ctx context.Context, relativePath string) error {
	name, err := validateRelativePath(relativePath)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := s.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect report for deletion: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("only regular report files can be deleted")
	}
	if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete report file: %w", err)
	}
	s.syncParent(filepath.Dir(name))
	return nil
}

func (s *FileStore) existingHash(ctx context.Context, path string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	file, _, err := s.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, ctxErr
		}
		return "", false, nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, ctxErr
		}
		return "", false, err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, fileChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", false, ctxErr
			}
			return "", false, readErr
		}
		if read == 0 {
			return "", false, io.ErrNoProgress
		}
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	return hex.EncodeToString(hash.Sum(nil)), true, nil
}

func (s *FileStore) createParents(parent string) error {
	if parent == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(parent, string(os.PathSeparator)) {
		current = filepath.Join(current, component)
		err := s.root.Mkdir(current, 0o750)
		if err == nil {
			if err := s.root.Chmod(current, 0o750); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, statErr := s.root.Stat(current)
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() {
			return fmt.Errorf("%q is not a directory", current)
		}
	}
	return nil
}

func (s *FileStore) createTemp(parent string) (*os.File, string, error) {
	for range 100 {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return nil, "", err
		}
		path := filepath.Join(parent, ".report-"+hex.EncodeToString(random)+".tmp")
		file, err := s.root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, path, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("could not allocate a unique temporary file")
}

func (s *FileStore) syncParent(parent string) {
	dir, err := s.root.Open(parent)
	if err != nil {
		return
	}
	_ = dir.Sync()
	_ = dir.Close()
}

func validateRelativePath(path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 || hasDotDotComponent(path) || !filepath.IsLocal(path) || os.IsPathSeparator(path[len(path)-1]) {
		return "", fmt.Errorf("invalid report path %q", path)
	}
	path = filepath.Clean(path)
	if path == "." {
		return "", fmt.Errorf("invalid report path %q", path)
	}
	return path, nil
}

func hasDotDotComponent(path string) bool {
	start := 0
	for index := 0; index <= len(path); index++ {
		if index == len(path) || os.IsPathSeparator(path[index]) {
			if path[start:index] == ".." {
				return true
			}
			start = index + 1
		}
	}
	return false
}

func writeAllContext(ctx context.Context, file *os.File, body []byte) error {
	for offset := 0; offset < len(body); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(offset+fileChunkSize, len(body))
		written, err := file.Write(body[offset:end])
		offset += written
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return ctx.Err()
}

func sha256HexContext(ctx context.Context, body []byte) (string, error) {
	hash := sha256.New()
	for offset := 0; offset < len(body); {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		end := min(offset+fileChunkSize, len(body))
		_, _ = hash.Write(body[offset:end])
		offset = end
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
