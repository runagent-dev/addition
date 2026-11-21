package vault_test

import (
	"errors"
	"os"
)

func writeEmptyPrivateFile(path string) error { return os.WriteFile(path, nil, 0o600) }

func removeFile(path string) { _ = os.Remove(path) }

func isErr(err, target error) bool { return errors.Is(err, target) }
