package oauth

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

func readState(dir string) ([]byte, error) {
	path := filepath.Join(dir, "state.json")
	info, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 80<<20 {
		return nil, errors.New("OAuth state must be a private regular file under 80 MiB")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 80<<20))
}
func syncDirectory(dir string) error {
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
