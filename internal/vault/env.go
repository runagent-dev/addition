package vault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// ErrRelativePath rejects a relative data-dir base, which would make the vault depend on the working directory.
var ErrRelativePath = errors.New("the data dir must be an absolute path")

// ResolveHome returns the absolute data dir: $ADDN_HOME, else the platform's user data dir.
func ResolveHome(env Env) (string, error) {
	env = env.withDefaults()
	if home := env.getenv("ADDN_HOME"); home != "" {
		return absoluteBase("ADDN_HOME", home)
	}
	switch env.GOOS {
	case "windows":
		base := env.getenv("LOCALAPPDATA")
		if base == "" {
			return "", errors.New("cannot locate the data dir: set ADDN_HOME or LOCALAPPDATA")
		}
		return joinBase("LOCALAPPDATA", base, appDir)
	case "darwin":
		home, err := userHome(env)
		if err != nil {
			return "", err
		}
		return joinBase("HOME", home, "Library", "Application Support", appDir)
	default:
		if xdg := env.getenv("XDG_DATA_HOME"); xdg != "" {
			return joinBase("XDG_DATA_HOME", xdg, appDir)
		}
		home, err := userHome(env)
		if err != nil {
			return "", err
		}
		return joinBase("HOME", home, ".local", "share", appDir)
	}
}

func joinBase(name, base string, elem ...string) (string, error) {
	if _, err := absoluteBase(name, base); err != nil {
		return "", err
	}
	return filepath.Join(append([]string{base}, elem...)...), nil
}

func absoluteBase(name, base string) (string, error) {
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("%w: %s is %q; set it to an absolute path", ErrRelativePath, name, base)
	}
	return filepath.Clean(base), nil
}

// checkInsideProfile keeps a Windows home under %USERPROFILE%, whose ACLs are the only protection in v0.1.
func checkInsideProfile(env Env, home string) error {
	if env.GOOS != "windows" {
		return nil
	}
	profile := env.getenv("USERPROFILE")
	if !filepath.IsAbs(profile) {
		return fmt.Errorf("%w: USERPROFILE is not an absolute path, so %s cannot be checked", ErrInsecureHome, home)
	}
	resolvedProfile, err := filepath.EvalSymlinks(profile)
	if err != nil {
		resolvedProfile = profile
	}
	if !isWithin(profile, home) && !isWithin(resolvedProfile, home) {
		return fmt.Errorf("%w: %s is outside USERPROFILE (%s); on Windows ADDN_HOME must be inside your profile", ErrInsecureHome, home, profile)
	}
	return nil
}

func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func userHome(env Env) (string, error) {
	home := env.getenv("HOME")
	if home == "" {
		return "", errors.New("cannot locate the data dir: set ADDN_HOME or HOME")
	}
	return home, nil
}
