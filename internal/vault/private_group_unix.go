//go:build unix

package vault

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// accountSources is where a user private group's sole membership is looked up; tests swap in fixtures.
type accountSources struct {
	groupFile, passwdFile string
	currentUser           func() (*user.User, error)
	lookupGroupID         func(gid string) (*user.Group, error)
}

var accounts = accountSources{
	groupFile:     "/etc/group",
	passwdFile:    "/etc/passwd",
	currentUser:   user.Current,
	lookupGroupID: user.LookupGroupId,
}

// checkPrivateGroup proves gid is the current user's primary group, named after them, with no other member.
func (a accountSources) checkPrivateGroup(gid uint32) error {
	id := strconv.FormatUint(uint64(gid), 10)
	u, err := a.currentUser()
	if err != nil {
		return cannotVerify(err)
	}
	if u.Gid != id {
		return errors.New("it is not your primary group")
	}
	g, err := a.lookupGroupID(id)
	if err != nil {
		return cannotVerify(err)
	}
	if g.Name != u.Username {
		return fmt.Errorf("it is named %q, not %q", g.Name, u.Username)
	}
	listed := false
	err = scanAccountFile(a.groupFile, func(f []string) error {
		if len(f) < 4 || f[2] != id {
			return nil
		}
		listed = true
		for _, m := range strings.Split(f[3], ",") {
			if m != "" && m != u.Username {
				return fmt.Errorf("%s is a member of it", m)
			}
		}
		return nil
	})
	if err == nil && !listed {
		err = cannotVerify(fmt.Errorf("it is not listed in %s", a.groupFile))
	}
	if err != nil {
		return err
	}
	return scanAccountFile(a.passwdFile, func(f []string) error {
		if len(f) >= 4 && f[3] == id && f[0] != u.Username {
			return fmt.Errorf("%s has it as their primary group", f[0])
		}
		return nil
	})
}

func cannotVerify(err error) error {
	return fmt.Errorf("addn cannot verify it has no other members (%v; accounts known only through NSS cannot be checked)", err)
}

// scanAccountFile calls fn with the colon-separated fields of each entry in an /etc/group or /etc/passwd file.
func scanAccountFile(path string, fn func(fields []string) error) error {
	file, err := os.Open(path)
	if err != nil {
		return cannotVerify(err)
	}
	defer file.Close()
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-"):
			return cannotVerify(fmt.Errorf("%s includes NIS entries", path))
		}
		if err := fn(strings.Split(line, ":")); err != nil {
			return err
		}
	}
	if err := lines.Err(); err != nil {
		return cannotVerify(err)
	}
	return nil
}
