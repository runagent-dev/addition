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
	err = scanAccountFile(a.groupFile, groupFormat, func(f []string) error {
		if !sameGID(f[groupFormat.gid], gid) {
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
	return scanAccountFile(a.passwdFile, passwdFormat, func(f []string) error {
		if sameGID(f[passwdFormat.gid], gid) && f[0] != u.Username {
			return fmt.Errorf("%s has it as their primary group", f[0])
		}
		return nil
	})
}

func cannotVerify(err error) error {
	return fmt.Errorf("addition cannot verify it has no other members (%v; accounts known only through NSS cannot be checked)", err)
}

// accountFormat is an account file's field count and the index of its numeric gid field.
type accountFormat struct{ fields, gid int }

var (
	groupFormat  = accountFormat{fields: 4, gid: 2}
	passwdFormat = accountFormat{fields: 7, gid: 3}
)

// sameGID compares numerically, as the kernel does, so a zero-padded "01000" still matches 1000.
func sameGID(field string, gid uint32) bool {
	n, err := strconv.ParseUint(field, 10, 32)
	return err == nil && uint32(n) == gid
}

// scanAccountFile calls fn with the fields of each entry in an /etc/group or /etc/passwd file,
// refusing to vouch for a file with entries it cannot parse unambiguously.
func scanAccountFile(path string, format accountFormat, fn func(fields []string) error) error {
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
		f := strings.Split(line, ":")
		if len(f) != format.fields || !numeric(f[format.gid]) {
			return cannotVerify(fmt.Errorf("%s has an entry addition cannot parse", path))
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	if err := lines.Err(); err != nil {
		return cannotVerify(err)
	}
	return nil
}

func numeric(field string) bool {
	_, err := strconv.ParseUint(field, 10, 32)
	return err == nil
}
