package config

import (
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var ErrAPIKeyNotConfigured = errors.New("환경 설정에서 나라장터 API 키를 먼저 등록하세요")
var ErrInvalidAPIKey = errors.New("공공데이터포털의 Encoding 또는 Decoding 인증키를 입력하세요 (공백 없이 최대 4096바이트)")
var errAPIKeyFile = errors.New("API 키 파일을 사용할 수 없습니다. 저장 경로와 권한을 확인하세요")

// APIKeyStore keeps the secret out of page models and reloads it for each run.
// Path's directory must be provisioned as an owner-only service directory.
type APIKeyStore struct {
	Path     string
	Fallback string
}

func defaultAPIKeyFile() string {
	if runtime.GOOS == "windows" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		} // Explicit G2B_API_KEY_FILE is required to save.
		return filepath.Join(dir, "namo", "secrets", "g2b-api-key")
	}
	return "/var/db/namo/secrets/g2b-api-key"
}

func (c Config) APIKeys() APIKeyStore {
	return APIKeyStore{Path: c.G2BAPIKeyFile, Fallback: c.G2BAPIKey}
}

// NormalizeAPIKey accepts either portal format and returns the decoded key.
// PathUnescape preserves literal '+', unlike QueryUnescape. Decode only once:
// a remaining percent escape (double encoding) fails the character check.
func NormalizeAPIKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 4096 {
		return "", ErrInvalidAPIKey
	}
	if strings.Contains(key, "%") {
		var err error
		key, err = url.PathUnescape(key)
		if err != nil {
			return "", ErrInvalidAPIKey
		}
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("+/=_-.", c)) {
			return "", ErrInvalidAPIKey
		}
	}
	return key, nil
}

func (s APIKeyStore) Read() (string, error) {
	key := strings.TrimSpace(s.Fallback)
	if s.Path != "" {
		if err := s.checkPath(); err != nil {
			return "", err
		}
		file, err := os.Open(s.Path)
		if err == nil {
			defer file.Close()
			info, statErr := file.Stat()
			if statErr != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
				return "", errAPIKeyFile
			}
			data, readErr := io.ReadAll(io.LimitReader(file, 4097))
			if readErr != nil || len(data) > 4096 {
				return "", errAPIKeyFile
			}
			key, err := NormalizeAPIKey(string(data))
			if err != nil {
				return "", errAPIKeyFile
			}
			return key, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", errAPIKeyFile
		}
	}
	if key == "" {
		return "", ErrAPIKeyNotConfigured
	}
	return NormalizeAPIKey(key)
}

func (s APIKeyStore) Save(key string) error {
	key, err := NormalizeAPIKey(key)
	if err != nil {
		return err
	}
	if err := s.checkPath(); err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errAPIKeyFile
	}
	file, err := os.CreateTemp(dir, ".g2b-key-*")
	if err != nil {
		return errAPIKeyFile
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := io.WriteString(file, key); err != nil {
		return errAPIKeyFile
	}
	if err := file.Sync(); err != nil {
		return errAPIKeyFile
	}
	if err := file.Close(); err != nil {
		return errAPIKeyFile
	}
	if err := os.Rename(file.Name(), s.Path); err != nil {
		return errAPIKeyFile
	}
	return nil
}

func (s APIKeyStore) checkPath() error {
	if !filepath.IsAbs(s.Path) || filepath.Clean(s.Path) != s.Path || filepath.Dir(s.Path) == s.Path {
		return errAPIKeyFile
	}
	for p := s.Path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errAPIKeyFile
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errAPIKeyFile
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
