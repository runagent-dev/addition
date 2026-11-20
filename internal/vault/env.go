package vault

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Env is the slice of the process environment the vault depends on, injectable in tests.
type Env struct {
	LookupEnv func(string) (string, bool)
	GOOS      string
	Now       func() time.Time
}

// OSEnv is the real process environment.
func OSEnv() Env {
	return Env{LookupEnv: os.LookupEnv, GOOS: runtime.GOOS, Now: time.Now}
}

func (e Env) getenv(key string) string {
	v, _ := e.LookupEnv(key)
	return v
}

func (e Env) withDefaults() Env {
	real := OSEnv()
	if e.LookupEnv == nil {
		e.LookupEnv = real.LookupEnv
	}
	if e.GOOS == "" {
		e.GOOS = real.GOOS
	}
	if e.Now == nil {
		e.Now = real.Now
	}
	return e
}

const appDir = "addition"

// ResolveHome returns the absolute data dir: $ADDN_HOME, else the platform's user data dir.
func ResolveHome(env Env) (string, error) {
	env = env.withDefaults()
	if home := env.getenv("ADDN_HOME"); home != "" {
		return filepath.Abs(home)
	}
	switch env.GOOS {
	case "windows":
		base := env.getenv("LOCALAPPDATA")
		if base == "" {
			return "", errors.New("cannot locate the data dir: set ADDN_HOME or LOCALAPPDATA")
		}
		return filepath.Join(base, appDir), nil
	case "darwin":
		home, err := userHome(env)
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", appDir), nil
	default:
		if xdg := env.getenv("XDG_DATA_HOME"); filepath.IsAbs(xdg) {
			return filepath.Join(xdg, appDir), nil
		}
		home, err := userHome(env)
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", appDir), nil
	}
}

func userHome(env Env) (string, error) {
	home := env.getenv("HOME")
	if home == "" {
		return "", errors.New("cannot locate the data dir: set ADDN_HOME or HOME")
	}
	return home, nil
}
