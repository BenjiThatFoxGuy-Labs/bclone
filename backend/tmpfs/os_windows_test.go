//go:build windows

package tmpfs

import (
	"errors"
	"os"
)

func osMkdirAll(string, os.FileMode) error {
	return errors.New("unsupported")
}

func osSymlink(string, string) error {
	return errors.New("unsupported")
}

func osReadDir(string) ([]os.DirEntry, error) {
	return nil, errors.New("unsupported")
}

func osWriteFile(string, []byte, os.FileMode) error {
	return errors.New("unsupported")
}
